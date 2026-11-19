package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestTheVoiceLinkIsInertUntilTheVoiceStarts fails if a gate closing, a
// Jabra press, a transcript, the floor or shutdown reaching the link before
// the voice is built panics or blocks.
func TestTheVoiceLinkIsInertUntilTheVoiceStarts(t *testing.T) {
	var link voiceLink
	link.GateClosed(1)
	link.Stop("Jabra button")
	link.OnTranscript(&voice.Transcript{Text: "Wispr, hello", GateOpen: true})
	link.OnUtteranceEnd(0, true)
	if err := link.WaitFloor(t.Context()); err != nil {
		t.Fatalf("WaitFloor with no voice: %v", err)
	}
	link.Close()
}

type stubConcierge struct{ answers chan voice.PendingAnswer }

func (stubConcierge) Tools(*voice.Exchange) voice.ToolRunner { return nil }
func (stubConcierge) Heard(*voice.Exchange, string)          {}
func (stubConcierge) Answer(context.Context, *voice.Exchange, string, voice.SayFunc) error {
	return nil
}
func (c stubConcierge) Answers() <-chan voice.PendingAnswer { return c.answers }
func (stubConcierge) Spoken(context.Context, string) error  { return nil }

// TestTheVoiceStartsOnlyWhenItCanSpeak fails if the voice starts while it is
// off, with no speaker, with no Concierge, or on an engine this build does
// not have, or does not start with voice A on Aura with a key.
func TestTheVoiceStartsOnlyWhenItCanSpeak(t *testing.T) {
	output := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return nil, 0, context.Canceled }})
	t.Cleanup(func() { _ = output.Close() })
	concierge := stubConcierge{answers: make(chan voice.PendingAnswer)}
	fakeDeepgramKey := strings.Repeat("x", 8) // any non-empty key; no request leaves the test
	cases := map[string]struct {
		change    func(*config.Config)
		noSpeaker bool
		noConcier bool
		starts    bool
	}{
		"on, Aura with a key": {starts: true},
		"off":                 {change: func(c *config.Config) { c.Voice.Enabled = false }},
		"no speaker":          {noSpeaker: true},
		"no concierge":        {noConcier: true},
		"aura without a key":  {change: func(c *config.Config) { c.DeepgramAPIKey = "" }},
		"voice B not built":   {change: func(c *config.Config) { c.Voice.Engine = config.VoiceEngineGPTLive }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{
				SpeakerEnabled: true,
				DeepgramAPIKey: fakeDeepgramKey,
				Voice:          config.Voice{Enabled: true, Engine: config.VoiceEngineTTS, TTSEngine: config.TTSEngineAura, WakePhrases: []string{"Wispr"}},
			}
			if c.change != nil {
				c.change(cfg)
			}
			deps := &voiceDeps{output: output, tap: voice.NewTap(), concierge: concierge, session: func() string { return "" }, red: func() bool { return true }, notify: func(string, string) {}}
			if c.noSpeaker {
				deps.output = nil
			}
			if c.noConcier {
				deps.concierge = nil
			}
			var link voiceLink
			deps.link = &link
			err := startVoice(t.Context(), cfg, deps)
			started := link.get() != nil
			if started != c.starts {
				t.Fatalf("started %v (error %v), want %v", started, err, c.starts)
			}
			if !c.starts && cfg.Voice.Enabled && err == nil {
				t.Fatal("the voice did not start and said nothing about why")
			}
			link.Close()
		})
	}
}

// TestKeytermsOnlyForNova3 fails if keyterms go to a Deepgram model that does
// not take them.
func TestKeytermsOnlyForNova3(t *testing.T) {
	cfg := &config.Config{DeepgramModel: "nova-2", Voice: config.Voice{Enabled: true, WakePhrases: []string{"Wispr"}}}
	if got := voiceKeyterms(cfg); len(got) != 0 {
		t.Fatalf("keyterms %q for nova-2, want none", got)
	}
	cfg.DeepgramModel = "nova-3-general"
	if got := voiceKeyterms(cfg); !slices.Equal(got, []string{"Wispr"}) {
		t.Fatalf("keyterms %q for nova-3-general, want the wake phrase", got)
	}
}

// closingsHeard is a voice that records the gate closings it hears.
type closingsHeard struct {
	mu       sync.Mutex
	closings []int
	stops    []string
}

func (c *closingsHeard) GateClosed(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closings = append(c.closings, n)
}

func (c *closingsHeard) Stop(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops = append(c.stops, reason)
}
func (*closingsHeard) OnTranscript(*voice.Transcript)  {}
func (*closingsHeard) OnUtteranceEnd(int, bool)        {}
func (*closingsHeard) WaitFloor(context.Context) error { return nil }
func (*closingsHeard) Close()                          {}

// TestAPauseStopsTheVoiceThroughTheGate fails if a pause or a light hold of
// the real capture gate does not reach the voice the link holds.
func TestAPauseStopsTheVoiceThroughTheGate(t *testing.T) {
	var link voiceLink
	heard := &closingsHeard{}
	link.set(heard)
	capture := newCaptureGate(nil, nil, nil)
	wireVoiceToCapture(capture, &link)
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	capture.HoldForLight()
	deadline := time.Now().Add(time.Second)
	for {
		heard.mu.Lock()
		got := slices.Clone(heard.closings)
		heard.mu.Unlock()
		if slices.Equal(got, []int{1, 2}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the voice heard closings %v after a pause and a light hold, want [1 2]", got)
		}
		time.Sleep(time.Millisecond)
	}
}
