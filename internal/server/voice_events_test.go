package server

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type recordingEventStore struct {
	mu     sync.Mutex
	stored []string
}

func (s *recordingEventStore) StoreEvent(_ context.Context, eventType, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored = append(s.stored, eventType)
	return nil
}

func (s *recordingEventStore) HasSession(string) (bool, error) { return true, nil }

// TestVoiceEventsGoToWebSocketsOnlyAndHoldNoWords fails if a voice event is
// stored, carries anything but its type, version, time, CLOCK_MONOTONIC time
// and session id, or if the hub sends a voice event of a kind the voice
// contract does not name.
func TestVoiceEventsGoToWebSocketsOnlyAndHoldNoWords(t *testing.T) {
	store := &recordingEventStore{}
	hub := NewHub()
	hub.SetEventStore(store)
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	kinds := []string{"voice_wake", "voice_exchange_started", "voice_answer_ready", "voice_spoken", "voice_tasks_changed", "agent_joined", "agent_left"}
	for _, kind := range kinds {
		before := monotonicNow(t)
		hub.BroadcastVoiceEvent(kind, "20261119-090000")
		after := monotonicNow(t)
		select {
		case msg := <-ch:
			var payload map[string]any
			if err := json.Unmarshal(msg, &payload); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			keys := slices.Sorted(maps.Keys(payload))
			if !slices.Equal(keys, []string{"at_mono_ns", "session_id", "timestamp", "type", "version"}) {
				t.Errorf("%s carries %v, want only type, version, timestamp, at_mono_ns and session_id", kind, keys)
			}
			if payload["type"] != kind || payload["session_id"] != "20261119-090000" {
				t.Errorf("%s payload %s", kind, msg)
			}
			// The acceptance driver compares at_mono_ns with its own reads of
			// CLOCK_MONOTONIC, so a step of the wall clock moves neither.
			if mono, ok := payload["at_mono_ns"].(float64); !ok || int64(mono) < before-1_000_000 || int64(mono) > after+1_000_000 {
				t.Errorf("%s: at_mono_ns %v, want CLOCK_MONOTONIC between %d and %d", kind, payload["at_mono_ns"], before, after)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: nothing broadcast", kind)
		}
	}
	if len(store.stored) != 0 {
		t.Errorf("stored %v, want no voice event stored", store.stored)
	}

	hub.BroadcastVoiceEvent("voice_transcript", "20261119-090000")
	select {
	case msg := <-ch:
		t.Errorf("an unnamed voice event went out: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func monotonicNow(t *testing.T) int64 {
	t.Helper()
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		t.Fatalf("CLOCK_MONOTONIC: %v", err)
	}
	return ts.Nano()
}
