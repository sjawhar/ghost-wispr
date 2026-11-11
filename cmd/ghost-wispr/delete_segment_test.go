package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type deletionWordsFake struct {
	before []audio.Word
	after  []audio.Word
}

func (f deletionWordsFake) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(contents, []byte("spliced")) {
		return f.after, nil
	}
	return f.before, nil
}

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
	for _, segment := range []transcribe.Segment{
		{Speaker: 0, Text: "before bridge", StartTime: 0, EndTime: 1, Timestamp: started},
		{Speaker: 1, Text: "private deletion token", StartTime: 1, EndTime: 2, Timestamp: started.Add(time.Second)},
		{Speaker: 0, Text: "after bridge", StartTime: 2, EndTime: 3, Timestamp: started.Add(2 * time.Second)},
	} {
		if err := store.AppendSegment("session", segment); err != nil {
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

func initialWords() []audio.Word {
	return []audio.Word{
		{Text: "before", Start: 0.1, End: 0.2},
		{Text: "bridge", Start: 0.3, End: 0.4},
		{Text: "private", Start: 1.1, End: 1.2},
		{Text: "deletion", Start: 1.3, End: 1.4},
		{Text: "token", Start: 1.5, End: 1.6},
		{Text: "after", Start: 2.1, End: 2.2},
		{Text: "bridge", Start: 2.3, End: 2.4},
	}
}

func cutWords() []audio.Word {
	return []audio.Word{
		{Text: "before", Start: 0.1, End: 0.2},
		{Text: "bridge", Start: 0.3, End: 0.4},
		{Text: "after", Start: 1.0, End: 1.1},
		{Text: "bridge", Start: 1.2, End: 1.3},
	}
}

func fakeSplice(_ context.Context, path string, _, _ float64) (string, error) {
	if err := os.WriteFile(path, []byte("spliced"), 0o600); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte("spliced"))
	return hex.EncodeToString(hash[:]), nil
}

func TestSegmentDeleteRunnerSplicesAndRefinesTheRecording(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, started := newDeletionTestStore(t, recording)
	var refinedPath string
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) ([]string, error) {
			return []string{recording}, nil
		},
		splice: fakeSplice,
		refine: func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
			if sessionID != "session" || gotStarted != started {
				t.Fatalf("refine called with session=%q started=%s", sessionID, gotStarted)
			}
			refinedPath = audioPath
			return nil
		},
	}

	if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("delete segment: %v", err)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatalf("read spliced recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("recording = %q, want spliced bytes", contents)
	}
	if refinedPath != recording {
		t.Fatalf("refinement read %q, want spliced %q", refinedPath, recording)
	}
}

func TestSegmentDeleteRunnerRefusesSurvivingWordsWithoutChangingRecording(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: append(cutWords(), audio.Word{Text: "private", Start: 0.9, End: 1.0})},
		ensureRecordings: func(context.Context, storage.Session) ([]string, error) {
			return []string{recording}, nil
		},
		splice: fakeSplice,
	}

	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("delete error = %v, want clean-cut refusal", err)
	}
	contents, readErr := os.ReadFile(recording)
	if readErr != nil {
		t.Fatalf("read original recording: %v", readErr)
	}
	if !bytes.Equal(contents, original) {
		t.Fatalf("refused deletion changed recording: got %q, want %q", contents, original)
	}
}

func TestSegmentDeleteRunnerRefusesOverCutWithoutChangingRecording(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()[:2]},
		ensureRecordings: func(context.Context, storage.Session) ([]string, error) {
			return []string{recording}, nil
		},
		splice: fakeSplice,
	}

	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("delete error = %v, want clean-cut refusal", err)
	}
	contents, readErr := os.ReadFile(recording)
	if readErr != nil {
		t.Fatalf("read original recording: %v", readErr)
	}
	if !bytes.Equal(contents, original) {
		t.Fatalf("over-cut refusal changed recording: got %q, want %q", contents, original)
	}
}

func TestSegmentDeleteRunnerRefusesKeptRawRecording(t *testing.T) {
	audioDir := t.TempDir()
	recording := filepath.Join(audioDir, "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	if err := os.WriteFile(filepath.Join(audioDir, "session.pcm"), []byte("raw audio"), 0o600); err != nil {
		t.Fatalf("write raw recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		audioDir:    audioDir,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) ([]string, error) {
			t.Fatal("recordings checked after kept raw audio")
			return nil, nil
		},
		splice: fakeSplice,
	}

	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentRawAudioKept) || !strings.Contains(err.Error(), "session.pcm") {
		t.Fatalf("delete error = %v, want named kept raw recording", err)
	}
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

func TestRestoreDeletionRecordingsDownloadsGCMissingAudioInTimeOrder(t *testing.T) {
	audioDir := t.TempDir()
	first := filepath.Join(audioDir, "20261111120000.mp3")
	second := filepath.Join(audioDir, "20261111130000.mp3")
	if err := os.WriteFile(first, []byte("local audio"), 0o600); err != nil {
		t.Fatalf("write local recording: %v", err)
	}
	drive := &deletionDriveFake{
		files: []*drive.File{
			{Name: "audio-20261111130000.mp3", MimeType: "audio/mpeg"},
			{Name: "transcript.md", MimeType: "application/vnd.google-apps.document"},
		},
		downloads: map[string][]byte{"audio-20261111130000.mp3": []byte("restored audio")},
	}
	recordings, err := restoreDeletionRecordings(context.Background(), &storage.Session{
		AudioPath:      first,
		AudioRemovedAt: time.Now().UTC().Format(time.RFC3339),
		GDriveFolderID: "folder",
	}, audioDir, drive)
	if err != nil {
		t.Fatalf("restore deletion recordings: %v", err)
	}
	if want := []string{first, second}; !slices.Equal(recordings, want) {
		t.Fatalf("recordings = %v, want %v", recordings, want)
	}
	if !slices.Equal(drive.downloaded, []string{"audio-20261111130000.mp3"}) {
		t.Fatalf("downloaded = %v, want missing Drive recording", drive.downloaded)
	}
	contents, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read restored recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("restored audio")) {
		t.Fatalf("restored recording = %q", contents)
	}
}

func TestStartupResplicesAnInterruptedDeletion(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	original := []byte("original")
	if err := os.WriteFile(recording, original, 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, started := newDeletionTestStore(t, recording)
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if segmentID != segments[1].ID {
		t.Fatalf("segment ID = %d, want %d", segmentID, segments[1].ID)
	}
	afterHash := sha256.Sum256([]byte("spliced"))
	if err := store.DeleteSegment("session", segmentID, storage.DeletedSpan{
		SessionID:    "session",
		AudioPath:    recording,
		Start:        1,
		End:          2,
		SHA256Before: hashBytesForDeleteTest(original),
		SHA256After:  hex.EncodeToString(afterHash[:]),
	}); err != nil {
		t.Fatalf("record interrupted delete: %v", err)
	}
	var refined bool
	count, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
		if sessionID != "session" || audioPath != recording || gotStarted != started {
			t.Fatalf("refine recovered splice session=%q audio=%q started=%s", sessionID, audioPath, gotStarted)
		}
		refined = true
		return nil
	})
	if err != nil {
		t.Fatalf("recover pending splice: %v", err)
	}
	if count != 1 || !refined {
		t.Fatalf("recovered %d splices, refined=%v; want one and true", count, refined)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatalf("read re-spliced recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("re-spliced recording = %q", contents)
	}
	pending, err := store.PendingSplices()
	if err != nil {
		t.Fatalf("check completed splice: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending splices after recovery = %+v", pending)
	}
}

func hashBytesForDeleteTest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}
