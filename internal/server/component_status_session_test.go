package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// statusTestStore opens a store with an ended session for each of ids, for
// the hub to store its events in.
func statusTestStore(t *testing.T, ids ...string) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 16, 14, 4, 49, 0, time.UTC)
	for _, id := range ids {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// statusPayload decodes a component_status event's component and session_id.
func statusPayload(t *testing.T, payload []byte) (component string, sessionID *string) {
	t.Helper()
	var event struct {
		Type      string  `json:"type"`
		Component string  `json:"component"`
		SessionID *string `json:"session_id"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	if event.Type != "component_status" {
		t.Fatalf("event %s is not a component_status", payload)
	}
	return event.Component, event.SessionID
}

// TestASessionsStatusEventCarriesItsSessionID: a component status about one
// session (its refinement, summary or sync) carries the session's id as
// session_id, in the broadcast and in the stored event, so a delete of the
// session finds it; a status of the whole unit (the microphone) carries none.
func TestASessionsStatusEventCarriesItsSessionID(t *testing.T) {
	const id = "20261116140449"
	store := statusTestStore(t, id)
	hub := NewHub()
	hub.SetEventStore(store)
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	hub.BroadcastSessionComponentStatus(id, "sync", storage.ComponentStatusConnected, "Google Drive synced for session "+id)
	hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone reconnected")

	want := map[string]string{"sync": id, "mic": ""}
	check := func(source string, payload []byte) {
		t.Helper()
		component, sessionID := statusPayload(t, payload)
		got := ""
		if sessionID != nil {
			got = *sessionID
			if got == "" {
				t.Errorf("%s: the %s status has an empty session_id: %s", source, component, payload)
			}
		}
		if got != want[component] {
			t.Errorf("%s: the %s status has session_id %q, want %q: %s", source, component, got, want[component], payload)
		}
	}
	for range want {
		select {
		case msg := <-ch:
			check("broadcast", msg)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for a component_status broadcast")
		}
	}
	events, err := store.GetEventsSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(want) {
		t.Fatalf("stored %d events, want %d: %v", len(events), len(want), events)
	}
	for _, e := range events {
		check("stored", []byte(e.Payload))
	}
}

// initialStatuses connects a WebSocket client to hub and returns the
// component statuses it is sent as it connects (the snapshot), which end at
// the pause status the hub then sends.
func initialStatuses(t *testing.T, hub *Hub) [][]byte {
	t.Helper()
	mux := http.NewServeMux()
	registerWSRoute(mux, hub, slog.Default())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var statuses [][]byte
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket message: %v", err)
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(message, &event); err != nil {
			t.Fatalf("decode websocket event: %v", err)
		}
		switch event.Type {
		case "component_status":
			statuses = append(statuses, message)
		case "status_changed":
			return statuses
		}
	}
}

// TestANewClientIsNotSentADeletedSessionsStatus: the hub keeps each
// component's last status and sends it to every client as it connects. Once
// a session is deleted, a client that connects is sent no status about it,
// also not one a refinement that finished after the delete broadcast; the
// status of the unit's microphone, and of a session that stays, are sent.
func TestANewClientIsNotSentADeletedSessionsStatus(t *testing.T) {
	const gone, kept = "20261116140449", "20261116141316"
	store := statusTestStore(t, gone, kept)
	hub := NewHub()
	hub.SetEventStore(store)
	hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone reconnected")
	hub.BroadcastSessionComponentStatus(kept, "sync", storage.ComponentStatusConnected, "Google Drive synced for session "+kept)
	hub.BroadcastSessionComponentStatus(gone, "refinement", storage.ComponentStatusConnected, "Batch refinement completed for session "+gone)
	hub.BroadcastStatusChanged(false)

	sent := func() map[string]string {
		t.Helper()
		bySession := map[string]string{}
		for _, payload := range initialStatuses(t, hub) {
			component, sessionID := statusPayload(t, payload)
			if sessionID == nil {
				bySession[component] = ""
			} else {
				bySession[component] = *sessionID
			}
		}
		return bySession
	}
	if got := sent(); got["refinement"] != gone {
		t.Fatalf("control: before the delete a new client is sent %v, want the refinement of %s", got, gone)
	}

	if _, err := store.DeleteSession(gone, t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got, want := sent(), map[string]string{"mic": "", "sync": kept}; len(got) != len(want) || got["mic"] != "" || got["sync"] != kept {
		t.Errorf("after the delete a new client is sent %v, want %v", got, want)
	}

	hub.BroadcastSessionComponentStatus(gone, "refinement", storage.ComponentStatusError, "Batch refinement failed for session "+gone)
	if got := sent(); len(got) != 2 {
		t.Errorf("after a late status of the deleted session a new client is sent %v, want only mic and sync", got)
	}
}
