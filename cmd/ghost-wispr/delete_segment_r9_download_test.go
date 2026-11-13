package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type r9MergedCutFixture struct {
	store    *storage.SQLiteStore
	dir      string
	audioA   string
	audioC   string
	segmentA int64
	revision int
	drive    *deletionDriveFake
	deletedC storage.DeletedSpan
}

func newR9MergedCutFixture(t *testing.T) r9MergedCutFixture {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "merged-cut.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	audioA, audioC := filepath.Join(dir, "a.mp3"), filepath.Join(dir, "c.mp3")
	aBytes, cBytes := []byte("recording A"), []byte("recording C before cut")
	for path, contents := range map[string][]byte{audioA: aBytes, audioC: cBytes} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, source := range []struct{ id, path string }{{"a", audioA}, {"c", audioC}} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(source.id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(source.id, at.Add(time.Minute), source.path); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"a", "c"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	aHash, cBefore := hashBytesForDeleteTest(aBytes), hashBytesForDeleteTest(cBytes)
	words := []storage.RefinedWord{
		{RecordingPath: audioA, RecordingSHA256: aHash, RecordingOrdinal: 0, Ordinal: 0, Text: "delete A", Start: 1, End: 2},
		{RecordingPath: audioC, RecordingSHA256: cBefore, RecordingOrdinal: 1, Ordinal: 0, Text: "deleted C", Start: 1, End: 2},
	}
	generation, err := store.DeletionGeneration("merged")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.PublishRefinementAtGeneration("merged", storage.RefinedTranscript(words), "test", words, storage.BuildRefinedSegments(words), generation)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	var segmentA int64
	for _, segment := range segments {
		if filepath.Base(segment.RecordingPath) == filepath.Base(audioA) {
			segmentA = segment.ID
		}
	}
	if segmentA == 0 {
		t.Fatal("control: no refined segment for A")
	}
	deletedC := storage.DeletedSpan{
		SessionID: "merged", AudioPath: audioC, Start: 1, End: 2,
		SHA256Before: cBefore, SHA256After: hashBytesForDeleteTest([]byte("recording C after cut")),
	}
	if _, err := store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at, splice_complete)
		VALUES(?, ?, ?, ?, ?, ?, ?, 1)`, deletedC.SessionID, deletedC.AudioPath, deletedC.Start, deletedC.End, deletedC.SHA256Before, deletedC.SHA256After, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder', drive_replace_pending = 1 WHERE id = 'merged'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(audioC); err != nil {
		t.Fatal(err)
	}
	drive := &deletionDriveFake{
		files: []*drive.File{
			{Name: recordingname.FileName(audioA), MimeType: "audio/mpeg", Sha256Checksum: aHash},
			{Name: recordingname.FileName(audioC), MimeType: "audio/mpeg", Sha256Checksum: cBefore},
		},
		downloads: map[string][]byte{
			recordingname.FileName(audioA): aBytes,
			recordingname.FileName(audioC): cBytes,
		},
	}
	return r9MergedCutFixture{store: store, dir: dir, audioA: audioA, audioC: audioC, segmentA: segmentA, revision: revision, drive: drive, deletedC: deletedC}
}

// TestR9DeleteAudioDoesNotRestorePrecutMergedRecording fails if deleting a
// segment from A downloads Drive's pre-cut C while C's replacement is owed.
func TestR9DeleteAudioDoesNotRestorePrecutMergedRecording(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	runner := &segmentDeleteRunner{
		store:    fixture.store,
		audioDir: fixture.dir,
		ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
			return restoreDeletionRecordings(ctx, fixture.store, &session, fixture.dir, fixture.drive, "")
		},
		duration: func(context.Context, string) (float64, error) { return 10, nil },
		splice:   fakeSplice,
	}

	err := runner.DeleteRefinedAudio(context.Background(), "merged", fixture.segmentA, fixture.revision)
	if err == nil {
		t.Fatal("Delete text and audio restored C's pre-cut Drive recording")
	}
	for _, name := range fixture.drive.downloaded {
		if name == recordingname.FileName(fixture.audioC) {
			t.Fatal("Delete text and audio downloaded C's pre-cut Drive recording")
		}
	}
}

// TestR9PendingSpliceDoesNotRestorePrecutMergedRecording fails if startup
// recovery downloads an owed recording's pre-cut Drive copy while recovering a
// different recording in the same merged session.
func TestR9PendingSpliceDoesNotRestorePrecutMergedRecording(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	aBefore, err := fileSHA256(fixture.audioA)
	if err != nil {
		t.Fatal(err)
	}
	pending := storage.DeletedSpan{
		SessionID: "merged", AudioPath: fixture.audioA, Start: 1, End: 2,
		SHA256Before: aBefore, SHA256After: hashBytesForDeleteTest([]byte("spliced")),
	}
	if _, err := fixture.store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, pending.SessionID, pending.AudioPath, pending.Start, pending.End, pending.SHA256Before, pending.SHA256After, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	recovered, err := recoverPendingSplice(context.Background(), fixture.store, fixture.dir, fixture.drive, fakeSplice, nil, &pending)
	if err == nil || recovered {
		t.Fatalf("pending splice recovery = recovered:%t err:%v, want refusal", recovered, err)
	}
	for _, name := range fixture.drive.downloaded {
		if name == recordingname.FileName(fixture.audioC) {
			t.Fatal("pending splice recovery downloaded C's pre-cut Drive recording")
		}
	}
}
