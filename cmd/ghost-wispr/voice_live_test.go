package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/dgagent"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
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

// The office Pi's config carries voice B's OpenAI workload identity, so that
// picking gptlive on the settings page builds voice B with no other change.
// It keeps voice A as the engine it starts with.
func TestThePiConfigCanPickVoiceB(t *testing.T) {
	cfg, _, err := config.Load(filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml"))
	if err != nil {
		t.Fatalf("load the Pi's config: %v", err)
	}
	if cfg.Voice.Engine != config.VoiceEngineTTS {
		t.Errorf("the Pi starts with voice.engine %q, want %q until a build with voice B runs there", cfg.Voice.Engine, config.VoiceEngineTTS)
	}
	ids := cfg.Voice.GPTLive
	if !strings.HasPrefix(ids.OpenAIIdentityProviderID, "idp_") || !strings.HasPrefix(ids.OpenAIServiceAccountID, "user-") {
		t.Fatalf("the Pi's voice.gptlive is %+v, want its OpenAI identity provider (idp_…) and service account (user-…)", ids)
	}
	cfg.Voice.Engine = config.VoiceEngineGPTLive
	var source voiceSource
	source.apply(t.Context(), &cfg)
	if v, err := source.Voice(); err != nil {
		t.Fatalf("picking gptlive with the Pi's config: %v, %v", v, err)
	} else if _, ok := v.(*gptlive.Voice); !ok {
		t.Fatalf("picking gptlive with the Pi's config built %T, want voice B", v)
	}
}

// TestEachSettingBuildsItsVoice fails if the voice for a call does not follow
// the settings saved last, rebuilds for settings that did not change, builds
// a voice this build does not have, or does not build voice C on
// voice.engine dgagent, and again for a new think model.
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
	if v, err := source.Voice(); v != nil || err == nil || !strings.Contains(err.Error(), "openai_identity_provider_id") {
		t.Fatalf("voice B without its OpenAI ids: %v, %v", v, err)
	}
	cfg.Voice.GPTLive = config.GPTLive{OpenAIIdentityProviderID: "idp_example", OpenAIServiceAccountID: "user-example"}
	source.apply(t.Context(), &cfg)
	voiceB, err := source.Voice()
	if _, ok := voiceB.(*gptlive.Voice); !ok || err != nil {
		t.Fatalf("voice B with its OpenAI ids: %v, %v", voiceB, err)
	}
	cfg.Voice.Engine = config.VoiceEngineDGAgent
	source.apply(t.Context(), &cfg)
	voiceC, err := source.Voice()
	if _, isC := voiceC.(*dgagent.Agent); err != nil || !isC {
		t.Fatalf("voice C on dgagent: %T, %v", voiceC, err)
	}
	cfg.Voice.DGAgent.ThinkModel = "open_ai/gpt-4o-mini"
	source.apply(t.Context(), &cfg)
	if changed, _ := source.Voice(); changed == voiceC || changed == nil {
		t.Fatal("a new voice.dgagent.think_model kept the old voice C")
	}
	cfg.DeepgramAPIKey = ""
	source.apply(t.Context(), &cfg)
	if v, err := source.Voice(); v != nil || err == nil {
		t.Fatalf("voice C with no key: %v, %v", v, err)
	}
	cfg.Voice.Engine = config.VoiceEngineTTS
	source.apply(t.Context(), &cfg)
	if v, err := source.Voice(); v != nil || err == nil {
		t.Fatalf("voice A on Aura with no key: %v, %v", v, err)
	}
}

