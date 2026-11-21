package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// watchedBackground admits work as the session manager does, with no stop,
// counts what it admits, and says whether admitted work is running (inside):
// a fake that the work under test reaches checks it, so the test sees that
// the store work ran as work the stop waits for. refuse makes it refuse, as
// the manager does once stopped.
type watchedBackground struct {
	running, admitted atomic.Int32
	refuse            atomic.Bool
}

func (b *watchedBackground) Go(f func(context.Context)) {
	if b.refuse.Load() {
		return
	}
	b.admitted.Add(1)
	b.running.Add(1)
	go func() {
		defer b.running.Add(-1)
		f(context.Background())
	}()
}

func (b *watchedBackground) Run(ctx context.Context, f func(context.Context) error) error {
	if b.refuse.Load() {
		return stopping.ErrStopped
	}
	b.admitted.Add(1)
	b.running.Add(1)
	defer b.running.Add(-1)
	return f(ctx)
}

func (b *watchedBackground) inside() bool { return b.running.Load() > 0 }

// watchingSyncer is a syncer that counts the syncs it runs outside bg.
type watchingSyncer struct {
	countingSyncer
	bg      *watchedBackground
	outside atomic.Int64
}

func (s *watchingSyncer) SyncSession(ctx context.Context, id string) error {
	if !s.bg.inside() {
		s.outside.Add(1)
	}
	return s.countingSyncer.SyncSession(ctx, id)
}

// watchingSweepStore is the sweep's store with one session to sync, counting
// the erasure retries it runs outside bg.
type watchingSweepStore struct {
	sweepStoreStub
	bg               *watchedBackground
	retries, outside atomic.Int64
}

func (s *watchingSweepStore) RetryOwedErasure() error {
	s.retries.Add(1)
	if !s.bg.inside() {
		s.outside.Add(1)
	}
	return nil
}

// TestTheAppsDriveWorkRunsAsWorkTheStopWaitsFor fails if a sync that Retry
// sync starts, or a pass of the Drive sweep (its syncs and its erasure
// retries), writes the store outside the work the stop cuts off and waits
// for: the store closes after the stop, under it. It also fails if the sweep
// keeps running once the stop refuses its passes.
func TestTheAppsDriveWorkRunsAsWorkTheStopWaitsFor(t *testing.T) {
	t.Run("a retried sync", func(t *testing.T) {
		bg := &watchedBackground{}
		syncer := &watchingSyncer{bg: bg}
		if err := retrySync(context.Background(), bg, syncer, "20261106090000"); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "the retried sync", func() bool { return syncer.calls.Load() == 1 })
		if n := syncer.outside.Load(); n != 0 {
			t.Fatalf("the retried sync ran outside the work the stop waits for")
		}
	})
	t.Run("the Drive sweep", func(t *testing.T) {
		bg := &watchedBackground{}
		syncer := &watchingSyncer{bg: bg}
		drive := &driveSync{}
		drive.Set(syncer)
		store := &watchingSweepStore{bg: bg}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		swept := goDone(func() { sweepDriveSync(ctx, bg, store, drive, newWarningsRegistry(), time.Millisecond, nil) })
		waitUntil(t, "two sweep passes", func() bool { return syncer.calls.Load() >= 2 && store.retries.Load() >= 2 })
		if syncer.outside.Load() != 0 || store.outside.Load() != 0 {
			t.Fatalf("a sweep pass ran %d syncs and %d erasure retries outside the work the stop waits for", syncer.outside.Load(), store.outside.Load())
		}
		bg.refuse.Store(true)
		if !returnedWithin(swept, 5*time.Second) {
			t.Fatal("the sweep kept running once the stop refused its passes")
		}
	})
}

// TestRunPassesEndsWhenTheStopRefusesAPass fails if runPasses runs a pass
// outside the work the stop waits for, or keeps running once the stop
// refuses a pass: garbage collection, the census and the Drive sweep run
// their passes through it.
func TestRunPassesEndsWhenTheStopRefusesAPass(t *testing.T) {
	bg := &watchedBackground{}
	var passes, outside atomic.Int32
	done := goDone(func() {
		runPasses(context.Background(), bg, time.Millisecond, func(context.Context) {
			if !bg.inside() {
				outside.Add(1)
			}
			if passes.Add(1) == 3 {
				bg.refuse.Store(true)
			}
		})
	})
	if !returnedWithin(done, 5*time.Second) {
		t.Fatal("runPasses kept running once the stop refused a pass")
	}
	if passes.Load() != 3 || outside.Load() != 0 {
		t.Fatalf("%d passes ran, %d outside the work the stop waits for; want 3, none outside", passes.Load(), outside.Load())
	}
}

