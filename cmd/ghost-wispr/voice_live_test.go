package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// soundsHeard is a voice that records the start and error sounds it is asked
// for.
type soundsHeard struct {
	closingsHeard
	mu      sync.Mutex
	started int
	failed  int
}

func (s *soundsHeard) Started() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started++
}

func (s *soundsHeard) RedCheckFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed++
}

func (s *soundsHeard) counts() (started, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.failed
}

// TestTheVoiceStartsWithASpeakerAndAConcierge fails if the voice does not
// start with a speaker and a Concierge, whatever voice.enabled and
// voice.engine say (both are read live at each call, R13), or starts without
// either.
func TestTheVoiceStartsWithASpeakerAndAConcierge(t *testing.T) {
	output := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) { return nil, 0, context.Canceled }})
	t.Cleanup(func() { _ = output.Close() })
	concierge := stubConcierge{answers: make(chan voice.PendingAnswer)}
	noVoice := func() (voice.Voice, error) { return nil, errors.New("no voice in this test") }
	cases := map[string]struct {
		noSpeaker, noConcierge bool
		starts                 bool
	}{
		"speaker and Concierge": {starts: true},
		"no speaker":            {noSpeaker: true},
		"no Concierge":          {noConcierge: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var link voiceLink
			deps := &voiceDeps{
				output: output, tap: voice.NewTap(), concierge: concierge, link: &link,
				voices: noVoice, enabled: func() bool { return false }, wakePhrases: []string{"Wispr"},
				session: func() string { return "" }, red: func() bool { return true }, notify: func(string, string) {},
			}
			if c.noSpeaker {
				deps.output = nil
			}
			if c.noConcierge {
				deps.concierge = nil
			}
			err := startVoice(deps)
			if started := link.get() != nil; started != c.starts {
				t.Fatalf("started %v (error %v), want %v", started, err, c.starts)
			}
			if !c.starts && err == nil {
				t.Fatal("the voice did not start and said nothing about why")
			}
			link.Close()
		})
	}
}

// TestEachSettingBuildsItsVoice fails if the voice for a call does not follow
// the settings saved last, rebuilds for settings that did not change, or
// builds a voice this build does not have.
func TestEachSettingBuildsItsVoice(t *testing.T) {
	key := strings.Repeat("k", 8) // any non-empty key; no request leaves the test
	cfg := config.Config{DeepgramAPIKey: key, Voice: config.Voice{Engine: config.VoiceEngineTTS, TTSEngine: config.TTSEngineAura}}
	var source voiceSource
	source.apply(t.Context(), &cfg)
	first, err := source.Voice()
	if err != nil || first == nil {
		t.Fatalf("voice A on Aura: %v, %v", first, err)
	}
	source.apply(t.Context(), &cfg)
	if again, _ := source.Voice(); again != first {
		t.Fatal("the same settings built a second voice")
	}
	cfg.Voice.TTSVoice = "aura-2-other-en"
	source.apply(t.Context(), &cfg)
	if changed, _ := source.Voice(); changed == first || changed == nil {
		t.Fatal("a new tts_voice kept the old voice")
	}
	cfg.Voice.Engine = config.VoiceEngineGPTLive
	source.apply(t.Context(), &cfg)
	if v, err := source.Voice(); v != nil || err == nil || !strings.Contains(err.Error(), "not in this build") {
		t.Fatalf("voice B in this build: %v, %v", v, err)
	}
	cfg.Voice.Engine, cfg.DeepgramAPIKey = config.VoiceEngineTTS, ""
	source.apply(t.Context(), &cfg)
	if v, err := source.Voice(); v != nil || err == nil {
		t.Fatalf("voice A on Aura with no key: %v, %v", v, err)
	}
}

// TestCaptureOpeningPlaysTheStartSound fails if capture moving into recording
// does not ask the voice for the start sound (R10).
func TestCaptureOpeningPlaysTheStartSound(t *testing.T) {
	var link voiceLink
	heard := &soundsHeard{}
	link.set(heard)
	capture := newCaptureGate(nil, nil, nil)
	wireVoiceToCapture(capture, &link)
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	eventuallyTrue(t, "the start sound", func() bool { started, _ := heard.counts(); return started == 1 })
}

