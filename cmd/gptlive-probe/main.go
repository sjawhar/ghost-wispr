// Command gptlive-probe checks voice B's route to OpenAI from the office
// unit, once the unit's Google identity is mapped to an OpenAI service
// account. First a control: a Google ID token for another audience, which
// OpenAI must refuse as an invalid subject token, so a refusal of the real
// one reads as a missing mapping and not a bad token. Then it exchanges the
// unit's Google ID token for an OpenAI access token, opens one GPT-Live
// session, sends one text turn, and reports the speech that comes back. It
// sends GPT-Live silence, never the room, and prints no token.
//
// Run it on the unit as the ghost-wispr user, who owns the credential's key:
//
//	sudo -u ghost-wispr gptlive-probe --identity-provider-id idp_… --service-account-id user-…
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// controlAudience is the control's audience: one OpenAI's identity
// provider does not accept.
const controlAudience = "https://example.invalid/gptlive-probe-control"

// probeSettle is how long the probe listens after Say returns. Speech in
// that time means Say returned before GPT-Live had finished, and the
// Supervisor, which ends an exchange when Say returns, would cut it.
const probeSettle = 2 * time.Second

// probeConfig is what one probe run needs.
type probeConfig struct {
	TokenURL           string // empty means gptlive.TokenEndpoint
	LiveURL            string // empty means gptlive.DefaultURL
	IdentityProviderID string
	ServiceAccountID   string
	// Subject mints the unit's Google ID token for an audience.
	Subject  func(ctx context.Context, audience string) (string, error)
	Text     string        // the text turn GPT-Live answers
	Voice    string        // GPT-Live's voice; empty means its default
	MinAudio time.Duration // the speech that passes the probe
	WAV      string        // where to write the speech; empty writes none
}

func main() { os.Exit(run()) }

