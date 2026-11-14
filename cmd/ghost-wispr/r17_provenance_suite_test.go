package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func suiteTwoWordRefine(store *storage.SQLiteStore) func(context.Context, string, string, time.Time) error {
	return func(_ context.Context, sessionID, audioPath string, _ time.Time) error {
		gen, claimed, err := store.ClaimRefinement(sessionID, audioPath)
		if err != nil || !claimed {
			return err
		}
		var words []storage.RefinedWord
		for i, path := range strings.Split(audioPath, ",") {
			base := strings.TrimSuffix(filepath.Base(path), ".mp3")
			words = append(words,
				storage.RefinedWord{RecordingPath: path, RecordingSHA256: base, RecordingOrdinal: i, Ordinal: 0, Text: "first-" + base + ".", Start: 0, End: 1},
				storage.RefinedWord{RecordingPath: path, RecordingSHA256: base, RecordingOrdinal: i, Ordinal: 1, Text: "second-" + base + ".", Start: 20, End: 21, Speaker: 1})
		}
		_, err = store.PublishRefinementAtGeneration(sessionID, storage.RefinedTranscript(words), "stub", words, storage.BuildRefinedSegments(words), gen)
		return err
	}
}

func suiteProvenanceDelete(t *testing.T, refinedFirst bool) string {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	sID, tID := "20261113090000", "20261113100000"
	sPath, tPath := filepath.Join(dir, sID+".mp3"), filepath.Join(dir, tID+".mp3")
	if err := os.WriteFile(tPath, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(sID, start); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(sID, start.Add(time.Minute), sPath); err != nil {
		t.Fatal(err)
	}
	if refinedFirst {
		if err := os.WriteFile(sPath, []byte("s"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := suiteTwoWordRefine(store)(context.Background(), sID, sPath, start); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(sPath); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = CASE WHEN ? THEN refinement_status ELSE 'failed' END, audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'folder-s', sync_status = ?, sync_state = ? WHERE id = ?`, refinedFirst, storage.SyncSynced, storage.SyncStateSynced, sID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(tID, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(tID, start.Add(61*time.Minute), tPath); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{sID, tID}, start, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	source := &r11FolderDrive{files: map[string][]*drive.File{"folder-s": {{Name: recordingname.FileName(sPath), MimeType: "audio/mpeg"}}}, downloads: map[string][]byte{"folder-s/" + recordingname.FileName(sPath): []byte("s")}}
	refine := suiteTwoWordRefine(store)
	retry := newRetryRefinementHook(store, func() string { return dir }, func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &session, dir, source, "")
	}, newWarningsRegistry(), refine)
	onSessionMerged(context.Background(), store, refine, func(context.Context, string, time.Time) error { return nil }, retry,
		func(_ context.Context, _ string, cause error) { t.Fatalf("hook failed: %v", cause) }, "merged")
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if strings.HasPrefix(segments[i].Text, "first-"+sID) {
			if err := store.DeleteRefinedSegmentText("merged", segments[i].ID, merged.RefinedRevision); err != nil {
				t.Fatal(err)
			}
		}
	}
	sourceSession, err := store.GetSession(sID)
	if err != nil {
		t.Fatal(err)
	}
	return sourceSession.CanonicalTranscript
}

func TestSuiteR17ProvenanceDeleteRefinedSourceControl(t *testing.T) {
	if got := suiteProvenanceDelete(t, true); !strings.Contains(got, "second-20261113090000") {
		t.Errorf("control: refined source lost its other words: %q", got)
	}
}

func TestSuiteR17ProvenanceDeleteUnrefinedSource(t *testing.T) {
	if got := suiteProvenanceDelete(t, false); !strings.Contains(got, "second-20261113090000") {
		t.Errorf("an unrefined restored source lost its other words on a delete: %q", got)
	}
}