// TestAFailedRedCheckPlaysTheErrorSound fails if the light failing its red
// check, on a resume or while recording, does not ask the voice for the error
// sound, or asks again while the same fault holds.
func TestAFailedRedCheckPlaysTheErrorSound(t *testing.T) {
	t.Run("on a resume", func(t *testing.T) {
		var link voiceLink
		heard := &soundsHeard{}
		link.set(heard)
		capture := newCaptureGate(nil, nil, nil)
		capture.beforeGateOpen = func() error { return errors.New("the light reads off") }
		wireVoiceToCapture(capture, &link)
		capture.Pause()
		capture.Resume()
		eventuallyTrue(t, "the error sound", func() bool { _, failed := heard.counts(); return failed == 1 })
	})
	t.Run("while recording", func(t *testing.T) {
		var link voiceLink
		heard := &soundsHeard{}
		link.set(heard)
		capture := newCaptureGate(nil, nil, nil)
		wireVoiceToCapture(capture, &link)
		guard := newLightGuard(capture, newWarningsRegistry(), time.Hour)
		guard.apply(errors.New("the light reads blue"))
		guard.apply(errors.New("the light reads blue"))
		if _, failed := heard.counts(); failed != 1 {
			t.Fatalf("the error sound was asked %d times for one fault, want once", failed)
		}
	})
}

// TestTheConciergeStartsOnFirstUse fails if a Concierge for a voice that is
// off at start-up starts before a call needs it, starts twice, or does not
// pass its answers on once started.
func TestTheConciergeStartsOnFirstUse(t *testing.T) {
	cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: "test"}
	store := conciergeStore(t)
	starts := 0
	var started *concierge.Concierge
	lazy := newLazyConcierge(func() (*concierge.Concierge, func(), error) {
		starts++
		c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, nil)
		started = c
		return c, stop, err
	}, nil, discardLogger)
	t.Cleanup(lazy.Stop)
	if err := lazy.Spoken(t.Context(), "t1"); err != nil {
		t.Fatalf("Spoken before any call: %v", err)
	}
	if err := lazy.NeverMind(t.Context(), "s1"); err != nil {
		t.Fatalf("NeverMind before any call: %v", err)
	}
	if starts != 0 {
		t.Fatalf("started %d times before any call", starts)
	}
	lazy.Heard(t.Context(), &voice.Exchange{SessionID: "s1"}, "what is happening")
	lazy.Tools(&voice.Exchange{SessionID: "s1"})
	if starts != 1 || started == nil {
		t.Fatalf("started %d times on first use, want once", starts)
	}
	if lazy.Answers() == nil {
		t.Fatal("no answers channel")
	}
}

// TestALazyConciergeThatCannotStartAnswersUnreachable fails if a Concierge
// that cannot start leaves a call silent instead of failing it as
// unreachable, so the voice says sorry and plays the error sound.
func TestALazyConciergeThatCannotStartAnswersUnreachable(t *testing.T) {
	lazy := newLazyConcierge(func() (*concierge.Concierge, func(), error) {
		return nil, nil, errors.New("no model credentials")
	}, nil, discardLogger)
	t.Cleanup(lazy.Stop)
	err := lazy.Answer(t.Context(), &voice.Exchange{}, "what is happening", func(context.Context, string, voice.SayMode) error { return nil })
	if !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("Answer = %v, want an error that wraps voice.ErrUnreachable", err)
	}
	if _, err := lazy.Tools(&voice.Exchange{}).Run(t.Context(), voice.ToolCall{Name: "read_issue"}); err == nil {
		t.Fatal("a tool ran on a Concierge that did not start")
	}
}

// TestVoiceTimingIsLogged fails if a timing event does not reach the journal
// as the acceptance driver reads it.
func TestVoiceTimingIsLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	at := time.Date(2026, 11, 19, 9, 0, 0, 5, time.UTC)
	voiceTiming(logger)("stop", "s1", at, "reason", "Jabra button")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("%v: %s", err, buf.Bytes())
	}
	if line["msg"] != "voice timing" || line["event"] != "stop" || line["session_id"] != "s1" || line["reason"] != "Jabra button" || line["at_unix_ns"] != float64(at.UnixNano()) {
		t.Fatalf("logged %s", buf.Bytes())
	}
}

func eventuallyTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
