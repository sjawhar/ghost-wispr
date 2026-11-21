package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type refinedOnce struct{}

func (refinedOnce) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	words := []transcribe.TimestampedWord{}
	for i, w := range strings.Fields("We named the warehouse robot project.") {
		words = append(words, transcribe.TimestampedWord{Text: w, Start: float64(i) * 0.4, End: float64(i)*0.4 + 0.3})
	}
	return transcribe.BatchResult{Transcript: "We named the warehouse robot project.", Model: "nova-3", Words: words}, nil
}

// TestADeletedSessionLeavesNoStatusEventNamingIt: the session manager announces
// a session's refinement and sync as component_status events ("Batch refinement
// completed for session <id>"), and the hub stores them for /api/events. After
// the session is deleted, no stored event may name it. On the office Pi on
// 2026-11-16, after DELETE /api/sessions/{id} of 20261116140449 and
// 20261116141316 (each 204, then 404), /api/events still served ten such events
// naming them, and eighteen naming five earlier deleted test sessions, through
// the start-up sweep of a867073c: those events carry the id only in their
// message, not as session_id, so the delete's match on payload.session_id
// misses them.
func TestADeletedSessionLeavesNoStatusEventNamingIt(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "20261116140449"
	started := time.Date(2026, 11, 16, 14, 4, 49, 0, time.UTC)
	recording := filepath.Join(t.TempDir(), id+".mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: "We named the warehouse robot project.", StartTime: 0, EndTime: 3, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording, ""); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.SetEventStore(store)
	manager := session.NewManager(store, nil, nil, hub, session.NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(refinedOnce{})
	if err := manager.RefineSession(context.Background(), id, recording, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}

	named := func() []storage.StoredEvent {
		t.Helper()
		events, err := store.GetEventsSince(0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		var out []storage.StoredEvent
		for _, e := range events {
			if strings.Contains(e.Payload, id) {
				out = append(out, e)
			}
		}
		return out
	}
	if len(named()) == 0 {
		t.Fatal("control: no stored event names the session before the delete, so its absence after proves nothing")
	}

	if _, err := store.DeleteSession(id, t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	for _, e := range named() {
		t.Errorf("event %d (%s) still names deleted session %s: %s", e.ID, e.EventType, id, e.Payload)
	}
}
