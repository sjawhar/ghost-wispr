package server

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventSerialization(t *testing.T) {
	events := []any{
		LiveTranscriptEvent{Event: newEvent("live_transcript", time.Unix(1, 0)), Speaker: 1, Text: "hello", StartTime: 0.1, EndTime: 1.2},
		SessionStartedEvent{Event: newEvent("session_started", time.Unix(1, 0)), SessionID: "abc"},
		SessionEndedEvent{Event: newEvent("session_ended", time.Unix(1, 0)), SessionID: "abc", Duration: 30},
		SummaryReadyEvent{Event: newEvent("summary_ready", time.Unix(1, 0)), SessionID: "abc", Summary: "ok"},
		StatusChangedEvent{Event: newEvent("status_changed", time.Unix(1, 0)), Paused: true},
	}

	for _, event := range events {
		b, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}

		if payload["type"] == nil {
			t.Fatalf("missing type in payload: %s", string(b))
		}
		if payload["version"] == nil {
			t.Fatalf("missing version in payload: %s", string(b))
		}
		if payload["timestamp"] == nil {
			t.Fatalf("missing timestamp in payload: %s", string(b))
		}
	}
}
