package main

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// cleanCutTranscriber hears, at every cut, one word no segment has, so the
// check after a cut accepts the first cut.
func cleanCutTranscriber(context.Context, string, string) ([]transcribe.TimestampedWord, error) {
	return []transcribe.TimestampedWord{{Text: "checkword", Start: 0, End: math.MaxFloat64}}, nil
}

var errTestRunnerHasNo = errors.New("this test runner has none")

// withTestDefaults gives r, which has its store, a fake for each dependency
// it leaves nil, and checks it as the app's runner is checked. The fakes
// change nothing: no recordings, no splice, a duration of -1 (unknown), a
// refinement and a summary that do nothing, and a cut check that accepts.
func withTestDefaults(r *segmentDeleteRunner) *segmentDeleteRunner {
	if r.audioDir == nil {
		r.audioDir = func() string { return "" }
	}
	if r.ensureRecordings == nil {
		r.ensureRecordings = func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{}, errTestRunnerHasNo
		}
	}
	if r.duration == nil {
		r.duration = func(context.Context, string) (float64, error) { return -1, nil }
	}
	if r.splice == nil {
		r.splice = func(context.Context, string, float64, float64) (string, error) { return "", errTestRunnerHasNo }
	}
	if r.refine == nil {
		r.refine = func(context.Context, string, string, time.Time) error { return nil }
	}
	if r.summarize == nil {
		r.summarize = func(context.Context, string, time.Time) error { return nil }
	}
	if r.transcribeCut == nil {
		r.transcribeCut = cleanCutTranscriber
	}
	if r.refused == nil {
		r.refused = newRefusedCuts(r.store)
	}
	if r.warnings == nil {
		r.warnings = newWarningsRegistry()
	}
	runner, err := newSegmentDeleteRunner(r)
	if err != nil {
		panic(err)
	}
	return runner
}

// testDeleteRunner is r with test fakes for the dependencies it leaves nil;
// a runner with no store gets one of its own.
func testDeleteRunner(t *testing.T, r *segmentDeleteRunner) *segmentDeleteRunner {
	t.Helper()
	if r.store == nil {
		store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "runner.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		r.store = store
	}
	return withTestDefaults(r)
}

// recoveryRunner is a runner on store and audioDir that recovers with
// splice and refine; a nil one is a fake.
func recoveryRunner(store *storage.SQLiteStore, audioDir string, splice func(context.Context, string, float64, float64) (string, error), refine func(context.Context, string, string, time.Time) error) *segmentDeleteRunner {
	return withTestDefaults(&segmentDeleteRunner{store: store, audioDir: func() string { return audioDir }, splice: splice, refine: refine})
}

// retryRunner is a runner on store and audioDir that restores recordings
// with ensure, refines with refine, and keeps a failed retry's warning in
// warnings, for "Retry refinement".
func retryRunner(store *storage.SQLiteStore, audioDir func() string, ensure func(context.Context, storage.Session) (deletionRecordingSet, error), refine func(context.Context, string, string, time.Time) error, warnings *warningsRegistry) *segmentDeleteRunner {
	return withTestDefaults(&segmentDeleteRunner{store: store, audioDir: audioDir, ensureRecordings: ensure, refine: refine, warnings: warnings})
}

// noCutsGuard is a recording restore guard with no cuts and no sources: a
// restore then checks no recording against a cut hash.
type noCutsGuard struct{}

func (noCutsGuard) LatestRecordingSHA256(string, string) (string, bool, error) { return "", false, nil }

func (noCutsGuard) RefinementRecordingSources(string) ([]storage.RefinementRecordingSource, error) {
	return nil, nil
}

// TestADeleteRunnerWithoutACutCheckDoesNotStart fails if the app could run
// "Delete text and audio" with no transcription to check each cut, or with
// any other dependency missing: the runner is refused when it is built.
func TestADeleteRunnerWithoutACutCheckDoesNotStart(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "runner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runner := withTestDefaults(&segmentDeleteRunner{store: store})
	runner.transcribeCut = nil
	if _, err := newSegmentDeleteRunner(runner); err == nil || err.Error() != "segment deletion is missing transcribeCut" {
		t.Fatalf("runner with no cut check: err = %v, want it refused naming transcribeCut", err)
	}
	if _, err := newSegmentDeleteRunner(&segmentDeleteRunner{}); err == nil {
		t.Fatal("an empty runner was accepted")
	}
}
