package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// batchFunc is a batch transcriber made of a function.
type batchFunc func(context.Context, string) (transcribe.BatchResult, error)

func (f batchFunc) Transcribe(ctx context.Context, path string) (transcribe.BatchResult, error) {
	return f(ctx, path)
}

// transcriptions is a batch transcriber that records which recordings it
// transcribed, and answers each with one word.
type transcriptions struct {
	mu   sync.Mutex
	done []string
}

func (r *transcriptions) Transcribe(_ context.Context, path string) (transcribe.BatchResult, error) {
	r.mu.Lock()
	r.done = append(r.done, filepath.Base(path))
	r.mu.Unlock()
	return transcribe.BatchResult{Model: "fixture", Words: []transcribe.TimestampedWord{{Text: "resumed", Start: 0, End: 1}}}, nil
}

func (r *transcriptions) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.done)
}

func openResumeStore(t *testing.T, path string) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// endedResumeSession ends session id with a synthetic recording in dir and
// returns the recording's path.
func endedResumeSession(t *testing.T, store *storage.SQLiteStore, dir, id string) string {
	t.Helper()
	at := time.Date(2026, 11, 20, 8, 1, 25, 0, time.UTC)
	path := filepath.Join(dir, id+".mp3")
	if err := os.WriteFile(path, []byte("synthetic fixture; no room audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, at); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, at.Add(21*time.Second), path); err != nil {
		t.Fatal(err)
	}
	return path
}

// cutOffRefinement leaves session id's refinement running, as a stop inside
// it does.
func cutOffRefinement(t *testing.T, store *storage.SQLiteStore, id, path string) int {
	t.Helper()
	generation, claimed, err := store.ClaimRefinement(id, path)
	if err != nil || !claimed {
		t.Fatalf("claim the refinement of %s: claimed=%t, %v", id, claimed, err)
	}
	return generation
}

// app is one run of the app's start and stop on a store, as main makes them:
// the real session manager, refining through transcriber, the real "Retry
// refinement" action (the recordings are on this unit, so it restores none),
// and the stop recovery main drives.
type app struct {
	t        *testing.T
	recovery *stopRecovery
	manager  *session.Manager
	warnings *warningsRegistry
	cancel   context.CancelFunc
}

// start runs the start's part, as main does: the recovery, before anything
// can refine, then the retries. recorder is the session's recorder, or nil.
// The recordings are on this unit, so the Retry action restores none.
func start(ctx context.Context, t *testing.T, store *storage.SQLiteStore, dir string, transcriber transcribe.BatchTranscriber, recorder session.Recorder) *app {
	t.Helper()
	return startWith(ctx, t, store, dir, transcriber, recorder, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{}, errors.New("the recording is on this unit: nothing to restore")
	})
}

// startWith is start with the Retry action restoring recordings with restore.
func startWith(ctx context.Context, t *testing.T, store *storage.SQLiteStore, dir string, transcriber transcribe.BatchTranscriber, recorder session.Recorder, restore func(context.Context, storage.Session) (deletionRecordingSet, error)) *app {
	t.Helper()
	recovery := recoverAtStart(store)
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	warnings := newWarningsRegistry()
	retry := retryRunner(store, func() string { return dir }, restore, manager.RefineSession, warnings).retryRefinement
	ctx, cancel := context.WithCancel(ctx)
	recovery.retry(ctx, warnings, retry)
	a := &app{t: t, recovery: recovery, manager: manager, warnings: warnings, cancel: cancel}
	t.Cleanup(cancel)
	return a
}

// stopBeforeTheRetries is a start that a stop cuts off after its recovery
// and before its retries begin, as main's start is between recoverAtStart and
// recovery.retry: the store then owes session id its retry. A whole start
// stopped at once cannot stand for it, since its retries may already have run
// before the stop, and then nothing is owed.
func stopBeforeTheRetries(t *testing.T, store *storage.SQLiteStore, id string) {
	t.Helper()
	recoverAtStart(store)
	if pending, _, err := store.RefinementResumes(); err != nil || !slices.Equal(pending, []string{id}) {
		t.Fatalf("after a start stopped before its retries, the store owes %v (%v), want the retry of %s", pending, err, id)
	}
}

