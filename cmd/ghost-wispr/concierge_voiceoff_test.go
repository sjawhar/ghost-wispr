package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// countingLazyConcierge is a lazy Concierge over the real startConcierge that
// counts its starts and stops.
func countingLazyConcierge(t *testing.T) (lazy *lazyConcierge, counts func() (starts, stops int)) {
	t.Helper()
	cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: strings.Repeat("k", 8)}
	store := conciergeStore(t)
	var mu sync.Mutex
	starts, stops := 0, 0
	lazy = newLazyConcierge(func() (*concierge.Concierge, func(), error) {
		c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
		mu.Lock()
		starts++
		mu.Unlock()
		return c, func() {
			mu.Lock()
			stops++
			mu.Unlock()
			stop()
		}, err
	}, nil, discardLogger)
	t.Cleanup(func() { lazy.Stop(context.Background()) })
	return lazy, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return starts, stops
	}
}

// TestTheConciergeStopsWhenTheVoiceIsTurnedOff fails if turning the voice off
// on the settings page leaves the Concierge running (R13: with voice.enabled
// off, no Concierge runs), or if turning it on again does not start a new
// one.
func TestTheConciergeStopsWhenTheVoiceIsTurnedOff(t *testing.T) {
	lazy, counts := countingLazyConcierge(t)
	first, err := lazy.start()
	if err != nil || first == nil {
		t.Fatalf("start: %v", err)
	}
	lazy.followEnabled(false)
	if _, stops := counts(); stops != 1 {
		t.Fatalf("turning the voice off stopped the Concierge %d times, want once", stops)
	}
	if lazy.started() != nil {
		t.Fatal("the Concierge still counts as started with the voice off")
	}
	// A call that still comes with the voice off starts none (ReviewPR89's
	// round 2, finding 1).
	if c, err := lazy.start(); c != nil || err == nil {
		t.Fatalf("start with the voice off gave %v, %v; want no Concierge and an error", c, err)
	}
	if starts, _ := counts(); starts != 1 {
		t.Fatalf("started %d times with the voice off, want once in all", starts)
	}
	lazy.followEnabled(true)
	deadline := time.Now().Add(2 * time.Second)
	for lazy.started() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	second := lazy.started()
	if second == nil || second == first {
		t.Fatal("turning the voice on again did not start a new Concierge")
	}
	if starts, _ := counts(); starts != 2 {
		t.Fatalf("started %d times, want twice", starts)
	}
	lazy.followEnabled(false)
	lazy.followEnabled(false)
	if _, stops := counts(); stops != 2 {
		t.Fatalf("stopped %d times after two turns off, want twice", stops)
	}
}

// TestTurningTheVoiceOffCancelsTheQuestionsAShutdownKeeps fails if a
// turn-off leaves a question of a meeting still recording working (PR #89's
// review, round 4: its late reply would play the answer sound once the voice
// is on again), or if a shutdown cancels it (the unit's Envoy session is the
// same after a restart, so its reply can still land).
func TestTurningTheVoiceOffCancelsTheQuestionsAShutdownKeeps(t *testing.T) {
	cfg := config.Config{GenAIBackend: "gemini", GeminiAPIKey: strings.Repeat("k", 8)}
	store := conciergeStore(t)
	at := time.Now()
	if err := store.CreateSession("meeting", at); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "q1", SessionID: "meeting", AskedAt: at, Holder: "session:agent-7"}); err != nil {
		t.Fatal(err)
	}
	newLazy := func() *lazyConcierge {
		lazy := newLazyConcierge(func() (*concierge.Concierge, func(), error) {
			return startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
		}, nil, discardLogger)
		t.Cleanup(func() { lazy.Stop(context.Background()) })
		if _, err := lazy.start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		return lazy
	}
	newLazy().Stop(context.Background())
	if task, err := store.VoiceTask("q1"); err != nil || task.State != storage.VoiceTaskWorking {
		t.Fatalf("after a shutdown the question = %+v, %v; want working", task, err)
	}
	newLazy().followEnabled(false)
	if task, err := store.VoiceTask("q1"); err != nil || task.State != storage.VoiceTaskCancelled {
		t.Fatalf("after the voice went off the question = %+v, %v; want cancelled", task, err)
	}
}

// TestTurningTheVoiceOffReachesTheVoiceAndTheConcierge fails if the settings
// hook of voice.enabled, as main wires it, does not tell the voice (which
// stops at once) and stop the Concierge.
func TestTurningTheVoiceOffReachesTheVoiceAndTheConcierge(t *testing.T) {
	var link voiceLink
	heard := &closingsHeard{}
	link.set(heard)
	lazy, counts := countingLazyConcierge(t)
	if _, err := lazy.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	followVoiceEnabled(&link, lazy)(&config.Config{})
	heard.mu.Lock()
	changes := heard.enabledChanges
	heard.mu.Unlock()
	if changes != 1 {
		t.Fatalf("the voice heard %d settings changes, want 1", changes)
	}
	if _, stops := counts(); stops != 1 {
		t.Fatalf("the Concierge stopped %d times, want once", stops)
	}
}
