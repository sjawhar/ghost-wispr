package storage

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// resumes returns the sessions whose refinement the next start runs again,
// and those whose automatic run a stop cut off too, failing the test on an
// error.
func resumes(t *testing.T, store *SQLiteStore) (pending, exhausted []string) {
	t.Helper()
	pending, exhausted, err := store.RefinementResumes()
	if err != nil {
		t.Fatalf("RefinementResumes: %v", err)
	}
	return pending, exhausted
}

func expectResumes(t *testing.T, store *SQLiteStore, what string, pending, exhausted []string) {
	t.Helper()
	gotPending, gotExhausted := resumes(t, store)
	if !slices.Equal(gotPending, pending) || !slices.Equal(gotExhausted, exhausted) {
		t.Fatalf("%s: pending %v and exhausted %v, want pending %v and exhausted %v", what, gotPending, gotExhausted, pending, exhausted)
	}
}

func recoverRefinements(t *testing.T, store *SQLiteStore) {
	t.Helper()
	if err := store.RecoverStaleRefinements(); err != nil {
		t.Fatalf("RecoverStaleRefinements: %v", err)
	}
}

func claimResume(t *testing.T, store *SQLiteStore, id string) bool {
	t.Helper()
	claimed, err := store.ClaimRefinementResume(id)
	if err != nil {
		t.Fatalf("ClaimRefinementResume(%s): %v", id, err)
	}
	return claimed
}

// TestAnAutomaticRefinementRetryIsClaimedOnceAcrossRestarts fails if a
// refinement whose automatic retry was itself cut off by a stop is retried
// automatically again. A refinement that crashes or hangs the process would
// otherwise be retried at every restart, for ever. The retry is recorded
// before it starts, so the start after a crash in it finds it, marks it
// exhausted, and offers it to nothing but a person's Retry; a Retry, which
// claims the refinement, ends that.
func TestAnAutomaticRefinementRetryIsClaimedOnceAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	store := newStoreAt(t, path)
	endedWith(t, store, "20261120080131", "/audio/a.mp3", RefinementRunning)

	recoverRefinements(t, store) // the start after the stop
	expectResumes(t, store, "after the stop", []string{"20261120080131"}, nil)
	if !claimResume(t, store, "20261120080131") {
		t.Fatal("the automatic retry of a refinement a stop cut off was not claimed")
	}
	if claimResume(t, store, "20261120080131") {
		t.Fatal("the automatic retry was claimed twice")
	}
	// The retry claims the refinement, and the process dies inside it.
	if _, claimed, err := store.ClaimRefinement("20261120080131", "/audio/a.mp3"); err != nil || !claimed {
		t.Fatalf("the retry's ClaimRefinement: claimed=%t, %v", claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = newStoreAt(t, path)
	recoverRefinements(t, store) // the start after the crash
	expectResumes(t, store, "after the crash in the automatic retry", nil, []string{"20261120080131"})
	expectAudio(t, store, "20261120080131", "/audio/a.mp3", "", RefinementFailed)
	if claimResume(t, store, "20261120080131") {
		t.Fatal("an automatic retry a stop cut off was claimed again")
	}
	recoverRefinements(t, store) // and every start after it
	expectResumes(t, store, "at a third start", nil, []string{"20261120080131"})

	// A person presses Retry: its claim ends the exhaustion.
	if _, claimed, err := store.ClaimRefinement("20261120080131", "/audio/a.mp3"); err != nil || !claimed {
		t.Fatalf("the pressed Retry's ClaimRefinement: claimed=%t, %v", claimed, err)
	}
	expectResumes(t, store, "after a pressed Retry", nil, nil)
}

// TestAnUnstartedAutomaticRetrySurvivesAnotherStop fails if a stop before the
// start reaches a refinement's automatic retry loses it: recovery marks the
// refinement failed, and only what the store keeps says it is owed a retry.
func TestAnUnstartedAutomaticRetrySurvivesAnotherStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	store := newStoreAt(t, path)
	endedWith(t, store, "20261120080131", "/audio/a.mp3", RefinementRunning)
	endedWith(t, store, "20261120081500", "/audio/b.mp3", RefinementRunning)
	recoverRefinements(t, store)
	if !claimResume(t, store, "20261120080131") {
		t.Fatal("the first automatic retry was not claimed")
	}
	// Stopped during the first retry, before the second: the first leaves
	// its claim behind, the second its pending retry.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = newStoreAt(t, path)
	recoverRefinements(t, store)
	expectResumes(t, store, "after a stop during the first retry", []string{"20261120081500"}, []string{"20261120080131"})
	expectAudio(t, store, "20261120081500", "/audio/b.mp3", "", RefinementFailed)
}

