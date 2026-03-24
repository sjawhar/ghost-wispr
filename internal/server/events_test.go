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
		SummaryReadyEvent{Event: newEvent("summary_ready", time.Unix(1, 0)), SessionID: "abc", Summary: "ok", Status: "completed"},
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
func TestSummaryReadyEventIncludesPreset(t *testing.T) {
	event := SummaryReadyEvent{
		Event:     newEvent("summary_ready", time.Unix(1, 0)),
		SessionID: "abc",
		Summary:   "ok",
		Status:    "completed",
		Preset:    "meeting-notes",
	}

	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	preset, ok := payload["summary_preset"]
	if !ok {
		t.Fatalf("missing summary_preset in payload: %s", string(b))
	}
	if preset != "meeting-notes" {
		t.Fatalf("expected summary_preset=meeting-notes, got %v", preset)
	}
}

func TestComponentStatusEventSerialization(t *testing.T) {
	event := ComponentStatusEvent{
		Event:     newEvent("component_status", time.Unix(1, 0)),
		Component: "deepgram",
		Status:    "disconnected",
		Message:   "Deepgram connection lost",
	}

	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if payload["type"] != "component_status" {
		t.Fatalf("expected type=component_status, got %v", payload["type"])
	}
	if payload["component"] != "deepgram" {
		t.Fatalf("expected component=deepgram, got %v", payload["component"])
	}
	if payload["status"] != "disconnected" {
		t.Fatalf("expected status=disconnected, got %v", payload["status"])
	}
	if payload["message"] != "Deepgram connection lost" {
		t.Fatalf("expected message=Deepgram connection lost, got %v", payload["message"])
	}
	if payload["version"] == nil {
		t.Fatalf("missing version in payload: %s", string(b))
	}
	if payload["timestamp"] == nil {
		t.Fatalf("missing timestamp in payload: %s", string(b))
	}
}

func TestComponentStatusEventAllComponents(t *testing.T) {
	components := []struct {
		component string
		status    string
		message   string
	}{
		{"deepgram", "connected", "Deepgram connection established"},
		{"deepgram", "disconnected", "Deepgram connection lost"},
		{"deepgram", "reconnecting", "Attempting to reconnect to Deepgram"},
		{"summary", "error", "Failed to generate summary"},
		{"sync", "error", "Google Drive sync failed"},
		{"mic", "error", "Microphone disconnected"},
	}

	for _, tc := range components {
		t.Run(tc.component+"_"+tc.status, func(t *testing.T) {
			event := ComponentStatusEvent{
				Event:     newEvent("component_status", time.Now()),
				Component: tc.component,
				Status:    tc.status,
				Message:   tc.message,
			}

			b, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}

			var payload map[string]any
			if err := json.Unmarshal(b, &payload); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}

			if payload["component"] != tc.component {
				t.Fatalf("expected component=%s, got %v", tc.component, payload["component"])
			}
			if payload["status"] != tc.status {
				t.Fatalf("expected status=%s, got %v", tc.status, payload["status"])
			}
			if payload["message"] != tc.message {
				t.Fatalf("expected message=%s, got %v", tc.message, payload["message"])
			}
		})
	}
}

func TestErrorBroadcast(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	hub.BroadcastComponentStatus("deepgram", "disconnected", "Connection lost")

	select {
	case msg := <-ch:
		var payload map[string]any
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if payload["type"] != "component_status" {
			t.Fatalf("expected type=component_status, got %v", payload["type"])
		}
		if payload["component"] != "deepgram" {
			t.Fatalf("expected component=deepgram, got %v", payload["component"])
		}
		if payload["status"] != "disconnected" {
			t.Fatalf("expected status=disconnected, got %v", payload["status"])
		}
		if payload["message"] != "Connection lost" {
			t.Fatalf("expected message=Connection lost, got %v", payload["message"])
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for component_status broadcast")
	}
}

func TestErrorBroadcastMultipleSubscribers(t *testing.T) {
	hub := NewHub()
	ch1 := hub.Subscribe()
	ch2 := hub.Subscribe()
	defer hub.Unsubscribe(ch1)
	defer hub.Unsubscribe(ch2)

	hub.BroadcastComponentStatus("summary", "error", "LLM call failed")

	for i, ch := range []chan []byte{ch1, ch2} {
		select {
		case msg := <-ch:
			var payload map[string]any
			if err := json.Unmarshal(msg, &payload); err != nil {
				t.Fatalf("subscriber %d: unmarshal failed: %v", i, err)
			}
			if payload["component"] != "summary" {
				t.Fatalf("subscriber %d: expected component=summary, got %v", i, payload["component"])
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d: timeout waiting for broadcast", i)
		}
	}
}
