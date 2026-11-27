package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
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
	runParallel(t)
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
	hook := mergedSessionHook(manager, store, fixedSummarizerOf(summarizer), &recordingBroadcaster{}, func(string) {},
		func(context.Context, string) error { return errors.New("no retry") }, newMergedRefinementFailureRecorder(store))
	go hook(context.Background(), id) // as the API runs it
	stopDuringSummary(t, store, manager, summarizer, id, nil)
}

// TestAMergedRefinementTheStopCutOffIsNotRecordedFailed fails if a merge's
// refinement, cut off by the stop while it restores a recording removed from
// this unit, is recorded failed by the stop, or is not retried by the next
// start (after the store's close and reopen, and the start's recovery); and
// if one whose restore failed for another reason is not recorded failed, with
// the page's Retry. The merge is real (storage.SQLiteStore.MergeSessions): a
// recording on this unit and one removed, so it starts pending, with a
// recording and the removal marker, a state no recovery lists. A refinement a
// stop cut off while it ran is the recovery's control.
func TestAMergedRefinementTheStopCutOffIsNotRecordedFailed(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(map[bool]string{true: "cut off by the stop", false: "the restore failed (control)"}[stopped], func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "merge-restart.db")
			store := openResumeStore(t, dbPath)
			at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
			endedResumeSession(t, store, dir, "local")
			endedResumeSession(t, store, dir, "removed")
			if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = ?, gdrive_folder_id = 'source-folder' WHERE id = 'removed'`, at.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if err := store.MergeSessions("merged", []string{"local", "removed"}, at, at.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if before := refinementOf(t, store, "merged"); before.RefinementStatus != storage.RefinementPending || before.AudioRemovedAt == "" || before.AudioPath == "" {
				t.Fatalf("control: the merge left refinement %q, recording %q, removal marker %q; want pending, with both", before.RefinementStatus, before.AudioPath, before.AudioRemovedAt)
			}
			path := endedResumeSession(t, store, dir, "running-control")
			cutOffRefinement(t, store, "running-control", path)
			guard := storeguard.New(t, store)
			manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
			appCtx, stopApp := newAppContext()
			defer stopApp()
			restoring := make(chan struct{}, 1)
			runner := retryRunner(store, func() string { return dir }, func(ctx context.Context, _ storage.Session) (deletionRecordingSet, error) {
				restoring <- struct{}{}
				if stopped {
					<-ctx.Done()
					return deletionRecordingSet{}, ctx.Err()
				}
				return deletionRecordingSet{}, errors.New("drive refused the restore")
			}, manager.RefineSession, newWarningsRegistry())
			runner.background = manager
			hook := mergedSessionHook(manager, store, fixedSummarizerOf(fixedSummarizer{}), &recordingBroadcaster{}, func(string) {}, runner.retryRefinement, newMergedRefinementFailureRecorder(store))
			done := make(chan struct{})
			go func() {
				defer close(done)
				hook(appCtx, "merged") // as the API runs it
			}()
			select {
			case <-restoring:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the merge hook did not restore the removed recording")
			}
			if stopped {
				stopApp()
			} else {
				// A failure no stop cut off is recorded before any stop:
				// the hook ends on its own.
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("control: the merge hook did not end after its restore failed")
				}
			}
			joinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := manager.StopBackground(joinCtx); err != nil {
				t.Fatal(err)
			}
			guard.Close(t) // as main closes the store after the stop
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the merge hook did not end once the stop had run")
			}
			guard.Check(t)
			atStop := refinementOf(t, store, "merged").RefinementStatus
			if !stopped {
				if atStop != storage.RefinementFailed {
					t.Fatalf("a restore that failed left the refinement %q, want %q, which the page offers Retry for", atStop, storage.RefinementFailed)
				}
				return
			}
			if atStop == storage.RefinementFailed {
				t.Fatalf("the stop was recorded as the refinement's failure (%q)", atStop)
			}

			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			restarted := openResumeStore(t, dbPath)
			recoverAtStart(restarted)
			pending, exhausted, err := restarted.RefinementResumes()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(pending, "running-control") {
				t.Fatalf("control: the start owes no retry to a refinement a stop cut off while it ran (owed %v, exhausted %v)", pending, exhausted)
			}
			if !slices.Contains(pending, "merged") {
				t.Fatalf("after the restart the merge's refinement is %q and owed no retry (owed %v, exhausted %v), with the refinement %q when the stop ran", refinementOf(t, restarted, "merged").RefinementStatus, pending, exhausted, atStop)
			}
			var retried []string
			resumeStoppedRefinements(context.Background(), restarted, newWarningsRegistry(), func(_ context.Context, id string) error {
				retried = append(retried, id)
				return nil
			})
			if !slices.Contains(retried, "merged") {
				t.Fatalf("the start retried %v, want the merge's refinement among them", retried)
			}
		})
	}
}

// TestAShutdownWaitsForAConciergeStartUnderWay fails if the Concierge's
// shutdown (lazyConcierge.Stop, which main runs before the store closes)
// returns while a start the voice's turn-on began still runs: the start
// writes the store (concierge.New cancels the questions of meetings that
// ended while it was off), and only then sees the shutdown and stops what it
// started.
func TestAShutdownWaitsForAConciergeStartUnderWay(t *testing.T) {
	cfg := geminiKeyConfig(strings.Repeat("k", 8))
	store := conciergeStore(t)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("ended", at); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("ended", at.Add(time.Minute), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "q1", SessionID: "ended", AskedAt: at, Holder: "session:agent-7"}); err != nil {
		t.Fatal(err)
	}
	guard := storeguard.New(t, store)
	entered, release := make(chan struct{}), make(chan struct{})
	var stopped atomic.Bool
	lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
		close(entered)
		<-release
		c, stop, err := startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
		return c, func(ctx context.Context) {
			stop(ctx)
			stopped.Store(true)
		}, err
	}, nil, discardLogger)
	lazy.followEnabled(true)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: turning the voice on started no Concierge")
	}
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	lazy.Stop(t.Context())
	guard.Close(t)
	if !stopped.Load() {
		t.Error("the shutdown returned before the start under way had ended and stopped what it started")
	}
	time.Sleep(100 * time.Millisecond) // a write the start left would land now
	guard.Check(t)
	if task, err := store.VoiceTask("q1"); err != nil || task.State != storage.VoiceTaskCancelled {
		t.Errorf("control: the start did not write the store before the shutdown returned: q1 = %+v, %v", task, err)
	}
}

// TestAShutdownWaitsForATurnOffUnderWay fails if the Concierge's shutdown
// (lazyConcierge.Stop) returns while a turn-off from a settings change is
// under way: the turn-off has detached its Concierge, so the shutdown sees
// none, and then cancels its working questions (VoiceOff), which writes the
// store after main closed it (storeguard records it). From review 6004725672
// of #132; the control, a turn-off that ended first, must stay clean too.
func TestAShutdownWaitsForATurnOffUnderWay(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "control: the turn-off ended before the shutdown"
		if held {
			name = "the turn-off is under way at the shutdown"
		}
		t.Run(name, func(t *testing.T) {
			store := conciergeStore(t)
			if err := store.CreateSession("meeting", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "q1", SessionID: "meeting", AskedAt: time.Now(), Holder: "session:agent-7"}); err != nil {
				t.Fatal(err)
			}
			cfg := geminiKeyConfig(strings.Repeat("k", 8))
			detached, release, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
			lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
				return startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
			}, func(c *concierge.Concierge) {
				if c == nil { // the turn-off has detached its Concierge
					close(detached)
					<-release
				}
			}, discardLogger)
			if _, err := lazy.start(); err != nil {
				t.Fatal(err)
			}
			guard := storeguard.New(t, store)
			go func() {
				defer close(ended)
				lazy.followEnabled(false)
			}()
			select {
			case <-detached:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the turn-off did not detach its Concierge")
			}
			if !held {
				close(release)
				<-ended
			} else {
				time.AfterFunc(200*time.Millisecond, func() { close(release) })
			}
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				lazy.Stop(t.Context())
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("the shutdown did not end")
			}
			guard.Close(t)
			select {
			case <-ended:
			case <-time.After(5 * time.Second):
				t.Fatal("the turn-off did not end")
			}
			time.Sleep(50 * time.Millisecond)
			guard.Check(t)
			if task, err := store.VoiceTask("q1"); err != nil || task.State != storage.VoiceTaskCancelled {
				t.Errorf("control: the turn-off did not cancel the working question before the shutdown returned: %+v, %v", task, err)
			}
		})
	}
}

// TestATurnOffHeldInTheStoreDoesNotHoldTheShutdownPastItsDeadline fails if
// the Concierge's shutdown waits past the shutdown's deadline for a turn-off
// whose store write waits for the store's one connection, or if that write
// lands once main has closed the store (closeStore): the shutdown gives up
// at the deadline, and the closed store refuses the write. From review
// 6004998055 of #132; the control is the same turn-off with the connection
// free, which cancels the question before the shutdown returns.
func TestATurnOffHeldInTheStoreDoesNotHoldTheShutdownPastItsDeadline(t *testing.T) {
	runParallel(t)
	for _, held := range []bool{false, true} {
		name := "control: the store's connection is free"
		if held {
			name = "the turn-off waits for the store's connection past the deadline"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store := openResumeStore(t, path)
			if got := store.DB().Stats().MaxOpenConnections; got != 1 {
				t.Fatalf("control: the store allows %d connections, want its one", got)
			}
			if err := store.CreateSession("meeting", time.Now()); err != nil {
				t.Fatal(err)
			}
			cfg := geminiKeyConfig(strings.Repeat("k", 8))
			detached := make(chan struct{})
			lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
				return startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
			}, func(c *concierge.Concierge) {
				if c == nil {
					close(detached)
				}
			}, discardLogger)
			if _, err := lazy.start(); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "q1", SessionID: "meeting", AskedAt: time.Now(), Holder: "session:agent-7"}); err != nil {
				t.Fatal(err)
			}
			recovery := recoverAtStart(store) // as main runs it at the start, before the connection is reserved
			var conn *sql.Conn
			if held {
				var err error
				if conn, err = store.DB().Conn(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
			}
			waitsBefore := store.DB().Stats().WaitCount
			manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
			turnedOff := make(chan struct{})
			go func() {
				defer close(turnedOff)
				_ = manager.Run(context.Background(), func(context.Context) error {
					lazy.followEnabled(false)
					return nil
				})
			}()
			select {
			case <-detached:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the turn-off did not detach its Concierge")
			}
			if held {
				waitUntil(t, "the turn-off's write waiting for the store's connection", func() bool {
					return store.DB().Stats().WaitCount > waitsBefore
				})
			}
			// The held turn-off must outlast the stop's deadline, so that deadline is short. The
			// control's turn-off is free to finish: it gets a deadline it fits under any load
			// (main's is shutdownWait, 30 s). With 150 ms, a turn-off slowed by -race and a loaded
			// machine still committed its write when the stop gave up, and closeStore rightly left
			// the store open under it (5 of 50 runs on one core beside 20 busy processes).
			deadline := 150 * time.Millisecond
			if !held {
				deadline = 10 * time.Second
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			recovery.stop(shutdownCtx, manager, lifecyclePublishWait)
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				lazy.Stop(shutdownCtx)
			}()
			select {
			case <-stopped:
			case <-time.After(deadline + time.Second):
				t.Fatal("the Concierge's shutdown waited past the shutdown's deadline for the turn-off")
			}
			if !closeStore(shutdownCtx, store) {
				t.Fatal("closeStore left the store open, though no statement ran on it")
			}
			if conn != nil {
				_ = conn.Close() // the turn-off's write gets the connection now
			}
			select {
			case <-turnedOff:
			case <-time.After(5 * time.Second):
				t.Fatal("the turn-off did not end")
			}
			reopened := openResumeStore(t, path)
			task, err := reopened.VoiceTask("q1")
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]storage.VoiceTaskState{false: storage.VoiceTaskCancelled, true: storage.VoiceTaskWorking}[held]; task.State != want {
				t.Errorf("the question after the shutdown is %q, want %q", task.State, want)
			}
		})
	}
}

// TestARunningConciergeStopsWithinTheShutdownsDeadline fails if the shutdown
// of a running Concierge (lazyConcierge.Stop) waits past the shutdown's
// deadline for a call of it whose store write waits for the store's one
// connection, or if that write lands once main has closed the store: the
// Concierge's stop gives up at the deadline (Concierge.StopWithin), and the
// closed store refuses the write. From review 6005590173 of #132; the
// control is the same call with the connection free, which lands before the
// shutdown.
func TestARunningConciergeStopsWithinTheShutdownsDeadline(t *testing.T) {
	runParallel(t)
	for _, held := range []bool{false, true} {
		name := "control: the store's connection is free"
		if held {
			name = "a call's store write waits for the connection past the deadline"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store := openResumeStore(t, path)
			if err := store.CreateSession("meeting", time.Now()); err != nil {
				t.Fatal(err)
			}
			cfg := geminiKeyConfig(strings.Repeat("k", 8))
			lazy := newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
				return startConcierge(t.Context(), &cfg, store, discardLogger, nil, nil)
			}, nil, discardLogger)
			if _, err := lazy.start(); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "q1", SessionID: "meeting", AskedAt: time.Now(), Holder: "session:agent-7"}); err != nil {
				t.Fatal(err)
			}
			conn, err := store.DB().Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			waitsBefore := store.DB().Stats().WaitCount
			wrote := make(chan error, 1)
			go func() { wrote <- lazy.Spoken(context.Background(), "q1") }()
			waitUntil(t, "the call's write waiting for the store's connection", func() bool {
				return store.DB().Stats().WaitCount > waitsBefore
			})
			if !held {
				_ = conn.Close()
				select {
				case err := <-wrote:
					if err != nil {
						t.Fatalf("control: the call's write = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("control: the call's write did not end")
				}
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			<-shutdownCtx.Done() // the stop's earlier steps took the whole deadline
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				lazy.Stop(shutdownCtx)
			}()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("the Concierge's shutdown waited past the shutdown's deadline for its call's write")
			}
			if !closeStore(shutdownCtx, store) {
				t.Fatal("closeStore left the store open, though no statement ran on it")
			}
			if held {
				_ = conn.Close() // the call's write gets the connection now
				select {
				case err := <-wrote:
					if !errors.Is(err, stopping.ErrStopped) {
						t.Errorf("the call's write after the store closed = %v, want stopping.ErrStopped", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the call's write did not end")
				}
			}
			reopened := openResumeStore(t, path)
			task, err := reopened.VoiceTask("q1")
			if err != nil {
				t.Fatal(err)
			}
			if spoken := task.SpokenAt != nil; spoken == held {
				t.Errorf("the question spoken after the shutdown: %t, want %t", spoken, !held)
			}
		})
	}
}
