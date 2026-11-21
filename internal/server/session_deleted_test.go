package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// readUntil reads the messages ch delivers until one has type until, and
// returns them all; it fails the test if none arrives within a second.
func readUntil(t *testing.T, next func() ([]byte, bool), until string) [][]byte {
	t.Helper()
	var got [][]byte
	for {
		msg, ok := next()
		if !ok {
			t.Fatalf("no %s event arrived; got %d events: %s", until, len(got), joinPayloads(got))
		}
		got = append(got, msg)
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &event); err != nil {
			t.Fatalf("decode %s: %v", msg, err)
		}
		if event.Type == until {
			return got
		}
	}
}

func joinPayloads(payloads [][]byte) string {
	s := make([]string, len(payloads))
	for i, p := range payloads {
		s[i] = string(p)
	}
	return strings.Join(s, " ")
}

// TestAStatusSentAfterItsSessionIsDeletedReachesNoClient: work that runs at a
// delete (a refinement, a summary, a Drive sync) reports on its session after
// the row is gone. No client is sent that status, not one already connected
// either, and no client that connects later.
func TestAStatusSentAfterItsSessionIsDeletedReachesNoClient(t *testing.T) {
	const gone = "20261116140449"
	store := statusTestStore(t, gone)
	hub := NewHub()
	hub.SetEventStore(store)
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	if _, err := store.DeleteSession(gone, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	hub.BroadcastSessionComponentStatus(gone, "refinement", storage.ComponentStatusError, "Batch refinement failed for session "+gone)
	hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone reconnected")

	next := func() ([]byte, bool) {
		select {
		case msg := <-ch:
			return msg, true
		case <-time.After(time.Second):
			return nil, false
		}
	}
	// The mic status, sent after it, is the control: the client hears.
	for _, msg := range readUntil(t, next, "component_status") {
		if strings.Contains(string(msg), gone) {
			t.Errorf("a connected client is sent a status of deleted session %s: %s", gone, msg)
		}
	}
	for _, status := range hub.SnapshotComponentStatuses() {
		if status.SessionID == gone {
			t.Errorf("the hub keeps a status of deleted session %s for clients that connect: %+v", gone, status)
		}
	}
}

// TestDeletingASessionTellsEveryConnectedClient: DELETE /api/sessions/{id}
// tells every connected web client which sessions it deleted (the session and
// those merged into it), by id only, so each tab drops their title, summary
// and segments.
func TestDeletingASessionTellsEveryConnectedClient(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	started := time.Date(2026, 11, 16, 14, 4, 49, 0, time.UTC)
	for i, id := range []string{"20261116140449", "20261116141316"} {
		at := started.Add(time.Duration(i) * 10 * time.Minute)
		recording := filepath.Join(audioDir, id+".mp3")
		if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(5*time.Minute), recording, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateTitle(id, "Purple elephant "+id); err != nil {
			t.Fatal(err)
		}
	}
	const merged = "20261116140449-merged"
	if err := store.MergeSessions(merged, []string{"20261116140449", "20261116141316"}, started, started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cfgStore := newTestConfigStore(t)
	if err := cfgStore.Update(func(cfg *config.Config) { cfg.AudioDir = audioDir }); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.SetEventStore(store)
	handler, err := Handler(testStaticFS(t), hub, store, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	var clients []*websocket.Conn
	for range 2 {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatalf("dial websocket: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		clients = append(clients, conn)
	}
	// Each client is connected: the connection event arrived.
	for _, conn := range clients {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("control: read the connection event: %v", err)
		}
	}

	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/sessions/"+merged, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", response.StatusCode)
	}

	for i, conn := range clients {
		next := func() ([]byte, bool) {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, msg, err := conn.ReadMessage()
			return msg, err == nil
		}
		events := readUntil(t, next, "session_deleted")
		var deleted struct {
			SessionID        string   `json:"session_id"`
			SourceSessionIDs []string `json:"source_session_ids"`
		}
		last := events[len(events)-1]
		if err := json.Unmarshal(last, &deleted); err != nil {
			t.Fatal(err)
		}
		if deleted.SessionID != merged || strings.Join(deleted.SourceSessionIDs, ",") != "20261116140449,20261116141316" {
			t.Errorf("client %d: session_deleted = %s, want %s and its two sources", i, last, merged)
		}
		if strings.Contains(strings.ToLower(string(last)), "purple") {
			t.Errorf("client %d: session_deleted carries the title: %s", i, last)
		}
	}
}
