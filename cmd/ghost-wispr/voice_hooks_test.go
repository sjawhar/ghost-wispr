package main

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// forgetting is a voice that records what it is told to forget.
type forgetting struct {
	closingsHeard
	mu       sync.Mutex
	ended    []string
	deleted  []string
	segments []string
}

func (f *forgetting) SessionEnded(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, id)
}

func (f *forgetting) SessionsDeleted(ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ids...)
}

func (f *forgetting) SegmentsDeleted(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.segments = append(f.segments, id)
}

var _ voiceControl = (*forgetting)(nil)

type endedHookHolder struct{ hook func(string) }

func (h *endedHookHolder) SetSessionEndedHook(hook func(string)) { h.hook = hook }

// TestSessionAndSegmentChangesReachTheVoiceAndTheConcierge fails if a session
// end, a session delete (the API's and the GC's) or a segment delete, wired as
// main wires them, does not reach both the voice and the Concierge.
func TestSessionAndSegmentChangesReachTheVoiceAndTheConcierge(t *testing.T) {
	var link voiceLink
	v := &forgetting{}
	link.set(v)
	var hooks conciergeHooks
	c := &recordedSessions{}
	hooks.set(c)
	manager := &endedHookHolder{}

	forgotten := wireVoiceSessionHooks(manager, &hooks, &link)
	if manager.hook == nil {
		t.Fatal("no session-ended hook on the manager")
	}
	manager.hook("s1")
	forgotten([]string{"s2"})

	controls := &server.ControlHooks{
		DeleteSegment:     func(context.Context, string, int64, int) error { return nil },
		DeleteSegmentText: func(context.Context, string, int64, int) error { return nil },
	}
	wireVoiceSegmentDeletion(controls, &link)
	if err := controls.DeleteSegment(t.Context(), "s3", 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := controls.DeleteSegmentText(t.Context(), "s4", 1, 0); err != nil {
		t.Fatal(err)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, got := range map[string][]string{
		"voice ended": v.ended, "voice deleted": v.deleted, "voice segments": v.segments,
		"concierge ended": c.ended, "concierge deleted": c.deleted,
	} {
		want := map[string][]string{
			"voice ended": {"s1"}, "voice deleted": {"s2"}, "voice segments": {"s3", "s4"},
			"concierge ended": {"s1"}, "concierge deleted": {"s2"},
		}[name]
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

var _ = voice.Transcript{}
