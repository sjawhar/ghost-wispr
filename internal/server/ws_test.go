package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestWSBroadcastEventShape(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	hub.BroadcastLiveTranscript(transcribe.Segment{
		Speaker:   2,
		Text:      "test line",
		StartTime: 0.5,
		EndTime:   1.1,
		Timestamp: time.Now().UTC(),
	})

	select {
	case msg := <-ch:
		var payload map[string]any
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if payload["type"] != "live_transcript" {
			t.Fatalf("expected event type live_transcript, got %#v", payload["type"])
		}
		if payload["version"] == nil {
			t.Fatalf("expected version field in payload: %s", string(msg))
		}
		if payload["timestamp"] == nil {
			t.Fatalf("expected timestamp field in payload: %s", string(msg))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for websocket broadcast")
	}
}

func TestHubSnapshotComponentStatusesReturnsLatestPerComponent(t *testing.T) {
	hub := NewHub()
	hub.BroadcastComponentStatus("mic", "unavailable", "Microphone unavailable")
	hub.BroadcastComponentStatus("deepgram", "connected", "Deepgram connected")
	hub.BroadcastComponentStatus("mic", "error", "Mic overflow")

	snapshot := hub.SnapshotComponentStatuses()
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 component statuses, got %d", len(snapshot))
	}

	statuses := map[string]ComponentStatusEvent{}
	for _, ev := range snapshot {
		statuses[ev.Component] = ev
	}

	if statuses["mic"].Status != "error" {
		t.Fatalf("expected latest mic status to be error, got %q", statuses["mic"].Status)
	}
	if statuses["deepgram"].Status != "connected" {
		t.Fatalf("expected deepgram connected, got %q", statuses["deepgram"].Status)
	}
}
