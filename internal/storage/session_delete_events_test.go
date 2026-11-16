package storage

import (
	"strings"
	"testing"
	"time"
)

// TestDeleteSessionRemovesItsStoredEvents: deleting a conversation from the unit removes the
// events that carry its text. On the office Pi on 2026-11-16, DELETE /api/sessions/{id} answered
// 204 and the session read back 404. But /api/events still served its summary_ready payloads
// (the full summary) and its live_transcript events, and the database file and the
// pre-migration backup still held its summary. Slice 3's precheck then refused a new run with
// "a copy of a phrase is already on the unit", whose own remedy is to delete that conversation.
func TestDeleteSessionRemovesItsStoredEvents(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 16, 7, 50, 0, 0, time.UTC)
	for _, id := range []string{"gone", "kept"} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	events := []struct{ eventType, payload string }{
		{"live_transcript", `{"type":"live_transcript","session_id":"gone","text":"Purple elephant seventeen."}`},
		{"summary_ready", `{"type":"summary_ready","session_id":"gone","summary":"The team named the project Purple Elephant 17."}`},
		{"summary_ready", `{"type":"summary_ready","session_id":"kept","summary":"Another conversation."}`},
	}
	for _, e := range events {
		if err := store.StoreEvent(e.eventType, e.payload); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.DeleteSession("gone", ""); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	left, err := store.GetEventsSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	keptSeen := false
	for _, e := range left {
		if strings.Contains(e.Payload, `"session_id":"gone"`) || strings.Contains(e.Payload, "Purple") {
			t.Errorf("event %d (%s) of the deleted session is still stored: %s", e.ID, e.EventType, e.Payload)
		}
		if strings.Contains(e.Payload, `"session_id":"kept"`) {
			keptSeen = true
		}
	}
	if !keptSeen {
		t.Error("the other session's event was removed too")
	}
}
