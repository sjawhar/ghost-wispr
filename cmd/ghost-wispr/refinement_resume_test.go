package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestARefinementAStopCutOffRunsAgainAtTheNextStart fails if a refinement the
// app was running when it stopped stays failed at the next start. On the
// office Pi on 2026-11-20 a restart during a session's refinement left it
// "failed": the start marked every refinement left running as failed
// (storage.RecoverStaleRefinements) and ran none of them again, so a meeting
// cut off that way stayed unrefined until a person pressed Retry. The start's
// two steps run here as main runs them, with the real "Retry refinement"
// action: the refinement the stop cut off runs again and completes, and a
// session whose refinement had completed is left alone.
func TestARefinementAStopCutOffRunsAgainAtTheNextStart(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	started := time.Date(2026, 11, 20, 8, 1, 25, 0, time.UTC)
	session := func(id string) string {
		t.Helper()
		audio := filepath.Join(dir, id+".mp3")
		if err := os.WriteFile(audio, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(21*time.Second), audio); err != nil {
			t.Fatal(err)
		}
		return audio
	}
	cutOff, done := "20261120080131", "20261120000356"
	// The stopped run had claimed this refinement, and died inside it.
	if _, claimed, err := store.ClaimRefinement(cutOff, session(cutOff)); err != nil || !claimed {
		t.Fatalf("claim the refinement the stop cut off: claimed=%t, %v", claimed, err)
	}
	session(done)
	if err := store.UpdateRefinementAtGeneration(done, "", storage.RefinementCompleted, "", 0); err != nil {
		t.Fatal(err)
	}

	refined := make(chan string, 4)
	retry := retryRunner(store, func() string { return dir }, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{}, errors.New("the recording is on this unit: nothing to restore")
	}, func(_ context.Context, id, audioPath string, _ time.Time) error {
		generation, claimed, err := store.ClaimRefinement(id, audioPath)
		if err != nil {
			return err
		}
		if !claimed {
			return errors.New("the refinement was not claimed")
		}
		words := []storage.RefinedWord{{RecordingPath: audioPath, RecordingSHA256: "audio", Ordinal: 0, Text: "Resumed words.", Start: 0, End: 1}}
		if _, err := store.PublishRefinementAtGeneration(id, storage.RefinedTranscript(words), "test", words, storage.BuildRefinedSegments(words), generation); err != nil {
			return err
		}
		refined <- id
		return nil
	}, newWarningsRegistry()).retryRefinement

	// The start, as main runs it: the recovery, then, once the recording
	// directories are cleaned up, the resume.
	stopped, err := store.RecoverStaleRefinements()
	if err != nil {
		t.Fatal(err)
	}
	resumeStoppedRefinements(t.Context(), stopped, retry, t.Logf)

	close(refined)
	var ran []string
	for id := range refined {
		ran = append(ran, id)
	}
	if !slices.Equal(ran, []string{cutOff}) {
		t.Fatalf("the start refined %v, want only the session the stop cut off, %s", ran, cutOff)
	}
	for id, want := range map[string]string{cutOff: storage.RefinementCompleted, done: storage.RefinementCompleted} {
		got, err := store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.RefinementStatus != want {
			t.Errorf("session %s refinement %q after the start, want %q", id, got.RefinementStatus, want)
		}
	}
}

// TestResumingStoppedRefinementsGoesOnPastAFailure fails if a resumed
// refinement that fails stops the ones after it, if they run out of order, or
// if a resume runs anything once its context has ended.
func TestResumingStoppedRefinementsGoesOnPastAFailure(t *testing.T) {
	var ran []string
	retry := func(_ context.Context, id string) error {
		ran = append(ran, id)
		if id == "20261120080131" {
			return errors.New("Deepgram refused the batch request")
		}
		return nil
	}
	resumeStoppedRefinements(t.Context(), []string{"20261120080131", "20261120081500"}, retry, t.Logf)
	if want := []string{"20261120080131", "20261120081500"}; !slices.Equal(ran, want) {
		t.Fatalf("resumed %v, want %v", ran, want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ran = nil
	resumeStoppedRefinements(ctx, []string{"20261120080131"}, retry, t.Logf)
	if len(ran) != 0 {
		t.Fatalf("a resume whose context had ended ran %v", ran)
	}
}
