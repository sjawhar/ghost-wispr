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

// survivorRemovedSourceMerge merges S, whose recording garbage collection
// removed after it synced to folder-s, with T, whose recording is on disk.
// The merge has no Drive folder of its own.
func survivorRemovedSourceMerge(t *testing.T) (store *storage.SQLiteStore, dir, sPath string) {
	t.Helper()
	dir = t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "survivor.db"))
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
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'failed', audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'folder-s', sync_status = ?, sync_state = ? WHERE id = ?`, storage.SyncSynced, storage.SyncStateSynced, sID); err != nil {
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
	return store, dir, sPath
}

// The merge hook's failure recorder, as run wires it, leaves a merged session
// whose recordings could not be restored reading failed, so the page offers
// Retry refinement.
func TestMergeHookRecordsAnUnrestorableRefinementAsFailed(t *testing.T) {
	store, dir, _ := survivorRemovedSourceMerge(t)
	source := &r11FolderDrive{files: map[string][]*drive.File{"folder-s": {}}}
	refine := suiteTwoWordRefine(store)
	retry := newRetryRefinementHook(store, func() string { return dir }, func(ctx context.Context, s storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &s, dir, source, "")
	}, newWarningsRegistry(), refine)
	onSessionMerged(context.Background(), store, refine, func(context.Context, string, time.Time) error { return nil }, retry,
		newMergedRefinementFailureRecorder(store), "merged")
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if merged.RefinementStatus != storage.RefinementFailed {
		t.Fatalf("merged refinement = %q after its recordings could not be restored, want failed", merged.RefinementStatus)
	}
}

// A merge whose audio was removed before it had a folder of its own still
// opens Drive, to restore each source's recording from the source's folder.
func TestEnsureDeletionRecordingsOpensDriveForARemovedMergeWithoutAFolder(t *testing.T) {
	store, dir, sPath := survivorRemovedSourceMerge(t)
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if merged.GDriveFolderID != "" || merged.AudioRemovedAt == "" {
		t.Fatalf("control: merged = folder %q removed %q, want no folder and removed audio", merged.GDriveFolderID, merged.AudioRemovedAt)
	}
	opened := 0
	source := &r11FolderDrive{
		files:     map[string][]*drive.File{"folder-s": {{Name: recordingname.FileName(sPath), MimeType: "audio/mpeg"}}},
		downloads: map[string][]byte{"folder-s/" + recordingname.FileName(sPath): []byte("s")},
	}
	set, err := ensureDeletionRecordings(context.Background(), store, &merged, dir, func(context.Context) (deletionRecordingSource, error) {
		opened++
		return source, nil
	})
	if err != nil {
		t.Fatalf("ensure recordings of a removed merge without a folder: %v", err)
	}
	if opened != 1 || !strings.Contains(strings.Join(set.Paths, ","), sPath) {
		t.Fatalf("ensure recordings = %v with Drive opened %d times, want %s restored from Drive", set.Paths, opened, sPath)
	}
}

// A source cut before it was merged: the restore of its recording for the
// merge checks Drive's copy against the source's own latest cut hash, so a
// copy from before the cut, which still holds the deleted audio, is refused.
func TestRestoreChecksTheProvenanceSourcesCutHash(t *testing.T) {
	store, dir, sPath := survivorRemovedSourceMerge(t)
	if _, err := store.DB().Exec(`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, splice_complete, deleted_at)
		VALUES('20261113090000', ?, 1, 2, 'before-cut', 'after-cut', 1, '2026-11-13T10:00:00Z')`, sPath); err != nil {
		t.Fatal(err)
	}
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	source := &r11FolderDrive{
		files:     map[string][]*drive.File{"folder-s": {{Name: recordingname.FileName(sPath), MimeType: "audio/mpeg", Sha256Checksum: "before-cut"}}},
		downloads: map[string][]byte{"folder-s/" + recordingname.FileName(sPath): []byte("pre-cut audio")},
	}
	_, err = restoreDeletionRecordings(context.Background(), store, &merged, dir, source, "")
	if err == nil || !strings.Contains(err.Error(), "latest cut hash") {
		t.Fatalf("restore of a pre-cut copy = %v (downloads %v), want a refusal against the source's cut hash", err, source.downloaded)
	}
	if _, statErr := os.Stat(sPath); !os.IsNotExist(statErr) {
		t.Fatalf("the refused pre-cut copy was installed at %s: %v", sPath, statErr)
	}
}