// settle waits until the start's retries have ended on their own.
func (a *app) settle() {
	a.t.Helper()
	done := make(chan struct{})
	go func() { a.recovery.retries.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		a.t.Fatal("the start's retries did not end within 10 s")
	}
}

// stop runs the stop's part, as main does: it cancels the run, then stops
// with the given shutdown deadline and lifecycle wait, and returns how long
// the stop took.
func (a *app) stop(deadline, lifecycleWait time.Duration) time.Duration {
	a.t.Helper()
	a.cancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	began := time.Now()
	a.recovery.stop(shutdownCtx, a.manager, lifecycleWait)
	return time.Since(began)
}

func refinementOf(t *testing.T, store *storage.SQLiteStore, id string) storage.Session {
	t.Helper()
	got, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func retryWarning(warnings *warningsRegistry, id string) string {
	for _, text := range warnings.List() {
		if strings.HasPrefix(text, "Retry refinement failed for session "+id+":") {
			return text
		}
	}
	return ""
}

// TestARefinementAStopCutOffRunsAgainOnce fails if a refinement the app was
// running when it stopped is not run again at the next start, or is run again
// more than once. On the office Pi on 2026-11-20 a restart during a session's
// refinement left it "failed", and nothing ran it again, so a meeting cut off
// that way stayed unrefined until a person pressed Retry. Here a start and a
// stop run as main runs them, twice: the first start retries the cut-off
// refinement, which completes, and stops on time; the second retries nothing.
// A completed refinement is left alone throughout.
func TestARefinementAStopCutOffRunsAgainOnce(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "resume.db")
	store := openResumeStore(t, db)
	cutOff, done := "20261120080131", "20261120000356"
	cutOffRefinement(t, store, cutOff, endedResumeSession(t, store, dir, cutOff))
	endedResumeSession(t, store, dir, done)
	if err := store.UpdateRefinementAtGeneration(done, "", storage.RefinementCompleted, "", 0); err != nil {
		t.Fatal(err)
	}

	transcriber := &transcriptions{}
	for run := 1; run <= 2; run++ {
		if run == 2 {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openResumeStore(t, db)
		}
		a := start(t.Context(), t, store, dir, transcriber, nil)
		a.settle()
		if took := a.stop(2*time.Second, lifecyclePublishWait); took > 500*time.Millisecond {
			t.Fatalf("run %d: the stop took %v with nothing to wait for", run, took)
		}
		if got := transcriber.list(); !slices.Equal(got, []string{cutOff + ".mp3"}) {
			t.Fatalf("after run %d the app transcribed %v, want the cut-off session's recording once", run, got)
		}
		for _, id := range []string{cutOff, done} {
			if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementCompleted {
				t.Fatalf("after run %d session %s refinement is %q, want completed", run, id, got)
			}
		}
		if warning := retryWarning(a.warnings, cutOff); warning != "" {
			t.Fatalf("after run %d the app shows %q", run, warning)
		}
	}
}

