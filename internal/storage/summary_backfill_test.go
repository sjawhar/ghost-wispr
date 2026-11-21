package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// backfillStates is a store with one ended session in each summary state a
// backfill meets, oldest first: pending, running (being made now), failed,
// completed with a summary, and completed empty.
func backfillStates(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a-pending", "b-running", "c-failed", "d-completed", "e-empty"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: "We talked.", StartTime: 0, EndTime: 1, Timestamp: at}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"b-running", "c-failed", "d-completed", "e-empty"} {
		if err := store.StartSummary(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.FailSummaryAtGeneration("c-failed", "summarization failed: model unavailable (503)", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("d-completed", "Talk", "They talked.", SummaryCompleted, "default", "{}", "gemini-x", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("e-empty", "", "", SummaryCompleted, "", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestABackfillLeavesASummaryBeingMadeNow fails if GetSessionsForBackfill
// takes a session whose summary is running: in this run of the app that
// summary is being made now (one the last run left running is pending again
// from the start, RecoverInterruptedSummaries), and POST
// /api/sessions/batch-backfill during the start's backlog summarized the
// same session twice. It takes a pending, failed or completed empty summary.
func TestABackfillLeavesASummaryBeingMadeNow(t *testing.T) {
	store := backfillStates(t)
	got, err := store.GetSessionsForBackfill()
	if want := []string{"a-pending", "c-failed", "e-empty"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("GetSessionsForBackfill = %v, %v; want %v", got, err, want)
	}
}

// TestASummaryTheLastRunLeftRunningIsTakenAtTheStart fails if a summary the
// last run of the app left running (it stopped during it) is not pending
// again after RecoverInterruptedSummaries, which the start runs before any
// summary, or is then not in the start's backlog (GetSessionsForBackfill):
// the start finishes it. It returns the sessions it recovered, and keeps
// every other summary state.
func TestASummaryTheLastRunLeftRunningIsTakenAtTheStart(t *testing.T) {
	store := backfillStates(t)
	recovered, err := store.RecoverInterruptedSummaries()
	if err != nil || !slices.Equal(recovered, []string{"b-running"}) {
		t.Fatalf("RecoverInterruptedSummaries = %v, %v; want [b-running]", recovered, err)
	}
	for id, want := range map[string]string{"a-pending": SummaryPending, "b-running": SummaryPending, "c-failed": SummaryFailed, "d-completed": SummaryCompleted, "e-empty": SummaryCompleted} {
		if sess, err := store.GetSession(id); err != nil || sess.SummaryStatus != want {
			t.Errorf("session %s: summary status %q, %v; want %q", id, sess.SummaryStatus, err, want)
		}
	}
	got, err := store.GetSessionsForBackfill()
	if want := []string{"a-pending", "b-running", "c-failed", "e-empty"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("GetSessionsForBackfill after the recovery = %v, %v; want %v", got, err, want)
	}
}

// TestABackfillStartsOnlyASummaryItCanTake fails if StartBackfillSummary
// starts a summary that is running (another run makes it now) or completed
// with text (made since the backlog listed it), or refuses a pending, failed
// or completed empty one. A backlog starts each session only through it, so
// two backlogs listing the same session make its summary once.
func TestABackfillStartsOnlyASummaryItCanTake(t *testing.T) {
	store := backfillStates(t)
	for id, takes := range map[string]bool{"a-pending": true, "b-running": false, "c-failed": true, "d-completed": false, "e-empty": true} {
		err := store.StartBackfillSummary(id)
		switch {
		case takes && err != nil:
			t.Errorf("StartBackfillSummary(%s) = %v; want the summary started", id, err)
		case !takes && !errors.Is(err, sql.ErrNoRows):
			t.Errorf("StartBackfillSummary(%s) = %v; want %v, the summary left as it is", id, err, sql.ErrNoRows)
		}
		if takes {
			if sess, _ := store.GetSession(id); sess.SummaryStatus != SummaryRunning {
				t.Errorf("session %s: summary status %q after the start; want %q", id, sess.SummaryStatus, SummaryRunning)
			}
			if err := store.StartBackfillSummary(id); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("a second StartBackfillSummary(%s) = %v; want %v: the first one makes it now", id, err, sql.ErrNoRows)
			}
		}
	}
	if sess, _ := store.GetSession("d-completed"); sess.SummaryStatus != SummaryCompleted || sess.Summary != "They talked." {
		t.Errorf("the completed session reads %q (%s) after a refused start; want its summary as it was", sess.Summary, sess.SummaryStatus)
	}
}
