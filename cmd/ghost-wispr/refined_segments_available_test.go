package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// refinementStatesRunner is a runner over a store holding one ended session
// per refinement state a deletion meets. Its Retry refinement reports each
// session it starts to restore on retried and then fails, and it refines
// nothing itself: a refinement started directly fails the test.
func refinementStatesRunner(t *testing.T) (*segmentDeleteRunner, <-chan string) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "states.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id, status, removedAt, folder string
		revision                      int
	}{
		{"refined", storage.RefinementCompleted, "", "", 2},
		{"live-only", storage.RefinementCompleted, "", "", 0},
		{"on-drive", storage.RefinementNoAudio, "2026-11-12T00:00:00Z", "folder-1", 0},
		{"nowhere", storage.RefinementNoAudio, "", "", 0},
		{"failed", storage.RefinementFailed, "", "", 0},
		{"running", storage.RefinementRunning, "", "", 0},
	} {
		if err := store.CreateSession(row.id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(row.id, started.Add(time.Minute), filepath.Join(dir, row.id+".mp3")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, refined_revision = ?, audio_removed_at = ?, gdrive_folder_id = ?, sync_status = ?, sync_state = ? WHERE id = ?`,
			row.status, row.revision, row.removedAt, row.folder, storage.SyncSynced, storage.SyncStateSynced, row.id); err != nil {
			t.Fatal(err)
		}
	}
	retried := make(chan string, 8)
	runner := testDeleteRunner(t, &segmentDeleteRunner{
		store:    store,
		audioDir: func() string { return dir },
		ensureRecordings: func(_ context.Context, session storage.Session) (deletionRecordingSet, error) {
			retried <- session.ID
			return deletionRecordingSet{}, errors.New("this test's Drive has no recordings")
		},
		refine: func(_ context.Context, sessionID, _ string, _ time.Time) error {
			t.Errorf("refined %s directly, without Retry refinement's restore", sessionID)
			return nil
		},
	})
	return runner, retried
}

// startedRetry reports whether the runner started Retry refinement of
// sessionID: within a second when want is true, none within 100 ms
// otherwise.
func startedRetry(t *testing.T, retried <-chan string, sessionID string, want bool) {
	t.Helper()
	wait := 100 * time.Millisecond
	if want {
		wait = time.Second
	}
	select {
	case got := <-retried:
		if !want || got != sessionID {
			t.Fatalf("started Retry refinement of %s, want %v for %s", got, want, sessionID)
		}
	case <-time.After(wait):
		if want {
			t.Fatalf("no Retry refinement started for %s", sessionID)
		}
	}
}

// TestADeletionAnswersEachRefinementState fails if the runner, which decides
// what a segment deletion does with a session not ready for it, answers a
// refinement state differently from how the segment-delete route did before
// the decision moved to the runner, or starts a refinement the route did not:
// a session refined with timed words is ready; one completed without timed
// words, or with its recording only on Drive, starts Retry refinement (which
// restores the recording first) and is refused; one whose recording is
// nowhere is refused with why; a failed or running refinement is refused and
// starts nothing.
func TestADeletionAnswersEachRefinementState(t *testing.T) {
	runner, retried := refinementStatesRunner(t)
	for _, tc := range []struct {
		session string
		want    error
		retries bool
	}{
		{"refined", nil, false},
		{"live-only", storage.ErrRefinedSegmentsUnavailable, true},
		{"on-drive", server.ErrSegmentRecordingRestoring, true},
		{"failed", server.ErrSegmentRefinementFailed, false},
		{"running", storage.ErrRefinedSegmentsUnavailable, false},
	} {
		t.Run(tc.session, func(t *testing.T) {
			if err := runner.readyToDelete(tc.session); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("readyToDelete(%s) = %v, want %v", tc.session, err, tc.want)
			}
			startedRetry(t, retried, tc.session, tc.retries)
		})
	}

	err := runner.readyToDelete("nowhere")
	var unavailable *server.SegmentRecordingUnavailableError
	if !errors.As(err, &unavailable) || err.Error() != "no recording is available on this unit or Drive" {
		t.Fatalf("readyToDelete(nowhere) = %v, want why no recording can be had", err)
	}
	startedRetry(t, retried, "nowhere", false)
}

// TestAnUnrefinedSessionUnderRecoveryIsToldItIsNotRefined fails if a
// deletion from a session that is both under recovery and not refined is
// answered "Restoring the recording" instead of "still being refined", as the
// route answered it before the runner made the decision; a refined session
// under recovery is still refused as restoring.
func TestAnUnrefinedSessionUnderRecoveryIsToldItIsNotRefined(t *testing.T) {
	runner, retried := refinementStatesRunner(t)
	ctx := context.Background()
	for _, id := range []string{"running", "refined"} {
		runner.locks.setRecovering(id, true)
	}
	if err := runner.DeleteRefinedText(ctx, "running", 1, 1); !errors.Is(err, storage.ErrRefinedSegmentsUnavailable) || isRecordingRestore(err) {
		t.Fatalf("Delete text in an unrefined session under recovery = %v, want not refined", err)
	}
	if err := runner.DeleteRefinedAudio(ctx, "running", 1, 1); !errors.Is(err, storage.ErrRefinedSegmentsUnavailable) || isRecordingRestore(err) {
		t.Fatalf("Delete text and audio in an unrefined session under recovery = %v, want not refined", err)
	}
	if err := runner.DeleteRefinedAudio(ctx, "refined", 1, 2); !isRecordingRestore(err) {
		t.Fatalf("Delete text and audio in a refined session under recovery = %v, want restoring", err)
	}
	startedRetry(t, retried, "running", false)
}

// TestADeletionFromALiveOnlySessionRestoresBeforeRefining fails if "Delete
// text and audio" from a completed session without timed words refines it
// directly instead of starting Retry refinement, which restores its
// recording from Drive first.
func TestADeletionFromALiveOnlySessionRestoresBeforeRefining(t *testing.T) {
	runner, retried := refinementStatesRunner(t)
	if err := runner.DeleteRefinedAudio(context.Background(), "live-only", 1, 1); !errors.Is(err, storage.ErrRefinedSegmentsUnavailable) {
		t.Fatalf("Delete text and audio in a live-only session = %v, want not refined", err)
	}
	startedRetry(t, retried, "live-only", true)
}

func isRecordingRestore(err error) bool {
	var restoring interface{ RecordingRestoreInProgress() }
	return errors.As(err, &restoring)
}
