package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

var (
	// stallTimeout is the longest an HTTP engine's Stream waits for its
	// provider's answer, or for the next bytes of it, before it fails: a
	// stalled stream would otherwise hold the Say, and the exchange's floor,
	// until the exchange ends. Time spent waiting on emit does not count. A
	// var so tests can shorten it.
	stallTimeout = 5 * time.Second
	// retryWait is how long an HTTP engine waits to ask again after its
	// provider refused a text for its rate (429). It asks again once. A var
	// so tests can shorten it.
	retryWait = 2 * time.Second
)

const (
	// maxErrorBody bounds the body read from a refused request.
	maxErrorBody = 64 << 10
	// pcmChunk is the most PCM an engine that streams raw PCM emits at once:
	// 100 ms at 24 kHz mono.
	pcmChunk = 4800
	// warmTimeout bounds a Session's warm-up.
	warmTimeout = 10 * time.Second
)

// httpEngine is what every engine that speaks over HTTP shares: its
// provider's signed-in client, the warm-up of a Session's way to the
// provider, and the guard of each request against a stall.
type httpEngine struct {
	name     string // the engine in its errors: "gemini tts", "openai tts", …
	provider string // who answers, as providererr names it: the provider's name
	p        *providers.Provider
	host     string       // the provider's base URL, which a warm-up asks
	raw      *http.Client // the client the sign-in wraps: the warm-up's
	client   *http.Client // signed in: each request carries p's credential
}

func newHTTPEngine(name string, p *providers.Provider, o *options) httpEngine {
	raw := o.base
	if raw == nil {
		// No total timeout: a stream lasts as long as its speech plays.
		raw = &http.Client{}
	}
	return httpEngine{name: name, provider: p.Name, p: p, host: p.BaseURL(), raw: raw, client: p.HTTPClient(raw)}
}

// connect warms a Session's way to the provider and never fails: in the
// background it gets the provider's credential, which the provider holds
// until it is due, and opens a connection with a request whose answer it
// drops, which the engine's HTTP client keeps in its pool for the first Say.
// A warm-up that fails leaves that Say to sign in and dial as it would have.
// The Conn streams with stream; its Close stops the warm-up.
func (e *httpEngine) connect(stream func(ctx context.Context, text string, emit func(pcm []byte) error) error) Conn {
	ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
	c := &warmConn{stream: stream, cancel: cancel, warmed: make(chan struct{})}
	go func() {
		defer close(c.warmed)
		e.warm(ctx)
	}()
	return c
}

// warm gets the credential and opens a connection to the provider at once.
func (e *httpEngine) warm(ctx context.Context) {
	credential := make(chan struct{})
	go func() {
		defer close(credential)
		_, _ = e.p.Credential(ctx)
	}()
	defer func() { <-credential }()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, e.host+"/", nil)
	if err != nil {
		return
	}
	// The unsigned client: the connection is the point, and it lands in the
	// pool the signed-in client draws from.
	resp, err := e.raw.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

// warmConn is a Session's warmed way to an HTTP engine's provider.
type warmConn struct {
	stream func(ctx context.Context, text string, emit func(pcm []byte) error) error
	cancel context.CancelFunc
	warmed chan struct{} // closed when the warm-up is over
}

// Stream is the engine's Stream; the warm-up only readied the way.
func (c *warmConn) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	return c.stream(ctx, text, emit)
}

// Close stops the warm-up and waits for it.
func (c *warmConn) Close() error {
	c.cancel()
	<-c.warmed
	return nil
}

