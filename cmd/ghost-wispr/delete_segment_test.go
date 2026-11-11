package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
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
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
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

func TestSegmentDeleteRunnerUsesProbedRecordingDuration(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	probed := false
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		duration: func(_ context.Context, path string) (float64, error) {
			if path != recording {
				t.Fatalf("probed %q, want %q", path, recording)
			}
			probed = true
			return 3, nil
		},
		splice: fakeSplice,
	}
	if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !probed {
		t.Fatal("runner did not probe recording duration")
	}
}

func TestCleanupSpliceTempCopies(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".delete-segment-before.mp3", ".splice-before.mp3", "recording.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := cleanupSpliceTempCopies(dir); err != nil {
		t.Fatalf("cleanup splice temp copies: %v", err)
	}
	for _, name := range []string{".delete-segment-before.mp3", ".splice-before.mp3"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temp copy %s remains: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "recording.mp3")); err != nil {
		t.Fatalf("recording disappeared: %v", err)
	}
}

func TestStartupRecoveryContinuesPastBadSplice(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	bad := filepath.Join(audioDir, "bad.mp3")
	good := filepath.Join(audioDir, "good.mp3")
	if err := os.WriteFile(bad, []byte("unexpected"), 0o600); err != nil {
		t.Fatalf("write bad recording: %v", err)
	}
	if err := os.WriteFile(good, []byte("original"), 0o600); err != nil {
		t.Fatalf("write good recording: %v", err)
	}
	now := time.Now().UTC()
	for _, entry := range []struct{ id, path string }{{"bad", bad}, {"good", good}} {
		if err := store.CreateSession(entry.id, now); err != nil {
			t.Fatalf("create %s: %v", entry.id, err)
		}
		if err := store.EndSession(entry.id, now.Add(time.Second), entry.path); err != nil {
			t.Fatalf("end %s: %v", entry.id, err)
		}
	}
	goodAfter := hashBytesForDeleteTest([]byte("spliced"))
	if _, err := store.DB().Exec(`
		INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at)
		VALUES (?, ?, 1, 2, ?, ?, ?), (?, ?, 1, 2, ?, ?, ?)`,
		"bad", bad, "not-the-current-hash", "ignored", now.Format(time.RFC3339Nano),
		"good", good, hashBytesForDeleteTest([]byte("original")), goodAfter, now.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert pending rows: %v", err)
	}
	outcomes, err := recoverPendingSplices(context.Background(), store, audioDir, nil, fakeSplice, nil)
	if err != nil {
		t.Fatalf("recover splices: %v", err)
	}
	recovered, failed := 0, 0
	for i := range outcomes {
		if outcomes[i].Recovered {
			recovered++
		}
		if outcomes[i].Err != nil {
			failed++
		}
	}
	if recovered != 1 || failed != 1 {
		t.Fatalf("outcomes=%v, want one recovered and one failed row", outcomes)
	}
	if contents, err := os.ReadFile(good); err != nil || !bytes.Equal(contents, []byte("spliced")) {
		t.Fatalf("good recovery bytes=%q err=%v", contents, err)
	}
	if contents, err := os.ReadFile(bad); err != nil || !bytes.Equal(contents, []byte("unexpected")) {
		t.Fatalf("bad recovery bytes=%q err=%v, want unchanged recording", contents, err)
	}
}

type mergedDeletionWordsFake struct{}

func (mergedDeletionWordsFake) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(contents, []byte("spliced")) {
		return cutWords(), nil
	}
	if strings.Contains(filepath.Base(path), "120000") {
		return []audio.Word{{Text: "unrelated", Start: 0, End: 3}}, nil
	}
	return initialWords(), nil
}

