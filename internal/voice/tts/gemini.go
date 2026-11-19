package tts

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

const (
	// geminiDefaultLocation is Vertex AI's US multi-region, which Google
	// recommends over global for a lower time to first audio when streaming.
	geminiDefaultLocation = "us"
	geminiDefaultModel    = "gemini-3.8-flash-tts"
	geminiDefaultVoice    = "Kore"
	// geminiRate is the only rate Gemini TTS streams: headerless PCM16 mono.
	geminiRate = 24000
	// geminiMaxEvent bounds one server-sent event: far more than the
	// base64 audio of one chunk.
	geminiMaxEvent = 16 << 20
	// geminiMaxErrorBody bounds the error body read from a refused request.
	geminiMaxErrorBody = 64 << 10
)

// GeminiConfig configures the Gemini TTS engine on Vertex AI.
type GeminiConfig struct {
	Project string
	// Location is a Vertex AI location; "us" when empty. "us" and "eu" are
	// multi-regions on their own hosts (aiplatform.us.rep.googleapis.com),
	// "global" is aiplatform.googleapis.com, and a region such as
	// "us-central1" is us-central1-aiplatform.googleapis.com.
	Location string
	Model    string // "gemini-3.8-flash-tts" when empty
	// Voice is a prebuilt voice name, an Extended Voice Library id, or a
	// designed voice's id; "Kore" when empty.
	Voice string
	// Credentials authorise every request with their bearer token.
	Credentials *auth.Credentials
	// BaseURL replaces the location's Vertex AI host, for tests.
	BaseURL string
	// HTTPClient carries the requests; a plain client when nil. It needs no
	// total timeout: a stream lasts as long as its speech plays.
	HTTPClient *http.Client
}

// Gemini synthesises speech with Gemini TTS through Vertex AI's
// streamGenerateContent with alt=sse, which streams headerless PCM16 at 24 kHz
// mono as base64 inline data, one server-sent event at a time. Each event is
// decoded once, straight into the audio it carries.
//
// Gemini is a Connector: a Session warms its way to Vertex AI as it opens, so
// the first Say pays neither the token nor the TLS dial.
type Gemini struct {
	host        string // the Vertex AI host, scheme included
	endpoint    string
	voice       string
	credentials *auth.Credentials
	client      *http.Client
}

// NewGemini returns a Gemini TTS engine for cfg.
func NewGemini(cfg *GeminiConfig) (*Gemini, error) {
	if cfg == nil {
		return nil, errors.New("gemini tts: no config")
	}
	project := strings.TrimSpace(cfg.Project)
	if project == "" {
		return nil, errors.New("gemini tts: a Vertex AI project is required")
	}
	if cfg.Credentials == nil {
		return nil, errors.New("gemini tts: Google credentials are required")
	}
	location := cmp.Or(strings.TrimSpace(cfg.Location), geminiDefaultLocation)
	model := cmp.Or(strings.TrimSpace(cfg.Model), geminiDefaultModel)
	base := strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.BaseURL), vertexHost(location)), "/")
	return &Gemini{
		host: base,
		endpoint: fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
			base, url.PathEscape(project), url.PathEscape(location), url.PathEscape(model)),
		voice:       cmp.Or(strings.TrimSpace(cfg.Voice), geminiDefaultVoice),
		credentials: cfg.Credentials,
		client:      cmp.Or(cfg.HTTPClient, &http.Client{}),
	}, nil
}

// vertexHost is the Vertex AI host of location, as go-genai maps it: the
// global host, a multi-region's own host, or a region's.
func vertexHost(location string) string {
	switch location {
	case "global":
		return "https://aiplatform.googleapis.com"
	case "us", "eu":
		return "https://aiplatform." + location + ".rep.googleapis.com"
	default:
		return "https://" + location + "-aiplatform.googleapis.com"
	}
}

// Name names the engine.
func (*Gemini) Name() string { return "gemini" }

// Format is PCM16 mono at 24 kHz, the only output Gemini TTS streams.
func (*Gemini) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: geminiRate, Channels: 1, Encoding: audio.EncodingPCM16}
}

// geminiWarmTimeout bounds a Session's warm-up.
const geminiWarmTimeout = 10 * time.Second

// Connect warms the Session's way to Vertex AI and never fails: in the
// background it fetches a token, which the credentials cache, and opens a
// connection with a request whose answer it drops, which the engine's HTTP
// client keeps in its pool for the first Say. A warm-up that fails leaves
// that Say to fetch and dial as it would have. Close stops the warm-up.
func (g *Gemini) Connect(context.Context) (Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), geminiWarmTimeout)
	c := &geminiConn{gemini: g, cancel: cancel, warmed: make(chan struct{})}
	go func() {
		defer close(c.warmed)
		g.warm(ctx)
	}()
	return c, nil
}

// warm fetches a token and opens a connection to the Vertex AI host at once.
func (g *Gemini) warm(ctx context.Context) {
	token := make(chan struct{})
	go func() {
		defer close(token)
		_, _ = g.credentials.Token(ctx)
	}()
	defer func() { <-token }()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, g.host+"/", nil)
	if err != nil {
		return
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, geminiMaxErrorBody))
	_ = resp.Body.Close()
}