// TestAStopDuringARetryEndsOnTimeAndTheRetryIsNotRunAgain fails if a stop
// that comes while the start retries a cut-off refinement, and while a
// pause's end of the current session is held by its recorder, outlasts its
// shutdown deadline, or if the next start runs that retry again. The retry's
// transcription hangs, as a Deepgram call can, and the pause's end holds its
// recorder, as saving a long recording does: the stop must still return at
// its deadline (main's 30 s and 5 s, scaled to 50 ms and 150 ms). Before, its
// wait for the session's events began a time limit of its own after the
// deadline was spent. The next start leaves the refinement failed, with
// Retry's warning, and transcribes nothing.
func TestAStopDuringARetryEndsOnTimeAndTheRetryIsNotRunAgain(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "stop.db")
	store := openResumeStore(t, db)
	const id = "20261120080131"
	cutOffRefinement(t, store, id, endedResumeSession(t, store, dir, id))

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hung := batchFunc(func(context.Context, string) (transcribe.BatchResult, error) {
		once.Do(func() { close(entered) })
		<-release // it does not heed its context
		return transcribe.BatchResult{}, errors.New("the hung call failed")
	})
	recorder := &heldEndRecorder{held: make(chan struct{}), release: make(chan struct{})}
	a := start(t.Context(), t, store, dir, hung, recorder)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start did not retry the refinement the stop cut off")
	}
	if _, err := a.manager.ManualStartSession(t.Context(), ""); err != nil {
		t.Fatalf("start session: %v", err)
	}
	pauseEnded := make(chan error, 1)
	go func() { pauseEnded <- a.manager.ForceEndSession(context.Background()) }() // the pause's end
	select {
	case <-recorder.held:
	case <-time.After(2 * time.Second):
		t.Fatal("the pause's end never reached the recorder")
	}

	logs := logtest.Capture(t)
	const deadline = 50 * time.Millisecond
	took := a.stop(deadline, 150*time.Millisecond)
	var warned []string
	for _, record := range logs.Records() {
		if record.Level == "WARN" {
			warned = append(warned, record.Message)
		}
	}
	t.Logf("the stop took %v; warned %q", took, warned)
	if took > deadline+50*time.Millisecond {
		t.Fatalf("the stop took %v, past its %v shutdown deadline", took, deadline)
	}
	if want := []string{"force end session failed", "lifecycle event publishing did not finish before shutdown", "the refinement retries a stop cut off did not end before shutdown"}; !slices.Equal(warned, want) {
		t.Fatalf("the stop warned %q, want %q", warned, want)
	}

	// The process ends; the hung call and the held end let go.
	close(release)
	close(recorder.release)
	if err := <-pauseEnded; err != nil {
		t.Fatalf("the pause's end: %v", err)
	}
	a.settle()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openResumeStore(t, db)
	transcriber := &transcriptions{}
	next := start(t.Context(), t, store, dir, transcriber, nil)
	next.settle()
	next.stop(2*time.Second, lifecyclePublishWait)
	if got := transcriber.list(); len(got) != 0 {
		t.Fatalf("the next start transcribed %v: it ran again the retry the stop cut off", got)
	}
	if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementFailed {
		t.Fatalf("the refinement is %q after the next start, want failed", got)
	}
	if warning := retryWarning(next.warnings, id); !strings.Contains(warning, errRefinementRetryCutOff.Error()) {
		t.Fatalf("the next start shows %q, want Retry's warning that the automatic retry was cut off", warning)
	}
}

// TestTheStopWaitsForARetryItCancels fails if the stop returns while a retry
// it cancelled is still writing: the database closes after the stop, under
// it. The retry's transcription takes 100 ms to give up once cancelled.
func TestTheStopWaitsForARetryItCancels(t *testing.T) {
	dir := t.TempDir()
	store := openResumeStore(t, filepath.Join(dir, "cancel.db"))
	const id = "20261120080131"
	cutOffRefinement(t, store, id, endedResumeSession(t, store, dir, id))
	entered := make(chan struct{})
	var once sync.Once
	slow := batchFunc(func(ctx context.Context, _ string) (transcribe.BatchResult, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		return transcribe.BatchResult{}, ctx.Err()
	})
	a := start(t.Context(), t, store, dir, slow, nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start did not retry the refinement the stop cut off")
	}
	if took := a.stop(2*time.Second, lifecyclePublishWait); took < 100*time.Millisecond || took > time.Second {
		t.Fatalf("the stop took %v, want it to wait the 100 ms its cancelled retry takes, and no longer", took)
	}
	if got := refinementOf(t, store, id).RefinementStatus; got == storage.RefinementRunning {
		t.Fatal("the stop returned with the retry's refinement still running")
	}
}

