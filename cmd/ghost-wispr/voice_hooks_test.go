package main

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// forgetting is a voice that records what it is told to forget, and the
// sessions it is told started.
type forgetting struct {
	closingsHeard
	mu       sync.Mutex
	started  []string
	ended    []string
	deleted  []string
	segments []string
}

func (f *forgetting) SessionStarted(id string, _ uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
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

type sessionHookHolder struct {
	started func(string, uint64)
	ended   func(string)
}

func (h *sessionHookHolder) SetSessionStartedHook(hook func(string, uint64)) { h.started = hook }

func (h *sessionHookHolder) SetSessionEndedHook(hook func(string)) { h.ended = hook }

// TestSessionAndSegmentChangesReachTheVoiceAndTheConcierge fails if a session
// start does not reach the voice, or a session end, a session delete (the
// API's and the GC's) or a segment delete, wired as main wires them, does not
// reach both the voice and the Concierge.
func TestSessionAndSegmentChangesReachTheVoiceAndTheConcierge(t *testing.T) {
	var link voiceLink
	v := &forgetting{}
	link.set(v)
	var hooks conciergeHooks
	c := &recordedSessions{}
	hooks.set(c)
	manager := &sessionHookHolder{}

	forgotten := wireVoiceSessionHooks(manager, &hooks, &link)
	if manager.started == nil || manager.ended == nil {
		t.Fatal("no session-started or session-ended hook on the manager")
	}
	manager.started("s0", 1)
	manager.ended("s1")
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
		"voice started": v.started, "voice ended": v.ended, "voice deleted": v.deleted, "voice segments": v.segments,
		"concierge ended": c.ended, "concierge deleted": c.deleted,
	} {
		want := map[string][]string{
			"voice started": {"s0"}, "voice ended": {"s1"}, "voice deleted": {"s2"}, "voice segments": {"s3", "s4"},
			"concierge ended": {"s1"}, "concierge deleted": {"s2"},
		}[name]
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

var _ = voice.Transcript{}
