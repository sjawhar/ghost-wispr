package tts

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptrace"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// liveSentences are ten answers of the kind voice A speaks in a meeting.
var liveSentences = []string{
	"Twelve issues are open, and three of them are blocked.",
	"The build on main passed twenty minutes ago.",
	"Nobody holds that issue right now; it was last updated yesterday.",
	"I asked the agent working on it, and I will tell you when it answers.",
	"The release went out this morning at nine fifteen.",
	"That pull request has two approvals and one open comment.",
	"The deploy is waiting for a review from the platform team.",
	"Three tests failed in the last run, all in the audio package.",
	"The meeting notes from Tuesday mention that decision.",
	"I could not find an issue with that name.",
}

// firstFrameOutput is an Output that plays nothing and keeps time: armed, it
// sends the time of the next frame written.
type firstFrameOutput struct {
	armed atomic.Bool
	first chan time.Time
}

func (o *firstFrameOutput) Begin() voice.Playback { return firstFramePlayback{o} }

type firstFramePlayback struct{ o *firstFrameOutput }

func (p firstFramePlayback) Write(context.Context, []byte, audio.AudioFormat) error {
	if p.o.armed.CompareAndSwap(true, false) {
		p.o.first <- time.Now()
	}
	return nil
}

func (firstFramePlayback) Drain(context.Context) error { return nil }

// liveWakeToTurn is how long before the asker's turn is final the Supervisor
// opens the voice: it opens at the wake phrase, 1 to 2 s before.
const liveWakeToTurn = time.Second

// streamConns notes whether each streaming request went out on a connection
// that was already open, as the Session's warm-up leaves one.
type streamConns struct {
	base   http.RoundTripper
	reused atomic.Bool
	seen   atomic.Bool
}

func (c *streamConns) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				c.reused.Store(info.Reused)
				c.seen.Store(true)
			},
		}))
	}
	return c.base.RoundTrip(r)
}

// describe says how the last streaming request got its connection.
func (c *streamConns) describe() string {
	switch {
	case c == nil:
		return "a socket of its own"
	case !c.seen.Load():
		return "no stream request seen"
	case c.reused.Load():
		return "a reused connection"
	default:
		return "a new connection"
	}
}

// liveEngine builds the engine GHOST_WISPR_LIVE_TTS names, with nothing
// shared with an earlier one: Gemini gets credentials and a connection pool
// of its own, so its first request pays the token and the dial as the first
// exchange after a long quiet does, and the pool is traced (nil for Aura).
// It skips the test when no engine is named.
func liveEngine(t *testing.T, logCredentials bool) (Engine, *streamConns) {
	t.Helper()
	model, voice := os.Getenv("GHOST_WISPR_LIVE_TTS_MODEL"), os.Getenv("GHOST_WISPR_LIVE_TTS_VOICE")
	switch name := os.Getenv("GHOST_WISPR_LIVE_TTS"); name {
	case "":
		t.Skip("set GHOST_WISPR_LIVE_TTS=aura or gemini to measure time to first audio against the real service")
	case "aura":
		const keyEnv = "GHOST_WISPR_DEEPGRAM_API_KEY"
		p := liveProvider(t, &config.Provider{API: config.APIDeepgram, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}}, nil)
		a, err := NewEngine(p, cmp.Or(model, "aura-2-harmonia-en"), voice)
		if err != nil {
			t.Fatal(err)
		}
		return a, nil
	case "gemini":
		credentials, err := googleauth.Detect(context.Background(), os.Getenv("GHOST_WISPR_GOOGLE_CREDENTIALS_FILE"), googleauth.CloudPlatformScope)
		if err != nil {
			t.Fatalf("Google credentials: %v", err)
		}
		if logCredentials {
			// Name the kind of credential, never its contents: a run must
			// show which identity spoke, as the metadata server's is the
			// only one these runs may use off the Pi.
			var kind struct {
				Type string `json:"type"`
			}
			if data := credentials.JSON(); len(data) == 0 {
				t.Log("gemini: Google credentials from the metadata server")
			} else if err := json.Unmarshal(data, &kind); err == nil {
				t.Logf("gemini: Google credentials of type %q from a file", kind.Type)
			}
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(transport.CloseIdleConnections)
		conns := &streamConns{base: transport}
		p := liveProvider(t, &config.Provider{
			API:      config.APIVertex,
			Project:  os.Getenv("GHOST_WISPR_LIVE_GCP_PROJECT"),
			Location: cmp.Or(os.Getenv("GHOST_WISPR_LIVE_TTS_LOCATION"), "us"),
			Auth:     config.ProviderAuth{Kind: config.AuthGoogle},
		}, credentials)
		g, err := NewEngine(p, cmp.Or(model, "gemini-3.8-flash-tts"), voice, WithHTTPClient(&http.Client{Transport: conns}))
		if err != nil {
			t.Fatal(err)
		}
		return g, conns
	default:
		t.Fatalf("GHOST_WISPR_LIVE_TTS=%q, want aura or gemini", name)
	}
	return nil, nil
}