// TestTheStopWaitsForTheEventsWithinItsLimit fails if the stop gives up on
// the session's lifecycle events before its limit with time left on the
// shutdown deadline, or waits past the limit.
func TestTheStopWaitsForTheEventsWithinItsLimit(t *testing.T) {
	const lifecycleWait = 80 * time.Millisecond
	began := time.Now()
	(&stopRecovery{}).stop(t.Context(), &stoppingManager{}, lifecycleWait)
	if took := time.Since(began); took < lifecycleWait || took > lifecycleWait+100*time.Millisecond {
		t.Fatalf("the stop waited %v for the session's events, want about %v", took, lifecycleWait)
	}
}

// stoppingManager ends at once, and its lifecycle events never finish.
type stoppingManager struct{}

func (*stoppingManager) ForceEndSession(context.Context) error { return nil }
func (*stoppingManager) WaitForLifecyclePublishes(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// heldEndRecorder is a recorder whose first EndSession waits until release
// closes, as a recorder saving a long recording does.
type heldEndRecorder struct {
	held, release chan struct{}
	once          sync.Once
}

func (*heldEndRecorder) StartSession(string) error { return nil }
func (r *heldEndRecorder) EndSession(string) (string, error) {
	r.once.Do(func() { close(r.held); <-r.release })
	return "", nil
}
func (*heldEndRecorder) DiscardUnsaved() string            { return "" }
func (*heldEndRecorder) HoldPreroll(time.Duration)         {}
func (*heldEndRecorder) ReleasePreroll()                   {}
func (*heldEndRecorder) SetLookback(time.Duration)         {}
func (*heldEndRecorder) Pending() time.Duration            { return 0 }
func (*heldEndRecorder) SinceClose() (time.Duration, bool) { return 0, true }

// crashChildDB names the database TestAutomaticRefinementRetryCrashChild
// starts on.
const crashChildDB = "GHOST_WISPR_TEST_RESUME_CRASH_DB"

// TestAutomaticRefinementRetryCrashChild is the app's start in a process of
// its own: its automatic refinement retry crashes the process (exit 73) inside
// the real session manager's batch transcription, after the real claim.
func TestAutomaticRefinementRetryCrashChild(t *testing.T) {
	db := os.Getenv(crashChildDB)
	if db == "" {
		t.Skip("run by TestAnAutomaticRetryThatCrashesTheProcessRunsOnce")
	}
	store := openResumeStore(t, db)
	a := start(context.Background(), t, store, filepath.Dir(db), batchFunc(func(context.Context, string) (transcribe.BatchResult, error) {
		attempts, err := os.OpenFile(db+".attempts", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = attempts.WriteString("automatic retry\n")
		}
		if err == nil {
			err = attempts.Close()
		}
		if err != nil {
			t.Error(err)
		}
		os.Exit(73)
		return transcribe.BatchResult{}, nil
	}), nil)
	a.settle()
}

// TestAnAutomaticRetryThatCrashesTheProcessRunsOnce fails if a refinement
// whose automatic retry crashes the process is retried automatically again:
// it would crash it at every restart. The retry is recorded before it
// starts, so the start after the crash leaves the refinement failed with
// Retry's warning and runs nothing, and so does every start after it, until a
// person's Retry runs it.
func TestAnAutomaticRetryThatCrashesTheProcessRunsOnce(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "restart.db")
	store := openResumeStore(t, db)
	const id = "20261120080131"
	path := endedResumeSession(t, store, dir, id)
	cutOffRefinement(t, store, id, path)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for run := 1; run <= 2; run++ {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutomaticRefinementRetryCrashChild$", "-test.v")
		child.Env = append(os.Environ(), crashChildDB+"="+db)
		out, err := child.CombinedOutput()
		cancel()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("start %d: %v\n%s", run, err, out)
		}
		t.Logf("start %d exited %d\n%s", run, code, out)
		if code != 0 && code != 73 {
			t.Fatalf("start %d exited %d", run, code)
		}
	}
	attempts, err := os.ReadFile(db + ".attempts")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(attempts), "automatic retry\n"); got != 1 {
		t.Fatalf("the refinement was retried automatically %d times across two starts, want once: a refinement that crashes the process would crash it at every restart", got)
	}

	// A third start, in this process: nothing runs, and Retry's warning says why.
	store = openResumeStore(t, db)
	transcriber := &transcriptions{}
	a := start(t.Context(), t, store, dir, transcriber, nil)
	a.settle()
	if got := transcriber.list(); len(got) != 0 {
		t.Fatalf("a third start transcribed %v", got)
	}
	if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementFailed {
		t.Fatalf("refinement %q after the crash in its retry, want failed", got)
	}
	if warning := retryWarning(a.warnings, id); !strings.Contains(warning, errRefinementRetryCutOff.Error()) {
		t.Fatalf("the start shows %q for the session, want Retry's warning that the automatic retry was cut off", warning)
	}

	// A person presses Retry, and the next start shows no warning.
	if err := retryRunner(store, func() string { return dir }, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{}, errors.New("nothing to restore")
	}, a.manager.RefineSession, a.warnings).retryRefinement(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementCompleted {
		t.Fatalf("refinement %q after a person's Retry, want completed", got)
	}
	a.stop(2*time.Second, lifecyclePublishWait)
	next := start(t.Context(), t, store, dir, transcriber, nil)
	next.settle()
	if warning := retryWarning(next.warnings, id); warning != "" {
		t.Fatalf("after a person's Retry the next start still shows %q", warning)
	}
}

