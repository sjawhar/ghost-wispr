package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestWSBroadcastEventShape(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	hub.BroadcastLiveTranscript("session-1", transcribe.Segment{
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
		if payload["session_id"] != "session-1" {
			t.Fatalf("expected session_id in payload, got %#v", payload["session_id"])
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

// TestWSDeliversInitialPausedStatus fails if a status broadcast before clients
// connect is lost, or if it replays an older status.
func TestWSDeliversInitialPausedStatus(t *testing.T) {
	hub := NewHub()
	hub.BroadcastStatusChanged(true)
	hub.BroadcastStatusChanged(false)
	mux := http.NewServeMux()
	registerWSRoute(mux, hub, slog.Default())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set websocket deadline: %v", err)
	}

	for range 2 {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket message: %v", err)
		}
		var event struct {
			Type   string `json:"type"`
			Paused bool   `json:"paused"`
		}
		if err := json.Unmarshal(message, &event); err != nil {
			t.Fatalf("decode websocket event: %v", err)
		}
		if event.Type == "status_changed" {
			if event.Paused {
				t.Fatal("initial status event said paused, want resumed")
			}
			return
		}
	}
	t.Fatal("websocket did not receive the initial paused status")
}
