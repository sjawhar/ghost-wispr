package session

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestASessionEndsSummaryWhoseEndWasNotStoredIsTakenAgainInThisRun fails if
// the summary a session's end makes (Manager.SummarizeSession), whose
// summary and failure the store both refused, leaves the session's summary
// running with no worker on it: a backlog of this run then leaves it out
// (storage.SQLiteStore.GetSessionsForBackfill) until a restart. It must go
// back to pending, which the backlog takes.
func TestASessionEndsSummaryWhoseEndWasNotStoredIsTakenAgainInThisRun(t *testing.T) {
	const id = "20261122100000"
	started := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(id, transcribe.Segment{Text: "Morning, Ben here. We move the launch to Friday.", StartTime: 0, EndTime: 3, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), "", ""); err != nil {
		t.Fatal(err)
	}
	// The store refuses every write that ends a summary, as a full disk would.
	if _, err := store.DB().Exec(`CREATE TEMP TRIGGER test_refuse_summary_end BEFORE UPDATE OF summary_status ON sessions
		WHEN NEW.summary_status IN ('completed', 'failed') BEGIN SELECT RAISE(ABORT, 'planted: the store refused the summary''s end'); END`); err != nil {
		t.Fatal(err)
	}
	summarizer := summary.New(config.Summarization{Model: "gemini/gemini-4-carbon", Presets: map[string]config.Preset{"default": {
		Description: "general", UserTemplate: "{{transcript}}",
	}}}, func(_, _ string) (llm.Client, error) { return &promptRecorder{}, nil })
	manager := NewManager(store, nil, summarizer, &hubMock{}, NewDetector(time.Hour), 0)
	if err := manager.SummarizeSession(context.Background(), id, started); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DROP TRIGGER test_refuse_summary_end`); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.SummaryStatus != storage.SummaryPending {
		t.Fatalf("after a summary whose end the store refused, the session's summary is %s; want it pending, for a backlog of this run", got.SummaryStatus)
	}
	if ids, err := store.GetSessionsForBackfill(); err != nil || !slices.Contains(ids, id) {
		t.Fatalf("a backlog of this run takes %v (%v); want session %s", ids, err, id)
	}
}