// TestTheStartSoundPlaysAtTheFirstAudioRecorded fails if capture moving into
// recording asks the voice for the start sound before any audio reaches the
// recording, more than once for one opening, or not at all (R10: the note
// says "recording"). On the Jabra a sound that plays before the microphone
// opens holds the light (prepareLightForOpen), so the microphone opened only
// once the sound and the speaker's idle close had passed (ReviewPR89's round
// 4).
func TestTheStartSoundPlaysAtTheFirstAudioRecorded(t *testing.T) {
	var link voiceLink
	heard := &soundsHeard{}
	link.set(heard)
	capture := newCaptureGate(nil, nil, nil)
	wireVoiceToCapture(capture, &link, nil)
	recording := capture.Writer(io.Discard)
	started := func() int { n, _ := heard.counts(); return n }

	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	time.Sleep(50 * time.Millisecond)
	if n := started(); n != 0 {
		t.Fatalf("the start sound was asked %d times before any audio reached the recording, want none", n)
	}
	for range 2 {
		if _, err := recording.Write(make([]byte, 320)); err != nil {
			t.Fatalf("a write through the open gate: %v", err)
		}
	}
	if n := started(); n != 1 {
		t.Fatalf("the start sound was asked %d times for one opening's audio, want once", n)
	}

	// The next opening plays it again, at its own first audio.
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	if _, err := recording.Write(make([]byte, 320)); err != nil {
		t.Fatalf("a write through the reopened gate: %v", err)
	}
	if n := started(); n != 2 {
		t.Fatalf("the start sound was asked %d times over two openings, want twice", n)
	}
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
		wireVoiceToCapture(capture, &link, nil)
		capture.Pause()
		capture.Resume()
		eventuallyTrue(t, "the error sound", func() bool { _, failed := heard.counts(); return failed == 1 })
	})
	t.Run("while recording", func(t *testing.T) {
		var link voiceLink
		heard := &soundsHeard{}
		link.set(heard)
		capture := newCaptureGate(nil, nil, nil)
		wireVoiceToCapture(capture, &link, nil)
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
	lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
		starts++
		c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
		started = c
		return c, stop, err
	}, nil, discardLogger)
	t.Cleanup(func() { lazy.Stop(context.Background()) })
	if err := lazy.Spoken(t.Context(), "t1"); err != nil {
		t.Fatalf("Spoken before any call: %v", err)
	}
	if cancelled, err := lazy.NeverMind(t.Context(), "s1", lazy.QuestionsAsked()); err != nil || cancelled {
		t.Fatalf("NeverMind before any call = %v, %v; want nothing cancelled", cancelled, err)
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
	lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
		return nil, nil, errors.New("no model credentials")
	}, nil, discardLogger)
	t.Cleanup(func() { lazy.Stop(context.Background()) })
	err := lazy.Answer(t.Context(), &voice.Exchange{}, "what is happening", func(context.Context, string, voice.SayMode) error { return nil })
	if !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("Answer = %v, want an error that wraps voice.ErrUnreachable", err)
	}
	if _, err := lazy.Tools(&voice.Exchange{}).Run(t.Context(), voice.ToolCall{Name: "read_issue"}); err == nil {
		t.Fatal("a tool ran on a Concierge that did not start")
	}
}

// TestVoiceTimingIsLogged fails if a timing event does not reach the journal
// as the acceptance driver reads it: through logging.New, the logger the
// service writes its journal with, as message "voice timing", with its time on
// the wall clock and on CLOCK_MONOTONIC (at_mono_ns), which the driver compares
// with its own reads of that clock. A time with no monotonic reading has no
// at_mono_ns, rather than one its wall clock would place.
func TestVoiceTimingIsLogged(t *testing.T) {
	var ts unix.Timespec
	monotonic := func() int64 {
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
			t.Fatal(err)
		}
		return ts.Nano()
	}
	before := monotonic()
	now := time.Now()
	after := monotonic()
	for name, tc := range map[string]struct {
		at   time.Time
		mono bool
	}{
		"time.Now": {now, true},
		"built":    {time.Date(2026, 11, 19, 9, 0, 0, 5, time.UTC), false},
	} {
		var buf bytes.Buffer
		logger := logging.New(&buf, logging.ParseLevel("info"))
		voiceTiming(logger)("stop", "s1", tc.at, "reason", "Jabra button")
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("%s: %v: %s", name, err, buf.Bytes())
		}
		if line["message"] != "voice timing" || line["event"] != "stop" || line["session_id"] != "s1" || line["reason"] != "Jabra button" || line["at_unix_ns"] != float64(tc.at.UnixNano()) {
			t.Fatalf("%s: logged %s", name, buf.Bytes())
		}
		mono, has := line["at_mono_ns"].(float64)
		if has != tc.mono {
			t.Fatalf("%s: at_mono_ns %v, want one: %v; logged %s", name, line["at_mono_ns"], tc.mono, buf.Bytes())
		}
		if has && (int64(mono) < before-1_000_000 || int64(mono) > after+1_000_000) {
			t.Fatalf("%s: at_mono_ns %d, want CLOCK_MONOTONIC between %d and %d", name, int64(mono), before, after)
		}
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