// run parses the flags, runs the probe and returns the exit code.
func run() int {
	credentials := flag.String("credentials", "/opt/ghost-wispr/google-credentials.json", "the unit's Google credential file")
	provider := flag.String("identity-provider-id", "", "OpenAI workload identity provider id (idp_…)")
	account := flag.String("service-account-id", "", "OpenAI service account id the provider maps the unit to (user-…)")
	text := flag.String("text", "Say in one short sentence that the Ghost Wispr probe reached you.", "the text turn GPT-Live answers")
	voiceName := flag.String("voice", "", "GPT-Live voice, such as marin")
	wav := flag.String("wav", "", "write GPT-Live's speech to this WAV file")
	timeout := flag.Duration("timeout", 60*time.Second, "give up after this long")
	flag.Parse()
	clientCertificatesOff()
	if *provider == "" || *account == "" {
		printf(os.Stderr, "gptlive-probe needs --identity-provider-id and --service-account-id\n")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	err := probe(ctx, &probeConfig{
		IdentityProviderID: *provider,
		ServiceAccountID:   *account,
		Subject: func(ctx context.Context, audience string) (string, error) {
			return googleauth.IDToken(ctx, *credentials, audience)
		},
		Text:     *text,
		Voice:    *voiceName,
		MinAudio: time.Second,
		WAV:      *wav,
	}, os.Stdout)
	if err != nil {
		printf(os.Stdout, "result: FAIL: %v\n", err)
		return 1
	}
	return 0
}

// clientCertificateEnv turns client certificates for Google's APIs on or
// off. The unit's service sets it false for its ID-token paths, and the
// probe does the same unless its caller set it.
const clientCertificateEnv = "GOOGLE_API_USE_CLIENT_CERTIFICATE"

func clientCertificatesOff() {
	if os.Getenv(clientCertificateEnv) == "" {
		_ = os.Setenv(clientCertificateEnv, "false")
	}
}

// printf writes one report line; a report that cannot be written changes
// nothing the probe does.
func printf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// probe runs one check and writes its report to out.
func probe(ctx context.Context, cfg *probeConfig, out io.Writer) error {
	if err := control(ctx, cfg); err != nil {
		return err
	}
	printf(out, "control: a token for another audience was refused (invalid_subject_token)\n")

	tokens, err := gptlive.NewTokenSource(gptlive.TokenConfig{
		Endpoint:           cfg.TokenURL,
		IdentityProviderID: cfg.IdentityProviderID,
		ServiceAccountID:   cfg.ServiceAccountID,
		Subject:            cfg.Subject,
	})
	if err != nil {
		return err
	}
	begin := time.Now()
	if _, err := tokens.Token(ctx); err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	printf(out, "token exchange: ok (%v)\n", time.Since(begin).Round(time.Millisecond))

	v, err := gptlive.New(gptlive.Config{URL: cfg.LiveURL, Voice: cfg.Voice, Token: tokens.Token})
	if err != nil {
		return err
	}
	tap := make(chan audio.Frame, 4)
	go silence(ctx, tap)
	rec := &recording{}
	begin = time.Now()
	s, err := v.Open(ctx, &voice.Exchange{SessionID: "gptlive-probe", WakeAt: begin, Tap: tap, Out: rec, Tools: probeTools{}})
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer func() { _ = s.Close() }()
	printf(out, "session: started (%v)\n", time.Since(begin).Round(time.Millisecond))
	seen := watch(s.Events())

	// Say returns once GPT-Live has spoken the turn's answer and it played.
	turn := time.Now()
	if err := s.Say(ctx, cfg.Text, voice.Ack); err != nil {
		return fmt.Errorf("text turn: %w", err)
	}
	said := rec.duration()
	select {
	case <-time.After(probeSettle):
	case <-ctx.Done():
		return ctx.Err()
	}
	if more := rec.duration() - said; more > 0 {
		return fmt.Errorf("the answer was cut: GPT-Live spoke %v more after Say returned, and an exchange ends when Say returns", more)
	}
	if err := seen.failure(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if first, ok := rec.firstAt(); ok {
		printf(out, "first audio: %v after the text turn\n", first.Sub(turn).Round(time.Millisecond))
	}
	printf(out, "audio: %.2f s at %d Hz\n", rec.duration().Seconds(), gptlive.SampleRate)
	printf(out, "words: %s\n", seen.words())
	if rec.duration() < cfg.MinAudio {
		return fmt.Errorf("GPT-Live spoke %v, want %v or more", rec.duration(), cfg.MinAudio)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if cfg.WAV != "" {
		if err := os.WriteFile(cfg.WAV, rec.wav(), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", cfg.WAV, err)
		}
		printf(out, "wav: %s\n", cfg.WAV)
	}
	printf(out, "result: PASS\n")
	return nil
}

// control exchanges a Google ID token for another audience, which OpenAI
// must refuse as an invalid subject token: then a refusal of the real token
// means the mapping, not the token.
func control(ctx context.Context, cfg *probeConfig) error {
	tokens, err := gptlive.NewTokenSource(gptlive.TokenConfig{
		Endpoint:           cfg.TokenURL,
		Audience:           controlAudience,
		IdentityProviderID: cfg.IdentityProviderID,
		ServiceAccountID:   cfg.ServiceAccountID,
		Subject:            cfg.Subject,
	})
	if err != nil {
		return err
	}
	_, err = tokens.Token(ctx)
	switch {
	case err == nil:
		return errors.New("control: OpenAI took a token for another audience, so this run cannot tell a refused token from a missing mapping")
	case !strings.Contains(err.Error(), "invalid_subject_token"):
		return fmt.Errorf("control: a token for another audience was refused with %w, want invalid_subject_token", err)
	}
	return nil
}

// events is what the probe saw on a session's Events, read as it comes so
// that nothing waits on the probe.
type events struct {
	mu    sync.Mutex
	text  strings.Builder
	first error
}

func watch(c <-chan voice.Event) *events {
	e := &events{}
	go func() {
		for ev := range c {
			e.mu.Lock()
			switch ev.Kind {
			case voice.EventAgentText:
				e.text.WriteString(ev.Text)
			case voice.EventErr:
				if e.first == nil {
					e.first = ev.Err
				}
			}
			e.mu.Unlock()
		}
	}()
	return e
}

func (e *events) words() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.TrimSpace(e.text.String())
}

func (e *events) failure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.first
}

// silence feeds the session 250 ms of silence every 250 ms, as a quiet room
// would: GPT-Live's timeline runs on its input audio.
func silence(ctx context.Context, tap chan<- audio.Frame) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			select {
			case tap <- audio.Frame{PCM: make([]int16, 4000), Rate: 16000, Channels: 1}:
			default:
			}
		}
	}
}

// probeTools answers a delegation, should GPT-Live make one.
type probeTools struct{}

func (probeTools) Run(_ context.Context, c voice.ToolCall) (json.RawMessage, error) {
	if c.Name != voice.AnswerTool {
		return nil, fmt.Errorf("the probe has no tool %q", c.Name)
	}
	return json.Marshal(voice.AnswerResult{Say: "The probe's backend answered, so delegation works."})
}

// recording is the speaker output for the probe: it keeps the speech.
type recording struct {
	mu    sync.Mutex
	pcm   []byte
	first time.Time
}

func (r *recording) Begin() voice.Playback { return r }

func (r *recording) Write(_ context.Context, frame []byte, format audio.AudioFormat) error {
	if format.SampleRate != gptlive.SampleRate || format.Channels != 1 {
		return fmt.Errorf("speech at %d Hz and %d channels, want %d Hz mono", format.SampleRate, format.Channels, gptlive.SampleRate)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.first.IsZero() {
		r.first = time.Now()
	}
	r.pcm = append(r.pcm, frame...)
	return nil
}

func (r *recording) Drain(context.Context) error { return nil }

func (r *recording) duration() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Duration(len(r.pcm)/2) * time.Second / gptlive.SampleRate
}

func (r *recording) firstAt() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first, !r.first.IsZero()
}

// wav is the speech as a WAV file: mono PCM16 at gptlive.SampleRate.
func (r *recording) wav() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := make([]byte, 44, 44+len(r.pcm))
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(r.pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], gptlive.SampleRate)
	binary.LittleEndian.PutUint32(h[28:], gptlive.SampleRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(r.pcm)))
	return append(h, r.pcm...)
}
