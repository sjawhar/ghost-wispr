package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type refinedRunnerFixture struct {
	store     *storage.SQLiteStore
	recording string
	segmentID int64
	revision  int
}

func newRefinedRunnerFixture(t *testing.T) refinedRunnerFixture {
	t.Helper()
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256(recording)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(time.Minute), recording); err != nil {
		t.Fatal(err)
	}
	words := []storage.RefinedWord{
		{RecordingPath: recording, RecordingSHA256: hash, Ordinal: 0, Text: "Cut.", Start: 0, End: 1},
		{RecordingPath: recording, RecordingSHA256: hash, Ordinal: 1, Text: "Keep.", Start: 1, End: 2},
	}
	revision, err := store.PublishRefinementAtGeneration("s", storage.RefinedTranscript(words), "test", words, storage.BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	return refinedRunnerFixture{store: store, recording: recording, segmentID: segments[0].ID, revision: revision}
}

func (f refinedRunnerFixture) runner() segmentDeleteRunner {
	return segmentDeleteRunner{
		store: f.store,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{f.recording}}, nil
		},
		splice: func(context.Context, string, float64, float64) (string, error) { return "after", nil },
	}
}

func TestDeleteRefinedAudioRefusesPendingSplice(t *testing.T) {
	fixture := newRefinedRunnerFixture(t)
	before, err := fileSHA256(fixture.recording)
	if err != nil {
		t.Fatal(err)
	}
	span := storage.DeletedSpan{AudioPath: fixture.recording, Start: 0, End: 1, SHA256Before: before, SHA256After: "after"}
	if err := fixture.store.DeleteRefinedSegmentAudio("s", fixture.segmentID, fixture.revision, &span); err != nil {
		t.Fatal(err)
	}
	runner := fixture.runner()
	if err := runner.DeleteRefinedAudio(context.Background(), "s", fixture.segmentID, fixture.revision+1); !errors.Is(err, storage.ErrSegmentSplicePending) {
		t.Fatalf("delete with pending splice = %v, want %v", err, storage.ErrSegmentSplicePending)
	}
}

func TestDeleteRefinedAudioChecksHashBeforeSplice(t *testing.T) {
	fixture := newRefinedRunnerFixture(t)
	if err := os.WriteFile(fixture.recording, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	spliced := false
	runner := fixture.runner()
	runner.splice = func(context.Context, string, float64, float64) (string, error) {
		spliced = true
		return "after", nil
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "s", fixture.segmentID, fixture.revision); err == nil {
		t.Fatal("delete accepted a recording changed since refinement")
	}
	if spliced {
		t.Fatal("delete spliced a recording whose hash no longer matches refinement")
	}
	contents, err := os.ReadFile(fixture.recording)
	if err != nil || string(contents) != "changed" {
		t.Fatalf("recording after hash refusal = %q (%v), want unchanged bytes", contents, err)
	}
}

func TestDeleteRefinedTextRefusesRecovery(t *testing.T) {
	fixture := newRefinedRunnerFixture(t)
	deleteLocks.setRecovering("s", true)
	t.Cleanup(func() { deleteLocks.setRecovering("s", false) })
	runner := fixture.runner()
	if err := runner.DeleteRefinedText(context.Background(), "s", fixture.segmentID, fixture.revision); !errors.Is(err, storage.ErrSegmentSplicePending) {
		t.Fatalf("text delete during recovery = %v, want %v", err, storage.ErrSegmentSplicePending)
	}
}

func TestDeleteRefinedAudioRefusesRecoveryWithoutWaitingForTheRecoveryLock(t *testing.T) {
	fixture := newRefinedRunnerFixture(t)
	deleteLocks.setRecovering("s", true)
	t.Cleanup(func() { deleteLocks.setRecovering("s", false) })
	unlock := deleteLocks.lock("s")
	t.Cleanup(unlock)
	runner := fixture.runner()
	result := make(chan error, 1)
	go func() {
		result <- runner.DeleteRefinedAudio(context.Background(), "s", fixture.segmentID, fixture.revision)
	}()
	select {
	case err := <-result:
		if !errors.Is(err, storage.ErrSegmentSplicePending) {
			t.Fatalf("audio delete during recovery = %v, want %v", err, storage.ErrSegmentSplicePending)
		}
		var restoring interface{ RecordingRestoreInProgress() }
		if !errors.As(err, &restoring) {
			t.Fatalf("audio delete during recovery = %v, want recording restore marker", err)
		}
	case <-time.After(time.Second):
		t.Fatal("audio delete waited for the recovery lock instead of refusing at once")
	}
}

func TestDeleteRefinedAudioRefusesKeptRawRecording(t *testing.T) {
	fixture := newRefinedRunnerFixture(t)
	audioDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(audioDir, "s.pcm"), []byte("raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := fixture.runner()
	runner.audioDir = audioDir
	if err := runner.DeleteRefinedAudio(context.Background(), "s", fixture.segmentID, fixture.revision); !errors.Is(err, server.ErrSegmentRawAudioKept) {
		t.Fatalf("delete with kept raw recording = %v, want %v", err, server.ErrSegmentRawAudioKept)
	}
}

func TestDeleteRefinedAudioRemovesDownloadedRecordingAfterRefusal(t *testing.T) {
	fixture := newRefinedRunnerFixture(t)
	downloaded := filepath.Join(t.TempDir(), "downloaded.mp3")
	if err := os.WriteFile(downloaded, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := fixture.runner()
	runner.ensureRecordings = func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: []string{downloaded}, Downloaded: []string{downloaded}}, nil
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "s", fixture.segmentID, fixture.revision); err == nil {
		t.Fatal("delete accepted downloaded recording with the wrong hash")
	}
	if _, err := os.Stat(downloaded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("downloaded recording remains after refusal: %v", err)
	}
}