// TestAStopDuringTheStartsRetriesKeepsTheRest fails if a stop while the
// start retries the refinements a stop cut off drops the ones it has not
// reached: they must run at the next start. The retry the stop cut off is not
// run again on its own.
func TestAStopDuringTheStartsRetriesKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "queue.db")
	store := openResumeStore(t, db)
	first, second := "20261120080131", "20261120081500"
	for _, id := range []string{first, second} {
		cutOffRefinement(t, store, id, endedResumeSession(t, store, dir, id))
	}

	// The first start: a stop comes during the first retry.
	entered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var transcribed []string
	a := start(t.Context(), t, store, dir, batchFunc(func(ctx context.Context, path string) (transcribe.BatchResult, error) {
		mu.Lock()
		transcribed = append(transcribed, filepath.Base(path))
		mu.Unlock()
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return transcribe.BatchResult{}, ctx.Err()
	}), nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first start retried nothing")
	}
	a.stop(2*time.Second, lifecyclePublishWait)
	if !slices.Equal(transcribed, []string{first + ".mp3"}) {
		t.Fatalf("the stopped start transcribed %v, want only the first", transcribed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The next start retries the second, and not the first.
	store = openResumeStore(t, db)
	transcriber := &transcriptions{}
	next := start(t.Context(), t, store, dir, transcriber, nil)
	next.settle()
	if got := transcriber.list(); !slices.Equal(got, []string{second + ".mp3"}) {
		t.Fatalf("the next start transcribed %v, want the second session's recording, which the stop kept it from", got)
	}
	if got := refinementOf(t, store, second).RefinementStatus; got != storage.RefinementCompleted {
		t.Errorf("the second session's refinement %q, want completed", got)
	}
	if got := refinementOf(t, store, first).RefinementStatus; got != storage.RefinementFailed {
		t.Errorf("the first session's refinement %q, want failed", got)
	}
	if warning := retryWarning(next.warnings, first); !strings.Contains(warning, errRefinementRetryCutOff.Error()) {
		t.Errorf("the first session shows %q, want Retry's warning that its automatic retry was cut off", warning)
	}
}

// TestTheStartsRetriesSkipDeletedSessionsAndSegments fails if the start's
// automatic retry refines a session deleted since the stop, or brings back a
// segment deleted since; a session kept is the control, and is refined.
func TestTheStartsRetriesSkipDeletedSessionsAndSegments(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "privacy.db")
	store := openResumeStore(t, db)
	for _, id := range []string{"gone", "segment-deleted", "kept"} {
		path := endedResumeSession(t, store, dir, id)
		if id == "segment-deleted" {
			if err := store.AppendSegment(id, transcribe.Segment{Text: "deleted synthetic words", Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`UPDATE segments SET untimed = 1 WHERE session_id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
		cutOffRefinement(t, store, id, path)
	}
	if _, err := store.DeleteSession("gone", dir); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("segment-deleted")
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments %v: %v", segments, err)
	}
	if err := store.DeleteRefinedSegmentText("segment-deleted", segments[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openResumeStore(t, db)
	transcriber := &transcriptions{}
	start(t.Context(), t, store, dir, transcriber, nil).settle()
	if got := transcriber.list(); !slices.Equal(got, []string{"kept.mp3"}) {
		t.Fatalf("the start transcribed %v, want only kept.mp3", got)
	}
	if _, err := store.GetSession("gone"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the deleted session came back: %v", err)
	}
	if segments, err := store.GetSegments("segment-deleted"); err != nil || len(segments) != 0 {
		t.Fatalf("the deleted segment came back: %v (%v)", segments, err)
	}
}

// TestTheStartsRetryKeepsATextDeletion fails if the start's automatic retry
// of a refinement puts back words a "Delete text" removed before the stop:
// the transcriber hears them again, and the stored transcript must not have
// them. Without the deletion, the control keeps them.
func TestTheStartsRetryKeepsATextDeletion(t *testing.T) {
	for _, deleteText := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "deleted"}[deleteText], func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "words.db")
			store := openResumeStore(t, db)
			path := endedResumeSession(t, store, dir, "recorded")
			hash, err := recordingname.SHA256(path)
			if err != nil {
				t.Fatal(err)
			}
			words := []storage.RefinedWord{
				{RecordingPath: path, RecordingSHA256: hash, Ordinal: 0, Text: "retained", Speaker: 0, Start: 0, End: 0.5},
				{RecordingPath: path, RecordingSHA256: hash, Ordinal: 1, Text: "synthetic-secret", Speaker: 1, Start: 1, End: 1.5},
				{RecordingPath: path, RecordingSHA256: hash, Ordinal: 2, Text: "neighbor", Speaker: 0, Start: 2, End: 2.5},
			}
			generation := cutOffRefinement(t, store, "recorded", path)
			if _, err := store.PublishRefinementAtGeneration("recorded", storage.RefinedTranscript(words), "fixture", words, storage.BuildRefinedSegments(words), generation); err != nil {
				t.Fatal(err)
			}
			if deleteText {
				segments, err := store.GetSegments("recorded")
				if err != nil || len(segments) != 3 {
					t.Fatalf("segments %v: %v", segments, err)
				}
				if err := store.DeleteRefinedSegmentText("recorded", segments[1].ID, refinementOf(t, store, "recorded").RefinedRevision); err != nil {
					t.Fatal(err)
				}
			}
			cutOffRefinement(t, store, "recorded", path)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			store = openResumeStore(t, db)
			start(t.Context(), t, store, dir, batchFunc(func(context.Context, string) (transcribe.BatchResult, error) {
				return transcribe.BatchResult{Model: "fixture", Words: []transcribe.TimestampedWord{
					{Text: "retained", Speaker: 0, Start: 0, End: 0.5},
					{Text: "synthetic-secret", Speaker: 1, Start: 1, End: 1.5},
					{Text: "neighbor", Speaker: 0, Start: 2, End: 2.5},
				}}, nil
			}), nil).settle()
			got := refinementOf(t, store, "recorded")
			if got.RefinementStatus != storage.RefinementCompleted || strings.Contains(got.CanonicalTranscript, "synthetic-secret") == deleteText || !strings.Contains(got.CanonicalTranscript, "neighbor") {
				t.Fatalf("after the start's retry: status %s, transcript %q", got.RefinementStatus, got.CanonicalTranscript)
			}
		})
	}
}

// TestADeletionWhileARetryIsOwedCancelsIt fails if a "Delete text" of an
// untimed segment of a merged session, made while the start owes its
// refinement a retry, leaves the next start to run it. Made while the
// refinement still runs, the same deletion fails it, and nothing runs it on
// its own; made while the retry is owed, after a stop before the start reached
// it, it must end the retry the same way. The real merge of a session without
// a recording and one with; the control, not deleted from, is retried.
func TestADeletionWhileARetryIsOwedCancelsIt(t *testing.T) {
	for _, mode := range []string{"control-not-deleted", "deleted-while-running", "deleted-while-owed"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "merged.db")
			store := openResumeStore(t, db)
			at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
			if err := store.CreateSession("without-recording", at); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendSegment("without-recording", transcribe.Segment{Text: "untimed synthetic words to delete", Timestamp: at}); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession("without-recording", at.Add(time.Minute), ""); err != nil {
				t.Fatal(err)
			}
			path := endedResumeSession(t, store, dir, "with-recording")
			if err := store.MergeSessions("merged", []string{"without-recording", "with-recording"}, at, at.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			cutOffRefinement(t, store, "merged", path)
			if mode == "deleted-while-owed" {
				stopBeforeTheRetries(t, store, "merged")
			}
			if mode != "control-not-deleted" {
				segments, err := store.GetSegments("merged")
				if err != nil || len(segments) != 1 {
					t.Fatalf("the merged session's segments %v: %v", segments, err)
				}
				if err := store.DeleteRefinedSegmentText("merged", segments[0].ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			store = openResumeStore(t, db)
			transcriber := &transcriptions{}
			start(t.Context(), t, store, dir, transcriber, nil).settle()
			want := 0
			if mode == "control-not-deleted" {
				want = 1
			}
			if got := len(transcriber.list()); got != want {
				t.Fatalf("the next start transcribed %d recordings, want %d", got, want)
			}
		})
	}
}

// TestAnOwedRetryRunsAfterGCTookTheRecording fails if GC, which runs beside
// the start's retries, ends a retry it has not reached by taking the
// session's synced recording: the refinement becomes no_audio, and the Retry
// action restores the recording from Drive and runs it, as a pressed Retry
// does. The control keeps its recording.
func TestAnOwedRetryRunsAfterGCTookTheRecording(t *testing.T) {
	for _, collected := range []bool{false, true} {
		t.Run(map[bool]string{false: "control-recording-kept", true: "recording-taken"}[collected], func(t *testing.T) {
			dir := t.TempDir()
			store := openResumeStore(t, filepath.Join(dir, "gc.db"))
			const id = "20261120080131"
			path := endedResumeSession(t, store, dir, id)
			if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ?, gdrive_folder_id = ?, summary_status = ? WHERE id = ?`,
				storage.SyncStateSynced, storage.SyncSynced, "synthetic-folder", storage.SummaryCompleted, id); err != nil {
				t.Fatal(err)
			}
			cutOffRefinement(t, store, id, path)
			stopBeforeTheRetries(t, store, id)
			if collected {
				if _, changed, err := store.MarkAudioRemoved(id, path, "", false, time.Now()); err != nil || !changed {
					t.Fatalf("GC's mark: changed=%t, %v", changed, err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}

			var restored int
			transcriber := &transcriptions{}
			startWith(t.Context(), t, store, dir, transcriber, nil, func(context.Context, storage.Session) (deletionRecordingSet, error) {
				restored++
				copied := filepath.Join(dir, "restored", filepath.Base(path))
				if err := os.MkdirAll(filepath.Dir(copied), 0o700); err != nil {
					return deletionRecordingSet{}, err
				}
				if err := os.WriteFile(copied, []byte("synthetic fixture; no room audio"), 0o600); err != nil {
					return deletionRecordingSet{}, err
				}
				return deletionRecordingSet{Paths: []string{copied}, Downloaded: []string{copied}}, nil
			}).settle()
			wantRestored := 0
			if collected {
				wantRestored = 1
			}
			if got := transcriber.list(); len(got) != 1 || restored != wantRestored {
				t.Fatalf("the start transcribed %v and restored %d recordings, want one transcription and %d restores", got, restored, wantRestored)
			}
			if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementCompleted {
				t.Fatalf("the refinement is %q after the start's retry, want completed", got)
			}
		})
	}
}