// TestAFinishedAutomaticRetryOwesNothing fails if a finished automatic retry
// is retried again or reported exhausted: neither once its end is recorded,
// nor when the process stopped after the refinement completed and before that.
func TestAFinishedAutomaticRetryOwesNothing(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261120080131", "/audio/a.mp3", RefinementRunning)
	endedWith(t, store, "20261120081500", "/audio/b.mp3", RefinementRunning)
	recoverRefinements(t, store)
	for _, id := range []string{"20261120080131", "20261120081500"} {
		if !claimResume(t, store, id) {
			t.Fatalf("the automatic retry of %s was not claimed", id)
		}
		if err := store.UpdateRefinementAtGeneration(id, "", RefinementCompleted, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishRefinementResume("20261120080131"); err != nil {
		t.Fatal(err)
	}
	// The second completed, and the process stopped before its end was
	// recorded.
	recoverRefinements(t, store)
	expectResumes(t, store, "after two completed retries", nil, nil)
	expectAudio(t, store, "20261120081500", "/audio/b.mp3", "", RefinementCompleted)
}

// TestAnAutomaticRetryIsNotClaimedOverAPersonsRetry fails if the start's
// automatic retry runs a refinement a person's Retry has run since the stop,
// or keeps owing it once it cannot run it.
func TestAnAutomaticRetryIsNotClaimedOverAPersonsRetry(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261120080131", "/audio/a.mp3", RefinementRunning)
	recoverRefinements(t, store)
	// The person's Retry runs and completes before the start reaches it.
	if _, claimed, err := store.ClaimRefinement("20261120080131", "/audio/a.mp3"); err != nil || !claimed {
		t.Fatalf("the pressed Retry's ClaimRefinement: claimed=%t, %v", claimed, err)
	}
	expectResumes(t, store, "once a pressed Retry claimed the refinement", nil, nil)
	if claimResume(t, store, "20261120080131") {
		t.Fatal("the automatic retry was claimed over a person's Retry")
	}
}

// TestADeletionWhileARetryIsOwedCancelsIt fails if a "Delete text" of an
// untimed segment, made after the start found the refinement a stop cut off
// and before it retried it, leaves that retry owed. The same deletion made
// while the refinement still ran fails it, so the start owes it nothing; one
// made while its retry is owed must end the retry the same way. The control
// session, not deleted from, keeps its retry.
func TestADeletionWhileARetryIsOwedCancelsIt(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "20261120080131", "/audio/a.mp3", RefinementRunning)
	endedWith(t, store, "20261120081500", "/audio/b.mp3", RefinementRunning)
	if err := store.AppendSegment("20261120080131", transcribe.Segment{Text: "untimed synthetic words", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE segments SET untimed = 1 WHERE session_id = ?`, "20261120080131"); err != nil {
		t.Fatal(err)
	}
	recoverRefinements(t, store)
	segments, err := store.GetSegments("20261120080131")
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments %v: %v", segments, err)
	}
	if err := store.DeleteRefinedSegmentText("20261120080131", segments[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if claimResume(t, store, "20261120080131") {
		t.Fatal("the automatic retry was claimed after a deletion made while it was owed")
	}
	if !claimResume(t, store, "20261120081500") {
		t.Fatal("the control session's automatic retry was not claimed")
	}
	expectResumes(t, store, "after the deletion", nil, nil)
}

// TestAnOwedRetrySurvivesGC fails if GC, which may take an owed session's
// synced recording before the start retries it, ends the retry: the
// refinement becomes no_audio, and the Retry action restores the recording
// from Drive and runs it.
func TestAnOwedRetrySurvivesGC(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedAndSynced(t, store, "20261120080131", "/audio/a.mp3", RefinementRunning)
	recoverRefinements(t, store)
	if _, changed, err := store.MarkAudioRemoved("20261120080131", "/audio/a.mp3", "", false, time.Now()); err != nil || !changed {
		t.Fatalf("GC's mark: changed=%t, %v", changed, err)
	}
	if got, err := store.GetSession("20261120080131"); err != nil || got.RefinementStatus != RefinementNoAudio {
		t.Fatalf("after GC's mark the refinement is %q (%v), want no_audio", got.RefinementStatus, err)
	}
	if !claimResume(t, store, "20261120080131") {
		t.Fatal("the automatic retry of a refinement whose recording GC took was not claimed")
	}
}
