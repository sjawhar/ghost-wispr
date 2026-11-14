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

func TestRetryRestoresAnUnrefinedRemovedSourceFromItsOwnFolder(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unrefined-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	sID, tID := "20261113090000", "20261113100000"
	sPath, tPath := filepath.Join(dir, sID+".mp3"), filepath.Join(dir, tID+".mp3")
	if err := os.WriteFile(tPath, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(sID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(sID, started.Add(time.Minute), sPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'failed', audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'folder-s', sync_status = ?, sync_state = ? WHERE id = ?`, storage.SyncSynced, storage.SyncStateSynced, sID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(tID, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(tID, started.Add(61*time.Minute), tPath); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{sID, tID}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-m', sync_status = ?, sync_state = ?, refinement_status = 'failed' WHERE id = 'merged'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	source := &r11FolderDrive{
		files: map[string][]*drive.File{
			"folder-m": {{Name: recordingname.FileName(tPath), MimeType: "audio/mpeg"}},
			"folder-s": {{Name: recordingname.FileName(sPath), MimeType: "audio/mpeg"}},
		},
		downloads: map[string][]byte{
			"folder-m/" + recordingname.FileName(tPath): []byte("t"),
			"folder-s/" + recordingname.FileName(sPath): []byte("s"),
		},
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	}); err != nil {
		t.Fatalf("restore unrefined source: %v", err)
	}
	if got := strings.Join(source.downloaded, ","); !strings.Contains(got, "folder-s/"+recordingname.FileName(sPath)) {
		t.Fatalf("restore downloads = %q, want source folder-s", got)
	}
}

func TestRetryRestoresPartlyRemovedMergedSourceFromItsFolder(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "partial-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	sID, tID, uID := "20261113090000", "20261113100000", "20261113110000"
	sPath, tPath, uPath := filepath.Join(dir, sID+".mp3"), filepath.Join(dir, tID+".mp3"), filepath.Join(dir, uID+".mp3")
	if err := os.WriteFile(tPath, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uPath, []byte("u"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		id   string
		path string
		at   time.Time
	}{{sID, sPath, started}, {tID, tPath, started.Add(time.Hour)}} {
		if err := store.CreateSession(pair.id, pair.at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(pair.id, pair.at.Add(time.Minute), pair.path); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("m1", []string{sID, tID}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'failed', audio_path = ?, audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'folder-m1', sync_status = ?, sync_state = ? WHERE id = 'm1'`, tPath, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(uID, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(uID, started.Add(121*time.Minute), uPath); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", uID}, started, started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-m2', sync_status = ?, sync_state = ?, refinement_status = 'failed' WHERE id = 'm2'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	source := &r11FolderDrive{
		files: map[string][]*drive.File{
			"folder-m1": {
				{Name: recordingname.FileName(sPath), MimeType: "audio/mpeg"},
				{Name: recordingname.FileName(tPath), MimeType: "audio/mpeg"},
			},
		},
		downloads: map[string][]byte{
			"folder-m1/" + recordingname.FileName(sPath): []byte("s"),
			"folder-m1/" + recordingname.FileName(tPath): []byte("t"),
		},
	}
	session, err := store.GetSession("m2")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	}); err != nil {
		t.Fatalf("restore partly removed merge: %v", err)
	}
	if got := strings.Join(source.downloaded, ","); !strings.Contains(got, "folder-m1/"+recordingname.FileName(sPath)) {
		t.Fatalf("restore downloads = %q, want removed source from folder-m1", got)
	}
}