// liveProvider is p, signed in with the key its auth.env names in the
// environment, or with credentials.
func liveProvider(t *testing.T, p *config.Provider, credentials *auth.Credentials) *providers.Provider {
	t.Helper()
	cfg := &config.Config{
		Providers:    map[string]config.Provider{"live": *p},
		ProviderKeys: map[string]string{p.Auth.Env: os.Getenv(p.Auth.Env)},
	}
	got, err := providers.New(cfg, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) { return credentials, nil },
	}).Get("live")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// sayFirstFrame says text and returns the time from the call to its first
// frame written to out.
func sayFirstFrame(t *testing.T, s voice.Session, out *firstFrameOutput, text string) time.Duration {
	t.Helper()
	out.armed.Store(true)
	said := time.Now()
	if err := s.Say(context.Background(), text, voice.Fact); err != nil {
		t.Fatalf("Say %q: %v", text, err)
	}
	select {
	case first := <-out.first:
		return first.Sub(said)
	default:
		t.Fatalf("Say %q returned without writing a frame", text)
	}
	return 0
}

func logMedian(t *testing.T, name, what string, waits []time.Duration) {
	t.Helper()
	slices.Sort(waits)
	median := (waits[len(waits)/2-1] + waits[len(waits)/2]) / 2
	t.Logf("%s: median %s %v over %d (fastest %v, slowest %v)", name, what,
		median.Round(time.Millisecond), len(waits), waits[0].Round(time.Millisecond), waits[len(waits)-1].Round(time.Millisecond))
}

// TestLiveTimeToFirstAudio measures, against the real service, the time from
// each of ten Says in one Session to its first frame written to the Output,
// and the median. It runs only when GHOST_WISPR_LIVE_TTS names an engine:
//
//	GHOST_WISPR_LIVE_TTS=aura GHOST_WISPR_DEEPGRAM_API_KEY=<key> go test -run TestLive -v ./internal/voice/tts
//	GHOST_WISPR_LIVE_TTS=gemini GHOST_WISPR_LIVE_GCP_PROJECT=<project> go test -run TestLive -v ./internal/voice/tts
//
// Gemini, on Vertex AI, reads Application Default Credentials, or the file
// GHOST_WISPR_GOOGLE_CREDENTIALS_FILE names; GHOST_WISPR_LIVE_TTS_LOCATION
// chooses its location ("us" by default; "global" to compare). Each engine
// takes GHOST_WISPR_LIVE_TTS_MODEL (aura-2-harmonia-en and
// gemini-3.8-flash-tts by default) and GHOST_WISPR_LIVE_TTS_VOICE.
func TestLiveTimeToFirstAudio(t *testing.T) {
	engine, _ := liveEngine(t, true)
	out := &firstFrameOutput{first: make(chan time.Time, 1)}
	opened := time.Now()
	s, err := New(engine).Open(context.Background(), &voice.Exchange{Out: out})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	t.Logf("%s: Open took %v", engine.Name(), time.Since(opened).Round(time.Millisecond))

	waits := make([]time.Duration, 0, len(liveSentences))
	for i, text := range liveSentences {
		wait := sayFirstFrame(t, s, out, text)
		waits = append(waits, wait)
		t.Logf("%s: Say %2d: first frame after %v (%q)", engine.Name(), i+1, wait.Round(time.Millisecond), text)
	}
	logMedian(t, engine.Name(), "time to first audio", waits)
}

// TestLiveFirstSayTimeToFirstAudio measures, against the real service, the
// first Say of ten fresh Sessions, each with a fresh engine, as an exchange
// meets it: the Session opens at the wake, liveWakeToTurn before the turn is
// final, and then speaks. It runs as TestLiveTimeToFirstAudio does.
func TestLiveFirstSayTimeToFirstAudio(t *testing.T) {
	waits := make([]time.Duration, 0, len(liveSentences))
	for i, text := range liveSentences {
		engine, conns := liveEngine(t, i == 0)
		out := &firstFrameOutput{first: make(chan time.Time, 1)}
		opened := time.Now()
		s, err := New(engine).Open(context.Background(), &voice.Exchange{Out: out})
		if err != nil {
			t.Fatalf("Open %d: %v", i+1, err)
		}
		open := time.Since(opened)
		time.Sleep(liveWakeToTurn)
		wait := sayFirstFrame(t, s, out, text)
		_ = s.Close()
		waits = append(waits, wait)
		t.Logf("%s: Session %2d: Open took %v, first frame %v after its first Say, which went out on %s (%q)",
			engine.Name(), i+1, open.Round(time.Millisecond), wait.Round(time.Millisecond), conns.describe(), text)
	}
	logMedian(t, "first Say", "time to first audio", waits)
}
