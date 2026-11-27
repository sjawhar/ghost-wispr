package main

import (
	"slices"
	"sync"
	"testing"
)

type recordedSessions struct {
	mu      sync.Mutex
	ended   []string
	deleted []string
	// gateClosings counts the gate closings it heard.
	gateClosings int
}

func (r *recordedSessions) SessionEnded(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, id)
	return nil
}

func (r *recordedSessions) SessionDeleted(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, id)
}

func (r *recordedSessions) GateClosed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gateClosings++
}

// TestAGateClosingReachesTheConcierge fails if a pause or a light hold of the
// real capture gate does not reach the Concierge, which drops what it read
// before the session existed (ReviewPR89's round 2), or reaches it before it
// runs.
func TestAGateClosingReachesTheConcierge(t *testing.T) {
	var link voiceLink
	var hooks conciergeHooks
	capture := newCaptureGate(nil, nil, nil)
	wireVoiceToCapture(capture, &link, &hooks)
	capture.Pause() // no Concierge yet: nothing to reach
	capture.Resume()
	waitOpen(t, capture)
	c := &recordedSessions{}
	hooks.set(c)
	capture.Pause()
	capture.Resume()
	waitOpen(t, capture)
	capture.HoldForLight()
	eventuallyTrue(t, "two gate closings at the Concierge", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.gateClosings == 2
	})
}

// TestConciergeSessionHooks fails if a session end or a session delete,
// through the API or the GC, does not reach the Concierge once it runs, or
// does anything but nothing before it runs.
func TestConciergeSessionHooks(t *testing.T) {
	runParallel(t)
	var hooks conciergeHooks
	hooks.SessionEnded("before")
	hooks.SessionsDeleted([]string{"before"})

	got := &recordedSessions{}
	hooks.set(got)
	hooks.SessionEnded("s1")
	hooks.SessionsDeleted([]string{"s2", "s3"})

	const id = "20261001130000"
	store, cfg := recordingMismatchSession(t, id)
	gc := deletionNoticeGCStore{SQLiteStore: store, onDeleted: hooks.SessionsDeleted}
	if _, err := runGCPass(gc, &cfg, &recordingMismatchSyncer{matches: true}, &gcHealth{}, newWarningsRegistry(), discardLogger); err != nil {
		t.Fatalf("GC pass: %v", err)
	}
	if _, err := store.GetSession(id); err == nil {
		t.Fatal("control: the GC pass kept the session, so it tests nothing")
	}

	got.mu.Lock()
	defer got.mu.Unlock()
	if !slices.Equal(got.ended, []string{"s1"}) {
		t.Errorf("ended %v, want [s1]", got.ended)
	}
	if !slices.Equal(got.deleted, []string{"s2", "s3", id}) {
		t.Errorf("deleted %v, want [s2 s3 %s]", got.deleted, id)
	}
}
