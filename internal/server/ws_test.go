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
