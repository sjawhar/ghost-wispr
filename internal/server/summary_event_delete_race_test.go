package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deleteAfterSummaryCommit is the store a summary is written to, which
// deletes the session as soon as the summary commits: a whole-conversation
// delete that lands between the summary's commit and the hub storing its
// event.
type deleteAfterSummaryCommit struct {
	*storage.SQLiteStore
	t *testing.T
}

func (s deleteAfterSummaryCommit) UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model string, generation int) error {
	if err := s.SQLiteStore.UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model, generation); err != nil {
		return err
	}
	if _, err := s.DeleteSession(sessionID, s.t.TempDir()); err != nil {
		s.t.Fatalf("delete the session after its summary committed: %v", err)
	}
	return nil
}

type fixedSummarizer struct{}

func (fixedSummarizer) Summarize(context.Context, string, string) (string, string, string, string, error) {
	return "Warehouse robot naming", "The team named the project zyxquorbelfin.", "default", "{}", nil
}

// TestASummaryEventAfterTheSessionsDeleteIsNotStored: a summary commits,
// then the hub broadcasts it and stores its event. When the session is
// deleted between the two, its summary must not be left in the stored events,
// which /api/events serves for seven days.
func TestASummaryEventAfterTheSessionsDeleteIsNotStored(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "20261116080201"
	started := time.Date(2026, 11, 16, 8, 2, 1, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: "We named the warehouse robot project and agreed to order a machine to clear the yard before the end of the month.", StartTime: 0, EndTime: 9, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.SetEventStore(store)
	manager := session.NewManager(deleteAfterSummaryCommit{SQLiteStore: store, t: t}, nil, fixedSummarizer{}, hub, session.NewDetector(time.Hour), 0)
	if err := manager.SummarizeSession(context.Background(), id, started); err != nil {
		t.Fatalf("SummarizeSession: %v", err)
	}

	if _, err := store.GetSession(id); err == nil {
		t.Fatal("control: the hook did not delete the session")
	}
	events, err := store.GetEventsSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Payload, `"session_id":"`+id+`"`) {
			t.Errorf("event %d (%s) of the deleted session is stored: %s", e.ID, e.EventType, e.Payload)
		}
	}
}