func TestDeleteSegmentWhenGCRemovedTheLocalAudio(t *testing.T) {
	for _, tc := range []struct {
		name          string
		audioPath     string
		wantDownloads []string
	}{
		{name: "one recording removed", audioPath: "first", wantDownloads: []string{"audio-20261111130000.mp3"}},
		{name: "all recordings removed", wantDownloads: []string{"audio-20261111120000.mp3", "audio-20261111130000.mp3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioDir := t.TempDir()
			first := filepath.Join(audioDir, "20261111120000.mp3")
			second := filepath.Join(audioDir, "20261111130000.mp3")
			if tc.audioPath != "" {
				if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
					t.Fatalf("write first recording: %v", err)
				}
			}
			store, segmentID, _ := newDeletionTestStore(t, second)
			path := ""
			if tc.audioPath != "" {
				path = first
			}
			if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = ?, gdrive_folder_id = ? WHERE id = ?`, path, time.Now().UTC().Format(time.RFC3339Nano), "folder", "session"); err != nil {
				t.Fatalf("mark local audio removed: %v", err)
			}
			drive := &deletionDriveFake{
				files: []*drive.File{
					{Name: "audio-20261111120000.mp3", MimeType: "audio/mpeg"},
					{Name: "audio-20261111130000.mp3", MimeType: "audio/mpeg"},
				},
				downloads: map[string][]byte{
					"audio-20261111120000.mp3": []byte("first"),
					"audio-20261111130000.mp3": []byte("second"),
				},
			}
			var refinedPath string
			runner := segmentDeleteRunner{
				store:       store,
				transcriber: mergedDeletionWordsFake{},
				ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
					return restoreDeletionRecordings(ctx, &session, audioDir, drive)
				},
				splice: fakeSplice,
				refine: func(_ context.Context, _ string, audioPath string, _ time.Time) error {
					refinedPath = audioPath
					return nil
				},
			}
			if err := runner.Delete(context.Background(), "session", segmentID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			want := first + "," + second
			session := storeSessionForDelete(t, store, "session")
			if session.AudioPath != want || session.AudioRemovedAt != "" {
				t.Fatalf("session audio_path=%q removed_at=%q, want %q and empty marker", session.AudioPath, session.AudioRemovedAt, want)
			}
			if refinedPath != want {
				t.Fatalf("refinement read %q, want complete audio path %q", refinedPath, want)
			}
			if contents, err := os.ReadFile(second); err != nil || !bytes.Equal(contents, []byte("spliced")) {
				t.Fatalf("spliced downloaded recording: bytes=%q err=%v", contents, err)
			}
			if !slices.Equal(drive.downloaded, tc.wantDownloads) {
				t.Fatalf("Drive downloads = %v, want %v", drive.downloaded, tc.wantDownloads)
			}
		})
	}
}

func TestDeleteSegmentRemovesDownloadedAudioOnRefusal(t *testing.T) {
	downloaded := filepath.Join(t.TempDir(), "downloaded.mp3")
	if err := os.WriteFile(downloaded, []byte("downloaded"), 0o600); err != nil {
		t.Fatalf("write downloaded recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, downloaded)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords(), after: append(cutWords(), audio.Word{Text: "private", Start: 0.9, End: 1})},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{downloaded}, Downloaded: []string{downloaded}}, nil
		},
		splice: fakeSplice,
	}
	if err := runner.Delete(context.Background(), "session", segmentID); !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("Delete error = %v, want clean-cut refusal", err)
	}
	if _, err := os.Stat(downloaded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("downloaded recording still exists after refusal: %v", err)
	}
}

func TestSegmentDeleteRunnerFindsUnknownSegmentBeforeNeedingTranscriber(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("recording"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, _, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{store: store}
	if err := runner.Delete(context.Background(), "session", 999999); !errors.Is(err, storage.ErrSegmentNotFound) {
		t.Fatalf("Delete error = %v, want segment not found", err)
	}
}

func TestSegmentDeleteRunnerRefusesSecondDeleteWhileSplicePending(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, firstID, _ := newDeletionTestStore(t, recording)
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	if err := store.DeleteSegment("session", firstID, storage.DeletedSpan{AudioPath: recording, Start: 1, End: 2, SHA256Before: "before", SHA256After: "after"}); err != nil {
		t.Fatalf("record pending splice: %v", err)
	}
	runner := segmentDeleteRunner{store: store}
	if err := runner.Delete(context.Background(), "session", segments[2].ID); !errors.Is(err, storage.ErrSegmentSplicePending) {
		t.Fatalf("second delete error = %v, want pending splice conflict", err)
	}
}

func storeSessionForDelete(t *testing.T, store *storage.SQLiteStore, id string) storage.Session {
	t.Helper()
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	return session
}

type sequenceDeletionWordsFake struct{}

func (sequenceDeletionWordsFake) Words(_ context.Context, path string) ([]audio.Word, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch string(contents) {
	case "original":
		return []audio.Word{
			{Text: "before", Start: 0.1, End: 0.2},
			{Text: "bridge", Start: 0.3, End: 0.4},
			{Text: "private", Start: 1.1, End: 1.2},
			{Text: "deletion", Start: 1.3, End: 1.4},
			{Text: "token", Start: 1.5, End: 1.6},
			{Text: "after", Start: 2.1, End: 2.2},
			{Text: "bridge", Start: 2.3, End: 2.4},
		}, nil
	case "spliced-one":
		return []audio.Word{
			{Text: "before", Start: 0.1, End: 0.2},
			{Text: "bridge", Start: 0.3, End: 0.4},
			{Text: "after", Start: 1.1, End: 1.2},
			{Text: "bridge", Start: 1.3, End: 1.4},
		}, nil
	case "spliced-two":
		return []audio.Word{{Text: "before", Start: 0.1, End: 0.2}, {Text: "bridge", Start: 0.3, End: 0.4}}, nil
	default:
		return nil, fmt.Errorf("unknown recording state %q", contents)
	}
}

func TestSegmentDeleteRunnerSerializesConcurrentDeletes(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, firstID, _ := newDeletionTestStore(t, recording)
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get segments: %v", err)
	}
	secondID := segments[2].ID
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var spliceCalls atomic.Int32
	controlledSplice := func(_ context.Context, path string, _, _ float64) (string, error) {
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if spliceCalls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		next := []byte("spliced-one")
		if bytes.Equal(contents, []byte("spliced-one")) {
			next = []byte("spliced-two")
		}
		if err := os.WriteFile(path, next, 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest(next), nil
	}
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: sequenceDeletionWordsFake{},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: controlledSplice,
	}
	errs := make(chan error, 2)
	go func() { errs <- runner.Delete(context.Background(), "session", firstID) }()
	<-firstEntered
	go func() { errs <- runner.Delete(context.Background(), "session", secondID) }()
	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent delete: %v", err)
		}
	}
	remaining, err := store.GetSegments("session")
	if err != nil {
		t.Fatalf("get remaining segments: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Text != "before bridge" {
		t.Fatalf("remaining segments = %+v, want only the first", remaining)
	}
	contents, err := os.ReadFile(recording)
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}
	if !bytes.Equal(contents, []byte("spliced-two")) {
		t.Fatalf("recording = %q, want both cuts", contents)
	}
}

func TestNeighbourCheckUsesWordsNearestTheCut(t *testing.T) {
	var beforeTokens []string
	var words []audio.Word
	for i := range 30 {
		token := fmt.Sprintf("word%d", i)
		beforeTokens = append(beforeTokens, token)
		words = append(words, audio.Word{Text: token, Start: float64(i), End: float64(i) + 0.5})
	}
	if missing := missingNeighbourTokens(words, 30, strings.Join(beforeTokens, " "), true); len(missing) != 0 {
		t.Fatalf("missing words = %v, want none from the six words nearest the cut", missing)
	}
}

func TestNeighbourCheckCountsRepeatedLostWords(t *testing.T) {
	words := []audio.Word{{Text: "the", Start: 1, End: 1.2}}
	if missing := missingNeighbourTokens(words, 0, "so the so", false); len(missing) != 2 {
		t.Fatalf("missing words = %v, want both lost occurrences of %q", missing, "so")
	}
}

func TestBoundaryCutSkipsOnlyCrossRecordingNeighbour(t *testing.T) {
	secondRecording := []audio.Word{{Text: "deleted", Start: 0.1, End: 0.2}, {Text: "after", Start: 1.2, End: 1.3}}
	if recordingHasWordsOnSide(secondRecording, 0, 1, true) {
		t.Fatal("selected recording unexpectedly has a before-side neighbour")
	}
	if !recordingHasWordsOnSide(secondRecording, 0, 1, false) {
		t.Fatal("selected recording should validate its after-side neighbour")
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
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
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

func TestSegmentDeleteRunnerRefusesKeptRawRecordingFromMergedSource(t *testing.T) {
	audioDir := t.TempDir()
	recording := filepath.Join(audioDir, "merged.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	if err := store.CreateSession("source-session", time.Now().UTC()); err != nil {
		t.Fatalf("create source session: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET status = 'merged', merged_into = ? WHERE id = ?`, "session", "source-session"); err != nil {
		t.Fatalf("mark source merged: %v", err)
	}
	if err := os.WriteFile(filepath.Join(audioDir, "source-session.pcm"), []byte("raw audio"), 0o600); err != nil {
		t.Fatalf("write source raw audio: %v", err)
	}
	runner := segmentDeleteRunner{
		store:       store,
		audioDir:    audioDir,
		transcriber: deletionWordsFake{before: initialWords(), after: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			t.Fatal("recordings checked after kept raw source audio")
			return deletionRecordingSet{}, nil
		},
		splice: fakeSplice,
	}
	err := runner.Delete(context.Background(), "session", segmentID)
	if !errors.Is(err, server.ErrSegmentRawAudioKept) || !strings.Contains(err.Error(), "source-session.pcm") {
		t.Fatalf("delete error = %v, want named source raw recording", err)
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
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
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
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			t.Fatal("recordings checked after kept raw audio")
			return deletionRecordingSet{}, nil
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
	recordingSet, err := restoreDeletionRecordings(context.Background(), &storage.Session{
		AudioPath:      first,
		AudioRemovedAt: time.Now().UTC().Format(time.RFC3339),
		GDriveFolderID: "folder",
	}, audioDir, drive)
	if err != nil {
		t.Fatalf("restore deletion recordings: %v", err)
	}
	if want := []string{first, second}; !slices.Equal(recordingSet.Paths, want) {
		t.Fatalf("recordings = %v, want %v", recordingSet.Paths, want)
	}
	if !slices.Equal(recordingSet.Downloaded, []string{second}) {
		t.Fatalf("downloaded paths = %v, want %v", recordingSet.Downloaded, []string{second})
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

func TestRestoreDeletionRecordingsHandlesTypedNilDriveSource(t *testing.T) {
	var source deletionRecordingSource = (*gdrive.Syncer)(nil)
	set, err := restoreDeletionRecordings(context.Background(), &storage.Session{GDriveFolderID: "folder"}, t.TempDir(), source)
	if err != nil {
		t.Fatalf("restore with sync off: %v", err)
	}
	if len(set.Paths) != 0 {
		t.Fatalf("restore paths = %v, want no Drive download while sync is off", set.Paths)
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
	outcomes, err := recoverPendingSplices(context.Background(), store, filepath.Dir(recording), nil, fakeSplice, func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
		if sessionID != "session" || audioPath != recording || gotStarted != started {
			t.Fatalf("refine recovered splice session=%q audio=%q started=%s", sessionID, audioPath, gotStarted)
		}
		refined = true
		return nil
	})
	if err != nil {
		t.Fatalf("recover pending splice: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Recovered || outcomes[0].Err != nil || !refined {
		t.Fatalf("outcomes=%v refined=%v; want one recovered span and true", outcomes, refined)
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