// post sends body to url and returns the provider's answer once its status
// is 200, asking again once after a 429. A refusal's error keeps the
// provider's status and the code code reads from the body, never the body:
// it can quote the text asked for.
func (e *httpEngine) post(ctx context.Context, url string, body []byte, code func(body []byte) string) (*answer, error) {
	// The sign-in comes first, outside the stall timer, so its failure is
	// reported as the provider's sign-in says it, not as a failed request.
	if _, err := e.p.Credential(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return nil, fmt.Errorf("%s: sign in: %w", e.name, err)
	}
	for asked := 1; ; asked++ {
		a, status, err := e.send(ctx, url, body, code)
		if status != http.StatusTooManyRequests || asked == 2 {
			return a, err
		}
		select {
		case <-time.After(retryWait):
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// send makes one request of post and returns its answer, or its status and
// why it failed.
func (e *httpEngine) send(ctx context.Context, url string, body []byte, code func(body []byte) string) (*answer, int, error) {
	stalled := fmt.Errorf("%s: no stream data for %s", e.name, stallTimeout)
	reqCtx, cancel := context.WithCancelCause(ctx)
	// The stall timer cancels the request; each byte of the answer restarts
	// it, and it waits while emit does.
	a := &answer{e: e, ctx: reqCtx, cancel: cancel, stall: time.AfterFunc(stallTimeout, func() { cancel(stalled) })}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		a.close()
		return nil, 0, fmt.Errorf("%s: create request: %w", e.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		err = a.fail("http request", err)
		a.close()
		return nil, 0, err
	}
	a.body = resp.Body
	if resp.StatusCode != http.StatusOK {
		err := a.refusal(resp.StatusCode, code)
		a.close()
		return nil, resp.StatusCode, err
	}
	return a, resp.StatusCode, nil
}

// answer is a provider's answer of status 200. Each byte read from it
// restarts the stall timer; the time the engine spends handing its audio on
// does not count.
type answer struct {
	e      *httpEngine
	ctx    context.Context // the request's: it ends at a stall, with the stall as its cause
	cancel context.CancelCauseFunc
	stall  *time.Timer
	body   io.ReadCloser
}

func (a *answer) Read(p []byte) (int, error) {
	n, err := a.body.Read(p)
	if n > 0 {
		a.stall.Reset(stallTimeout)
	}
	return n, err
}

// pause stops the stall timer while the engine hands audio on; resume
// starts it again.
func (a *answer) pause()  { a.stall.Stop() }
func (a *answer) resume() { a.stall.Reset(stallTimeout) }

// emit hands pcm to emit, which may wait on the Output for as long as it
// takes: that wait is no stall.
func (a *answer) emit(emit func(pcm []byte) error, pcm []byte) error {
	a.pause()
	if err := emit(pcm); err != nil {
		return err
	}
	a.resume()
	return nil
}

// streamPCM emits the answer, raw PCM, as it arrives. It fails an answer
// that held none.
func (a *answer) streamPCM(emit func(pcm []byte) error) error {
	buf := make([]byte, pcmChunk)
	audio := false
	for {
		n, err := a.Read(buf)
		if n > 0 {
			audio = true
			if err := a.emit(emit, buf[:n]); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return a.fail("read stream", err)
		}
	}
	if !audio {
		return a.noAudio()
	}
	return nil
}

// noAudio is the error of an answer that held no audio.
func (a *answer) noAudio() error { return fmt.Errorf("%s: %w", a.e.name, ErrNoAudio) }

// fail is why the request, or a read of its answer, failed: the stall or
// the caller's cancel when one ended it, else err reduced, since a failure
// net/http reports can quote the answer.
func (a *answer) fail(what string, err error) error {
	if a.ctx.Err() != nil {
		return context.Cause(a.ctx)
	}
	return fmt.Errorf("%s: %s: %w", a.e.name, what, providererr.From(a.e.provider, err))
}

// refusal is the error of an answer of status: the provider's status and
// the code code reads from its body. The body, and the message in it, can
// quote the text asked for, so neither is kept.
func (a *answer) refusal(status int, code func(body []byte) string) error {
	body, err := io.ReadAll(io.LimitReader(a, maxErrorBody))
	if err != nil {
		if a.ctx.Err() != nil {
			return context.Cause(a.ctx)
		}
		return fmt.Errorf("%s: api error (status %d), body unreadable: %w", a.e.name, status, providererr.From(a.e.provider, err))
	}
	return fmt.Errorf("%s: api error: %w", a.e.name, providererr.New(a.e.provider, status, code(body), string(body)))
}

// close ends the request and its stall timer.
func (a *answer) close() {
	a.stall.Stop()
	a.cancel(nil)
	if a.body != nil {
		_ = a.body.Close()
	}
}

// googleCode is the code of a Google API's refusal: error.status, such as
// INVALID_ARGUMENT. An OAuth refusal, whose error is a string, names none.
func googleCode(body []byte) string {
	var refusal struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Error.Status
}
