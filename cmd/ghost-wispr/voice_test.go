package main

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

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
	if err := link.WithFloor(t.Context(), func() {}); err != nil {
		t.Fatalf("WithFloor with no voice: %v", err)
	}
	link.Close()
}

type stubConcierge struct{ answers chan voice.PendingAnswer }

func (stubConcierge) Tools(*voice.Exchange) voice.ToolRunner         { return nil }
func (stubConcierge) Heard(context.Context, *voice.Exchange, string) {}
func (stubConcierge) Answer(context.Context, *voice.Exchange, string, voice.SayFunc) error {
	return nil
}
func (c stubConcierge) Answers() <-chan voice.PendingAnswer   { return c.answers }
func (stubConcierge) Spoken(context.Context, string) error    { return nil }
func (stubConcierge) NeverMind(context.Context, string) error { return nil }

// TestKeytermsOnlyForNova3 fails if keyterms go to a Deepgram model that does
// not take them.
func TestKeytermsOnlyForNova3(t *testing.T) {
	cfg := &config.Config{DeepgramModel: "nova-2", SpeakerEnabled: true, Voice: config.Voice{WakePhrases: []string{"Wispr"}}}
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
func (*closingsHeard) OnTranscript(*voice.Transcript)                  {}
func (*closingsHeard) OnUtteranceEnd(int, bool)                        {}
func (*closingsHeard) WithFloor(_ context.Context, start func()) error { start(); return nil }
func (*closingsHeard) Close()                                          {}
func (*closingsHeard) SessionEnded(string)                             {}
func (*closingsHeard) SessionsDeleted([]string)                        {}
func (*closingsHeard) SegmentsDeleted(string)                          {}
func (*closingsHeard) Started()                                        {}
func (*closingsHeard) RedCheckFailed()                                 {}

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
