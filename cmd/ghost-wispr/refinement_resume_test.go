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

// wordsFor answers every recording with one word.
func wordsFor(word string) batchFunc {
	return func(context.Context, string) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{Model: "fixture", Words: []transcribe.TimestampedWord{{Text: word, Start: 0, End: 1}}}, nil
	}
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

// realRetry is the app's "Retry refinement" action on store, with the real
// session manager refining through transcriber; the recordings are on this
// unit, so it restores none.
func realRetry(store *storage.SQLiteStore, dir string, transcriber transcribe.BatchTranscriber, warnings *warningsRegistry) func(context.Context, string) error {
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	return retryRunner(store, func() string { return dir }, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{}, errors.New("the recording is on this unit: nothing to restore")
	}, manager.RefineSession, warnings).retryRefinement
}

// startAgain runs the start's two calls main makes for the refinements a
// stop cut off, in main's order (TestMainWiresTheStopsRecovery pins main to
// them): the recovery, then the retries.
func startAgain(ctx context.Context, t *testing.T, store *storage.SQLiteStore, warnings *warningsRegistry, retry func(context.Context, string) error) {
	t.Helper()
	if err := store.RecoverStaleRefinements(); err != nil {
		t.Fatal(err)
	}
	resumeStoppedRefinements(ctx, store, warnings, retry)
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

// TestARefinementAStopCutOffRunsAgainAtTheNextStart fails if a refinement the
// app was running when it stopped stays failed at the next start. On the
// office Pi on 2026-11-20 a restart during a session's refinement left it
// "failed": the start marked every refinement left running as failed and ran
// none of them again, so a meeting cut off that way stayed unrefined until a
// person pressed Retry. Here the start retries it with the real "Retry
// refinement" action and session manager, and leaves a completed refinement
// alone.
func TestARefinementAStopCutOffRunsAgainAtTheNextStart(t *testing.T) {
	dir := t.TempDir()
	store := openResumeStore(t, filepath.Join(dir, "resume.db"))
	cutOff, done := "20261120080131", "20261120000356"
	cutOffRefinement(t, store, cutOff, endedResumeSession(t, store, dir, cutOff))
	endedResumeSession(t, store, dir, done)
	if err := store.UpdateRefinementAtGeneration(done, "", storage.RefinementCompleted, "", 0); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var transcribed []string
	warnings := newWarningsRegistry()
	startAgain(t.Context(), t, store, warnings, realRetry(store, dir, batchFunc(func(ctx context.Context, path string) (transcribe.BatchResult, error) {
		mu.Lock()
		transcribed = append(transcribed, filepath.Base(path))
		mu.Unlock()
		return wordsFor("resumed")(ctx, path)
	}), warnings))

	if !slices.Equal(transcribed, []string{cutOff + ".mp3"}) {
		t.Fatalf("the start transcribed %v, want only the recording of the session the stop cut off", transcribed)
	}
	for _, id := range []string{cutOff, done} {
		if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementCompleted {
			t.Errorf("session %s refinement %q after the start, want completed", id, got)
		}
	}
	if pending, exhausted, err := store.RefinementResumes(); err != nil || len(pending)+len(exhausted) != 0 {
		t.Fatalf("after the retry the store owes pending %v, exhausted %v (%v), want nothing", pending, exhausted, err)
	}
}

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
	warnings := newWarningsRegistry()
	startAgain(context.Background(), t, store, warnings, realRetry(store, filepath.Dir(db), batchFunc(func(context.Context, string) (transcribe.BatchResult, error) {
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
	}), warnings))
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

	for start := 1; start <= 2; start++ {
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
			t.Fatalf("start %d: %v\n%s", start, err, out)
		}
		t.Logf("start %d exited %d\n%s", start, code, out)
		if code != 0 && code != 73 {
			t.Fatalf("start %d exited %d", start, code)
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
	warnings := newWarningsRegistry()
	startAgain(t.Context(), t, store, warnings, realRetry(store, dir, batchFunc(func(context.Context, string) (transcribe.BatchResult, error) {
		t.Error("a third start retried the refinement whose automatic retry crashed the process")
		return transcribe.BatchResult{}, errors.New("not retried")
	}), warnings))
	if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementFailed {
		t.Fatalf("refinement %q after the crash in its retry, want failed", got)
	}
	if warning := retryWarning(warnings, id); !strings.Contains(warning, errRefinementRetryCutOff.Error()) {
		t.Fatalf("the start shows %q for the session, want Retry's warning that the automatic retry was cut off", warning)
	}

	// A person's Retry runs it, and the next start shows no warning.
	if err := realRetry(store, dir, wordsFor("retried"), warnings)(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := refinementOf(t, store, id).RefinementStatus; got != storage.RefinementCompleted {
		t.Fatalf("refinement %q after a person's Retry, want completed", got)
	}
	warnings = newWarningsRegistry()
	startAgain(t.Context(), t, store, warnings, realRetry(store, dir, wordsFor("again"), warnings))
	if warning := retryWarning(warnings, id); warning != "" {
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
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var mu sync.Mutex
	var transcribed []string
	transcriber := batchFunc(func(ctx context.Context, path string) (transcribe.BatchResult, error) {
		mu.Lock()
		transcribed = append(transcribed, filepath.Base(path))
		mu.Unlock()
		stop()
		return transcribe.BatchResult{}, ctx.Err()
	})
	warnings := newWarningsRegistry()
	startAgain(ctx, t, store, warnings, realRetry(store, dir, transcriber, warnings))
	if !slices.Equal(transcribed, []string{first + ".mp3"}) {
		t.Fatalf("the stopped start transcribed %v, want only the first", transcribed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The next start retries the second, and not the first.
	store = openResumeStore(t, db)
	transcribed = nil
	warnings = newWarningsRegistry()
	startAgain(t.Context(), t, store, warnings, realRetry(store, dir, batchFunc(func(ctx context.Context, path string) (transcribe.BatchResult, error) {
		mu.Lock()
		transcribed = append(transcribed, filepath.Base(path))
		mu.Unlock()
		return wordsFor("resumed")(ctx, path)
	}), warnings))
	if !slices.Equal(transcribed, []string{second + ".mp3"}) {
		t.Fatalf("the next start transcribed %v, want the second session's recording, which the stop kept it from", transcribed)
	}
	if got := refinementOf(t, store, second).RefinementStatus; got != storage.RefinementCompleted {
		t.Errorf("the second session's refinement %q, want completed", got)
	}
	if got := refinementOf(t, store, first).RefinementStatus; got != storage.RefinementFailed {
		t.Errorf("the first session's refinement %q, want failed", got)
	}
	if warning := retryWarning(warnings, first); !strings.Contains(warning, errRefinementRetryCutOff.Error()) {
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
	var transcribed []string
	warnings := newWarningsRegistry()
	startAgain(t.Context(), t, store, warnings, realRetry(store, dir, batchFunc(func(ctx context.Context, path string) (transcribe.BatchResult, error) {
		transcribed = append(transcribed, filepath.Base(path))
		return wordsFor("retained")(ctx, path)
	}), warnings))
	if !slices.Equal(transcribed, []string{"kept.mp3"}) {
		t.Fatalf("the start transcribed %v, want only kept.mp3", transcribed)
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
			warnings := newWarningsRegistry()
			startAgain(t.Context(), t, store, warnings, realRetry(store, dir, batchFunc(func(context.Context, string) (transcribe.BatchResult, error) {
				return transcribe.BatchResult{Model: "fixture", Words: []transcribe.TimestampedWord{
					{Text: "retained", Speaker: 0, Start: 0, End: 0.5},
					{Text: "synthetic-secret", Speaker: 1, Start: 1, End: 1.5},
					{Text: "neighbor", Speaker: 0, Start: 2, End: 2.5},
				}}, nil
			}), warnings))
			got := refinementOf(t, store, "recorded")
			if got.RefinementStatus != storage.RefinementCompleted || strings.Contains(got.CanonicalTranscript, "synthetic-secret") == deleteText || !strings.Contains(got.CanonicalTranscript, "neighbor") {
				t.Fatalf("after the start's retry: status %s, transcript %q", got.RefinementStatus, got.CanonicalTranscript)
			}
		})
	}
}
