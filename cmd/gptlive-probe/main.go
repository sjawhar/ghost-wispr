// Command gptlive-probe checks voice B's route to OpenAI from the office
// unit, once the unit's Google identity is mapped to an OpenAI service
// account: it exchanges the unit's Google ID token for an OpenAI access
// token, opens one GPT-Live session, sends one text turn, and reports the
// speech that comes back. It sends GPT-Live silence, never the room, and
// prints no token.
//
//	gptlive-probe --identity-provider-id idp_… --service-account-id user-…
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
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

func main() {
	credentials := flag.String("credentials", "/opt/ghost-wispr/google-credentials.json", "the unit's Google credential file")
	provider := flag.String("identity-provider-id", "", "OpenAI workload identity provider id (idp_…)")
	account := flag.String("service-account-id", "", "OpenAI service account id the provider maps the unit to (user-…)")
	text := flag.String("text", "Say in one short sentence that the Ghost Wispr probe reached you.", "the text turn GPT-Live answers")
	voiceName := flag.String("voice", "", "GPT-Live voice, such as marin")
	wav := flag.String("wav", "", "write GPT-Live's speech to this WAV file")
	timeout := flag.Duration("timeout", 60*time.Second, "give up after this long")
	flag.Parse()
	if *provider == "" || *account == "" {
		fmt.Fprintln(os.Stderr, "gptlive-probe needs --identity-provider-id and --service-account-id")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	err := probe(ctx, probeConfig{
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
		fmt.Printf("result: FAIL: %v\n", err)
		os.Exit(1)
	}
}

// probe runs one check and writes its report to out.
func probe(ctx context.Context, cfg probeConfig, out io.Writer) error {
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
	fmt.Fprintf(out, "token exchange: ok (%v)\n", time.Since(begin).Round(time.Millisecond))

	v, err := gptlive.New(gptlive.Config{URL: cfg.LiveURL, Voice: cfg.Voice, Token: tokens.Token})
	if err != nil {
		return err
	}
	tap := make(chan audio.Frame, 4)
	go silence(ctx, tap)
	rec := &recording{}
	begin = time.Now()
	s, err := v.Open(ctx, voice.Exchange{SessionID: "gptlive-probe", WakeAt: begin, Tap: tap, Out: rec, Tools: probeTools{}})
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer func() { _ = s.Close() }()
	fmt.Fprintf(out, "session: started (%v)\n", time.Since(begin).Round(time.Millisecond))

	turn := time.Now()
	if err := s.Say(ctx, cfg.Text, voice.Ack); err != nil {
		return fmt.Errorf("text turn: %w", err)
	}
	var words strings.Builder
	heard := false
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !heard {
		select {
		case ev := <-s.Events():
			switch ev.Kind {
			case voice.EventAgentText:
				words.WriteString(ev.Text)
			case voice.EventErr:
				return fmt.Errorf("session: %w", ev.Err)
			case voice.EventSpeechEnded:
				heard = rec.duration() >= cfg.MinAudio
			}
		case <-tick.C:
		case <-ctx.Done():
			return fmt.Errorf("%v of speech before the deadline, want %v: %w", rec.duration(), cfg.MinAudio, ctx.Err())
		}
	}
	if first, ok := rec.firstAt(); ok {
		fmt.Fprintf(out, "first audio: %v after the text turn\n", first.Sub(turn).Round(time.Millisecond))
	}
	fmt.Fprintf(out, "audio: %.2f s at %d Hz\n", rec.duration().Seconds(), gptlive.SampleRate)
	fmt.Fprintf(out, "words: %s\n", strings.TrimSpace(words.String()))
	if err := s.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if cfg.WAV != "" {
		if err := os.WriteFile(cfg.WAV, rec.wav(), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", cfg.WAV, err)
		}
		fmt.Fprintf(out, "wav: %s\n", cfg.WAV)
	}
	fmt.Fprintln(out, "result: PASS")
	return nil
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