// geminiConn is a Session's warmed way to Vertex AI.
type geminiConn struct {
	gemini *Gemini
	cancel context.CancelFunc
	warmed chan struct{} // closed when the warm-up is over
}

// Stream is Gemini's Stream; the warm-up only readied the way.
func (c *geminiConn) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	return c.gemini.Stream(ctx, text, emit)
}

// Close stops the warm-up and waits for it.
func (c *geminiConn) Close() error {
	c.cancel()
	<-c.warmed
	return nil
}

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	SpeechConfig       geminiSpeechConfig `json:"speechConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

// geminiVoiceConfig names the voice in voiceConfig.voice, the field of the
// Gemini 3.8 TTS models, which takes a prebuilt, library or designed voice.
type geminiVoiceConfig struct {
	Voice string `json:"voice"`
}

// geminiEvent is one server-sent event of the stream: audio, or an error.
// encoding/json decodes the base64 data into its bytes.
type geminiEvent struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				InlineData struct {
					Data []byte `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *geminiError `json:"error"`
}

type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// geminiStallTimeout is the longest a Stream waits for Vertex AI's answer,
// or for the next bytes of it, before it fails: a stalled stream would
// otherwise hold the Say, and the exchange's floor, until the exchange ends.
// Time spent waiting on emit does not count. A var so tests can shorten it.
var geminiStallTimeout = 5 * time.Second

// Stream synthesises text and emits each event's audio as it arrives. It
// fails when no answer, or no byte of it, comes for geminiStallTimeout.
func (g *Gemini) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	body, err := json.Marshal(geminiRequest{
		Contents: []geminiContent{{Role: "user", Parts: []geminiPart{{Text: text}}}},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig:       geminiSpeechConfig{VoiceConfig: geminiVoiceConfig{Voice: g.voice}},
		},
	})
	if err != nil {
		return fmt.Errorf("gemini tts: marshal request: %w", err)
	}
	token, err := g.credentials.Token(ctx)
	if err != nil {
		return fmt.Errorf("gemini tts: get token: %w", err)
	}
	// The stall timer cancels the request; each byte that arrives restarts
	// it, and it waits while emit does.
	stalled := fmt.Errorf("gemini tts: no stream data for %s", geminiStallTimeout)
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(geminiStallTimeout, func() { cancel(stalled) })
	defer stall.Stop()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gemini tts: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token.Value)

	resp, err := g.client.Do(req)
	if err != nil {
		if reqCtx.Err() != nil {
			return context.Cause(reqCtx)
		}
		return fmt.Errorf("gemini tts: http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return geminiStatusError(resp)
	}

	events := bufio.NewScanner(stallReader{r: resp.Body, stall: stall})
	events.Buffer(make([]byte, 0, 64<<10), geminiMaxEvent)
	var data []byte
	for {
		more := events.Scan()
		line := events.Bytes()
		if more && len(line) > 0 {
			// An event's data lines; any other field (event, id, a
			// comment) carries nothing for us.
			if field, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimPrefix(field, []byte(" "))...)
			}
			continue
		}
		// A blank line, or the end of the stream, ends an event.
		if len(data) > 0 {
			stall.Stop()
			if err := g.emitEvent(data, emit); err != nil {
				return err
			}
			stall.Reset(geminiStallTimeout)
			data = data[:0]
		}
		if !more {
			break
		}
	}
	if err := events.Err(); err != nil {
		if reqCtx.Err() != nil {
			return context.Cause(reqCtx)
		}
		return fmt.Errorf("gemini tts: read stream: %w", err)
	}
	return nil
}

// stallReader restarts a stream's stall timer with each byte it reads.
type stallReader struct {
	r     io.Reader
	stall *time.Timer
}

func (s stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.stall.Reset(geminiStallTimeout)
	}
	return n, err
}

// emitEvent decodes one event and emits its audio.
func (*Gemini) emitEvent(data []byte, emit func(pcm []byte) error) error {
	var event geminiEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("gemini tts: decode event: %w", err)
	}
	if e := event.Error; e != nil {
		return fmt.Errorf("gemini tts: stream error %d %s: %s", e.Code, e.Status, e.Message)
	}
	for _, candidate := range event.Candidates {
		for _, part := range candidate.Content.Parts {
			if len(part.InlineData.Data) == 0 {
				continue
			}
			if err := emit(part.InlineData.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

// geminiStatusError is the error of a refused request: Vertex AI's message
// when the body carries one, else the body.
func geminiStatusError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, geminiMaxErrorBody))
	if err != nil {
		return fmt.Errorf("gemini tts: api error (status %d), body unreadable: %w", resp.StatusCode, err)
	}
	var refusal struct {
		Error geminiError `json:"error"`
	}
	if json.Unmarshal(body, &refusal) == nil && refusal.Error.Message != "" {
		return fmt.Errorf("gemini tts: api error (status %d): %s", resp.StatusCode, refusal.Error.Message)
	}
	return fmt.Errorf("gemini tts: api error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
