package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func newDeletionTestStore(t *testing.T, recording string) (*storage.SQLiteStore, int64, time.Time) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	testSegments := []transcribe.Segment{
		{Speaker: 0, Text: "before bridge", StartTime: 0, EndTime: 1, Timestamp: started},
		{Speaker: 1, Text: "private deletion token", StartTime: 1, EndTime: 2, Timestamp: started.Add(time.Second)},
		{Speaker: 0, Text: "after bridge", StartTime: 2, EndTime: 3, Timestamp: started.Add(2 * time.Second)},
	}
	for i := range testSegments {
		if err := store.AppendSegment("session", testSegments[i]); err != nil {
			t.Fatalf("append segment: %v", err)
		}
	}
	if err := store.EndSession("session", started.Add(3*time.Second), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	return store, segments[1].ID, started
}

func deleteRefinedAudioForTest(t *testing.T, store *storage.SQLiteStore, sessionID string, liveSegmentID int64, span *storage.DeletedSpan) error {
	t.Helper()
	live, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	index := -1
	for i := range live {
		if live[i].ID == liveSegmentID {
			index = i
			break
		}
	}
	if index < 0 {
		return storage.ErrSegmentNotFound
	}
	words := make([]storage.RefinedWord, 0, len(live))
	refined := make([]storage.RefinedSegment, 0, len(live))
	for i := range live {
		segment := &live[i]
		words = append(words, storage.RefinedWord{
			RecordingPath: span.AudioPath, RecordingSHA256: span.SHA256Before, Ordinal: i,
			Text: segment.Text, Speaker: segment.Speaker, Start: segment.StartTime, End: segment.EndTime,
		})
		refined = append(refined, storage.RefinedSegment{
			RecordingPath: span.AudioPath, RecordingSHA256: span.SHA256Before, FirstWordOrdinal: i, LastWordOrdinal: i,
			Text: segment.Text, Speaker: segment.Speaker, Start: segment.StartTime, End: segment.EndTime,
		})
	}
	generation, err := store.DeletionGeneration(sessionID)
	if err != nil {
		return err
	}
	revision, err := store.PublishRefinementAtGeneration(sessionID, storage.RefinedTranscript(words), "test", words, refined, generation)
	if err != nil {
		return err
	}
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		return err
	}
	return store.DeleteRefinedSegmentAudio(sessionID, segments[index].ID, revision, span)
}

func fakeSplice(_ context.Context, path string, _, _ float64) (string, error) {
	if err := os.WriteFile(path, []byte("spliced"), 0o600); err != nil {
		return "", err
	}
	return hashBytesForDeleteTest([]byte("spliced")), nil
}

func hashBytesForDeleteTest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func r4PendingStore(t *testing.T) (*storage.SQLiteStore, string, []int64) {
	t.Helper()
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	if err := deleteRefinedAudioForTest(t, store, "session", segmentID, &storage.DeletedSpan{
		SessionID: "session", AudioPath: recording, Start: 1, End: 2,
		SHA256Before: hashBytesForDeleteTest(original), SHA256After: hashBytesForDeleteTest([]byte("spliced")),
	}); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.GetSegments("session")
	if err != nil {
		t.Fatal(err)
	}
	return store, recording, []int64{remaining[0].ID, remaining[1].ID}
}

type deletionDriveFake struct {
	files      []*drive.File
	downloads  map[string][]byte
	downloaded []string
}

func (f *deletionDriveFake) ListFolderFiles(context.Context, string) ([]*drive.File, error) {
	return f.files, nil
}

func (f *deletionDriveFake) DownloadRecording(_ context.Context, _ string, name, destination string) error {
	f.downloaded = append(f.downloaded, name)
	return os.WriteFile(destination, f.downloads[name], 0o600)
}
