package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
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
		c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, nil)
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
	t.Cleanup(lazy.Stop)
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
