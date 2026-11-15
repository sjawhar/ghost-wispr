package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type r12GatedDrive struct {
	mu       sync.Mutex
	files    map[string][]byte
	gateUsed bool
	entered  chan struct{}
	release  chan struct{}
}

func (d *r12GatedDrive) ListFolderFiles(context.Context, string) ([]*drive.File, error) {
	files := make([]*drive.File, 0, len(d.files))
	for name, data := range d.files {
		files = append(files, &drive.File{Name: name, MimeType: "audio/mpeg", Sha256Checksum: hashBytesForDeleteTest(data)})
	}
	return files, nil
}

func (d *r12GatedDrive) DownloadRecording(_ context.Context, _ string, name, destination string) error {
	d.mu.Lock()
	gate := !d.gateUsed
	d.gateUsed = true
	d.mu.Unlock()
	if gate {
		close(d.entered)
		<-d.release
	}
	return os.WriteFile(destination, d.files[name], 0o600)
}

func r12AllRemovedMerge(t *testing.T) (*storage.SQLiteStore, string, []string) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "restore-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ids := []string{"20261106100000", "20261106110000"}
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, id := range ids {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), filepath.Join(dir, id+".mp3")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", ids, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', refinement_status = 'completed', gdrive_folder_id = 'folder', sync_status = ?, sync_state = ? WHERE id = 'merged'`, storage.SyncSynced, storage.SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	return store, dir, ids
}

// TestR12ConcurrentRestoresKeepTheRecordings fails if an overlapping restore
// can finish its fenced write and then lose its recordings to the other
// restore's cleanup defer.
func TestR12ConcurrentRestoresKeepTheRecordings(t *testing.T) {
	store, dir, ids := r12AllRemovedMerge(t)
	source := &r12GatedDrive{files: map[string][]byte{}, entered: make(chan struct{}), release: make(chan struct{})}
	for _, id := range ids {
		source.files[recordingname.FileName(filepath.Join(dir, id+".mp3"))] = []byte("audio " + id)
	}
	restore := func() error {
		session, err := store.GetSession("merged")
		if err != nil {
			return err
		}
		return restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
			return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
		})
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- restore() }()
	<-source.entered
	go func() { second <- restore() }()
	select {
	case err := <-second:
		t.Fatalf("second restore finished before the first released its download: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(source.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(dir, id+".mp3")); err != nil {
			t.Fatalf("restored recording %s is missing: %v", id, err)
		}
	}
}

// TestR12SamePathRestoreKeepsTheRecording fails if the fast path that leaves
// audio_path unchanged returns before it claims its downloaded recording.
func TestR12SamePathRestoreKeepsTheRecording(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "same-path.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id := "20261113090000"
	path := filepath.Join(dir, id+".mp3")
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'no_audio', gdrive_folder_id = 'folder', sync_status = ?, sync_state = ? WHERE id = ?`, storage.SyncSynced, storage.SyncStateSynced, id); err != nil {
		t.Fatal(err)
	}
	source := &deletionDriveFake{
		files:     []*drive.File{{Name: recordingname.FileName(path), MimeType: "audio/mpeg"}},
		downloads: map[string][]byte{recordingname.FileName(path): []byte("audio")},
	}
	session, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, dir, source, "")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("same-path restore removed its recording: %v", err)
	}
}