// TestTheDeletionsBackgroundWorkRunsAsWorkTheStopWaitsFor fails if Retry
// refinement, called directly or started in the background as a deletion
// starts it, or the start's recovery of pending splices, does its store work
// outside the work the stop cuts off and waits for.
func TestTheDeletionsBackgroundWorkRunsAsWorkTheStopWaitsFor(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	rs := gdrive.RestoredSession{ID: "restored", StartedAt: started, EndedAt: &ended, DriveFolderID: "folder-1", HasAudio: true}
	restored := restoredDriveSession(&rs, ended)
	if err := store.ImportSession(&restored, nil, nil); err != nil {
		t.Fatal(err)
	}
	bg := &watchedBackground{}
	var ensured, outside atomic.Int32
	runner := retryRunner(store, t.TempDir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		ensured.Add(1)
		if !bg.inside() {
			outside.Add(1)
		}
		return deletionRecordingSet{}, errors.New("nothing to restore")
	}, func(context.Context, string, string, time.Time) error { return nil }, newWarningsRegistry())
	runner.background = bg

	if err := runner.retryRefinement(context.Background(), "restored"); err == nil {
		t.Fatal("control: the retry succeeded with nothing to restore")
	}
	runner.startRetryRefinement("restored")
	waitUntil(t, "the retry a deletion starts", func() bool { return ensured.Load() == 2 })
	if n := outside.Load(); n != 0 {
		t.Fatalf("%d of 2 Retry refinements restored outside the work the stop waits for", n)
	}

	before := bg.admitted.Load()
	runner.recoverAtStart(context.Background(), nil, newWarningsRegistry())
	if bg.admitted.Load() == before {
		t.Fatal("the start's recovery of pending splices ran outside the work the stop waits for")
	}
}

// TestTheStopWaitsForAMergedSessionsSummary fails if the stop, as main runs
// it, returns while the summary of a session a merge made (the merge hook, as
// main wires it) still runs, or if the summary it cut off is recorded failed
// instead of left running for the next start.
func TestTheStopWaitsForAMergedSessionsSummary(t *testing.T) {
	store := openResumeStore(t, filepath.Join(t.TempDir(), "merged.db"))
	const id = "20261120090000"
	endedSessionWithWords(t, store, id, storage.SummaryCompleted)
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	summarizer := &heldSummarizer{entered: make(chan struct{}), stall: 100 * time.Millisecond}
	hook := mergedSessionHook(manager, store, summarizer, &recordingBroadcaster{}, func(string) {},
		func(context.Context, string) error { return errors.New("no retry") }, newMergedRefinementFailureRecorder(store))
	go hook(context.Background(), id) // as the API runs it
	stopDuringSummary(t, store, manager, summarizer, id, nil)
}

// TestAMergedRefinementTheStopCutOffIsNotRecordedFailed fails if the merge
// hook's failure recorder records a refinement the stop cut off as failed, or
// does not record one that failed for another reason.
func TestAMergedRefinementTheStopCutOffIsNotRecordedFailed(t *testing.T) {
	store := openResumeStore(t, filepath.Join(t.TempDir(), "refine.db"))
	const id = "20261120090000"
	endedSessionWithWords(t, store, id, storage.SummaryCompleted)
	before := refinementOf(t, store, id).RefinementStatus
	record := newMergedRefinementFailureRecorder(store)

	stopped, stop := context.WithCancelCause(context.Background())
	stop(stopping.ErrStopped)
	record(stopped, id, context.Canceled)
	if got := refinementOf(t, store, id).RefinementStatus; got != before {
		t.Fatalf("a refinement the stop cut off: refinement status %q, want it left %q", got, before)
	}

	record(context.Background(), id, errors.New("the recording is gone"))
	if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementFailed {
		t.Fatalf("control: a refinement that failed: refinement status %q, want %q", got, storage.RefinementFailed)
	}
}
