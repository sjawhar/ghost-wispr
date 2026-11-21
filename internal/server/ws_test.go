package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestWSSubscribesBeforeItSaysConnected fails if a socket sends its
// connection frame before it hears the hub's events. The page reads its
// joined agents and questions again on that frame, so an event published
// after that read but before the subscription reaches neither.
func TestWSSubscribesBeforeItSaysConnected(t *testing.T) {
	hub := NewHub()
	mux := http.NewServeMux()
	registerWSRoute(mux, hub, slog.Default())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// The hub's lock holds Subscribe back. A socket that sends its frame
	// first sends it while the lock is held, with no subscription yet.
	hub.mu.Lock()
	var unlocked sync.Once
	unlock := func() { unlocked.Do(hub.mu.Unlock) }
	t.Cleanup(unlock)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	messages := make(chan []byte, 8)
	go func() {
		defer close(messages)
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messages <- message
		}
	}()

	// A socket that subscribes first sends nothing while the lock is held;
	// one that sends its frame first sends it at once.
	select {
	case message := <-messages:
		t.Fatalf("the socket sent %s with %d subscriptions to the hub: an event published now never reaches it", message, len(hub.clients))
	case <-time.After(500 * time.Millisecond):
	}
	unlock()

	readType := func() string {
		t.Helper()
		select {
		case message, ok := <-messages:
			if !ok {
				t.Fatal("websocket closed")
			}
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(message, &event); err != nil {
				t.Fatalf("decode websocket event: %v", err)
			}
			return event.Type
		case <-time.After(2 * time.Second):
			t.Fatal("websocket sent nothing")
		}
		return ""
	}
	if got := readType(); got != "connection" {
		t.Fatalf("first websocket event = %q, want connection", got)
	}
	hub.BroadcastSessionStarted("session-1")
	if got := readType(); got != "session_started" {
		t.Fatalf("websocket event after the connection frame = %q, want session_started", got)
	}
}
