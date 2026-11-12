package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestReview3NoSummarizerCompletesSummaryAfterDelete proves that the
// no-summarizer path aligns the summary generation before completing it after
// a segment deletion.
func TestReview3NoSummarizerCompletesSummaryAfterDelete(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "review3.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"before bridge", "private deletion token", "after bridge"} {
		if err := store.AppendSegment("s", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("s", started.Add(3*time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil, nil, nil, 0)
	manager.generateSummary(context.Background(), "s", started)

	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("s", segments[1].ID, storage.DeletedSpan{AudioPath: "recording.mp3", Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	manager.generateSummary(context.Background(), "s", started)

	after, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if after.SummaryStatus != storage.SummaryCompleted && after.SummaryStatus != storage.SummaryFailed {
		t.Errorf("after a deletion with no summarizer, summary_status = %q: the Drive replacement waits forever", after.SummaryStatus)
	}
}

// noSummarizerStore opens a real store holding one ended session with two
// segments.
func noSummarizerStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "no-summarizer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"kept words", "deleted words"} {
		if err := store.AppendSegment("s", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("s", started.Add(2*time.Second), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestNoSummarizerDoesNotRepublishAnOldSummary fails if regenerating the
// summary of a session whose summary a summarizer made earlier, and Envoy
// already has, sends that same summary to Envoy again at a new version once
// no summarizer is configured.
func TestNoSummarizerDoesNotRepublishAnOldSummary(t *testing.T) {
	store := noSummarizerStore(t)
	if err := store.UpdateSummaryAtGeneration("s", "Title", "The summary a summarizer made.", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateEnvoyPublishStatusAtVersion("s", before.SummaryVersion, storage.EnvoyPublishPublished); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil, nil, nil, 0)

	manager.generateSummary(context.Background(), "s", time.Now().UTC())

	after, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if after.SummaryStatus != storage.SummaryCompleted || after.Summary != before.Summary {
		t.Fatalf("summary after a regeneration with no summarizer = %q (%q), want the earlier one kept, completed", after.Summary, after.SummaryStatus)
	}
	if after.SummaryVersion != before.SummaryVersion || after.EnvoyPublishStatus != storage.EnvoyPublishPublished {
		t.Errorf("summary version %d -> %d, envoy %q: the unchanged summary was queued for Envoy again", before.SummaryVersion, after.SummaryVersion, after.EnvoyPublishStatus)
	}
	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("sessions needing an Envoy publish = %v, want none", ids)
	}
}

// TestNoSummarizerCanonicalizeFailureEndsTheSummary fails if a canonicalize
// error leaves the summary running, which holds the Drive replacement forever.
func TestNoSummarizerCanonicalizeFailureEndsTheSummary(t *testing.T) {
	store := noSummarizerStore(t)
	if _, err := store.DB().Exec(`CREATE TRIGGER refuse_canonical BEFORE UPDATE OF canonical_transcript ON sessions
		BEGIN SELECT RAISE(ABORT, 'canonical transcript refused'); END`); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil, nil, nil, 0)

	manager.generateSummary(context.Background(), "s", time.Now().UTC())

	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if session.SummaryStatus != storage.SummaryFailed {
		t.Errorf("summary_status after canonicalize failure = %q, want %q", session.SummaryStatus, storage.SummaryFailed)
	}
}

// TestNoSummarizerRefusedCompletionStillSyncs fails if a refused empty-summary
// write skips the Drive sync that the no-summarizer path owes.
func TestNoSummarizerRefusedCompletionStillSyncs(t *testing.T) {
	store := noSummarizerStore(t)
	if _, err := store.DB().Exec(`CREATE TRIGGER refuse_completion BEFORE UPDATE OF summary_status ON sessions
		WHEN NEW.summary_status = 'completed'
		BEGIN SELECT RAISE(ABORT, 'summary completion refused'); END`); err != nil {
		t.Fatal(err)
	}
	synced := make(chan string, 1)
	manager := NewManager(store, nil, nil, nil, nil, 0)
	manager.SetSyncer(syncerMock{called: synced})

	manager.generateSummary(context.Background(), "s", time.Now().UTC())

	select {
	case sessionID := <-synced:
		if sessionID != "s" {
			t.Errorf("synced session %q, want s", sessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a refused empty-summary write skipped the Drive sync")
	}
}
