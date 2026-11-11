package gdrive

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// remountingUploader brings a recording back on disk while it uploads the
// session's other files: a disk remounted during a sync.
type remountingUploader struct {
	recording string
	uploaded  []string
}

func (u *remountingUploader) Upload(_ context.Context, _ string, files []SyncFile) (string, error) {
	if err := os.WriteFile(u.recording, make([]byte, 2*1024*1024), 0o644); err != nil {
		return "", err
	}
	for _, f := range files {
		u.uploaded = append(u.uploaded, f.Name)
	}
	return "folder-1", nil
}

func (u *remountingUploader) UploadInto(ctx context.Context, _ string, files []SyncFile) error {
	_, err := u.Upload(ctx, "", files)
	return err
}

// FolderOnDrive reports the one folder the uploader makes as on Drive.
func (u *remountingUploader) FolderOnDrive(_ context.Context, folderID string) (bool, error) {
	return folderID == "folder-1", nil
}

func (*remountingUploader) RecordingsMatch(context.Context, string, map[string]string) (bool, error) {
	return true, nil
}

// Review of #37, round 2, blocker 1: a recording missing when the sync built
// its file list, and back on disk by the end of the upload, was never
// uploaded, yet the session was marked synced, so GC gated on sync removed a
// recording Drive never had. Whether a recording was uploaded is decided from
// the files the sync uploaded, not from a second look at the disk.
func TestOrchestratorDoesNotMarkSyncedARecordingThatCameBackDuringTheUpload(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	const id = "20261106130000"
	recording := filepath.Join(dir, id+".mp3") // its disk is not mounted yet
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	uploader := &remountingUploader{recording: recording}
	o := NewOrchestratorWithUploader(uploader, store)

	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Errorf("sync succeeded; Drive got only %v", uploader.uploaded)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.SyncStatus == storage.SyncSynced {
		t.Errorf("marked synced, but Drive got only %v: %s was never uploaded", uploader.uploaded, recording)
	}

	gcConfig := gc.Config{MaxAudioSizeMB: 1, SyncGated: true, AudioDir: dir, Drive: o}
	if _, err := gc.New(store, gcConfig, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if _, err := os.Stat(recording); err != nil {
		t.Errorf("GC gated on sync removed %s, a recording Drive never had: %v", recording, err)
	}
}

// Review of #37, round 3, blocker 1, the reviewer's four steps. Drive files
// were named by their place in the audio path (audio-1, audio-2), and a
// re-sync skips a file whose name the folder has; when the row shrank between
// a partial sync and its retry, a different recording took the uploaded one's
// name, the session was marked synced without it, and GC gated on sync then
// removed it. Each audio file is now named after its own recording.
func TestGCNeverRemovesARecordingDriveSkippedOnARetry(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root := t.TempDir()
	audioDir, mnt := filepath.Join(root, "audio"), filepath.Join(root, "mnt")
	for _, dir := range []string{audioDir, mnt} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	recordings := map[string][]byte{}
	var paths []string
	for i, path := range []string{
		filepath.Join(audioDir, "20261106100000.mp3"),
		filepath.Join(mnt, "20261106101000.mp3"),
		filepath.Join(mnt, "20261106102000.mp3"),
	} {
		content := bytes.Repeat([]byte{byte('A' + i)}, 2*1024*1024)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		recordings[path] = content
		paths = append(paths, path)
	}
	a, b := paths[0], paths[1]
	const id = "20261106100000-merged"
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), strings.Join(paths, ",")); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1. Sync on: the first sync stops after the summary, the transcript and
	// one recording.
	drive.failAfter = 3
	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("the cut-off first sync succeeded")
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	folder := sess.GDriveFolderID
	if sess.SyncState != storage.SyncStateRetryScheduled || folder == "" {
		t.Fatalf("after the cut-off sync: %q in folder %q, want %q in its folder", sess.SyncState, folder, storage.SyncStateRetryScheduled)
	}

	// 2. Sync off, so GC is not gated; mnt is away for a pass over budget.
	away := filepath.Join(root, "mnt-away")
	if err := os.Rename(mnt, away); err != nil {
		t.Fatal(err)
	}
	if _, err := gc.New(store, gc.Config{MaxAudioSizeMB: 1, AudioDir: audioDir}, logger).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if err := os.Rename(away, mnt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("GC kept %s (%v); the test needs it removed", a, err)
	}

	// 3. mnt back, sync on, the retry.
	drive.failAfter = 0
	retryErr := o.SyncSession(context.Background(), id)
	if sess, err = store.GetSession(id); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.SyncState == storage.SyncStateSynced && !drive.has(folder, recordings[b]) {
		t.Errorf("the retry (%v) marked the session synced, but Drive has %v and not %s", retryErr, keys(drive.files(folder)), b)
	}

	// 4. A pass of GC gated on sync.
	if _, err := gc.New(store, gc.Config{MaxAudioSizeMB: 1, SyncGated: true, AudioDir: audioDir, Drive: o}, logger).Run(); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil && !drive.has(folder, recordings[path]) {
			t.Errorf("LOST: %s is gone from disk and was never on Drive (Drive has %v)", path, keys(drive.files(folder)))
		}
	}
}

// Review of #37, round 3, note 1: a retry into the folder a cut-off first
// sync left skipped summary.md and transcript.md by name, so a summary
// re-made in between never reached Drive, and a restore would bring back the
// old one. The retry replaces a document whose content changed.
func TestOrchestratorRetryReplacesAChangedSummary(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recording := filepath.Join(t.TempDir(), "20261106100000.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	const id = "20261106100000"
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "", "The first summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)

	drive.failAfter = 2 // the summary and the transcript, not the recording
	if err := o.SyncSession(context.Background(), id); err == nil {
		t.Fatal("the cut-off first sync succeeded")
	}
	if err := store.UpdateSummary(id, "", "The summary, made again.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize again: %v", err)
	}
	drive.failAfter = 0
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	summaries := 0
	for _, f := range drive.folders[sess.GDriveFolderID] {
		if f.Name != "summary.md" {
			continue
		}
		summaries++
		if !strings.Contains(string(f.content), "The summary, made again.") {
			t.Errorf("Drive's summary.md after the retry:\n%s\nwant the summary made again", f.content)
		}
	}
	if summaries != 1 {
		t.Errorf("the folder holds %d summary.md, want 1", summaries)
	}
}

func keys(m map[string][]byte) []string {
	var names []string
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// pausingStore is the real store with GC paused just before it puts back in
// a session's row a recording it failed to remove: GC has marked the row (the
// recording no longer named) and removed what it could.
type pausingStore struct {
	*storage.SQLiteStore
	reached, release chan struct{}
}

func (s pausingStore) RestoreAudio(removal *storage.AudioRemoval, restored string) error {
	close(s.reached)
	<-s.release
	return s.SQLiteStore.RestoreAudio(removal, restored)
}

// gcRestoreRace sets up a session whose two recordings disk pressure takes,
// one of which (on a directory that refuses removals) it then has to put
// back, and runs that GC pass up to the put-back. It returns the store, the
// session id, the recording GC puts back with its content, a function that
// lets GC finish and waits for it, and the GC logger.
func gcRestoreRace(t *testing.T) (*storage.SQLiteStore, string, string, []byte, func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory")
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir, stuckDir := t.TempDir(), t.TempDir()
	removable := filepath.Join(audioDir, "20261106100000.mp3")
	stuck := filepath.Join(stuckDir, "20261106101000.mp3")
	content := bytes.Repeat([]byte{'B'}, 2*1024*1024)
	for path, data := range map[string][]byte{removable: bytes.Repeat([]byte{'A'}, 2*1024*1024), stuck: content} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(stuckDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuckDir, 0o755) })
	const id = "20261106100000-merged"
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), removable+","+stuck); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("summarize: %v", err)
	}

	paused := pausingStore{store, make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Sync is off, so GC is not gated.
		if _, err := gc.New(paused, gc.Config{MaxAudioSizeMB: 1, AudioDir: audioDir}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(); err != nil {
			t.Errorf("gc run: %v", err)
		}
	}()
	select {
	case <-paused.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("GC never reached the put-back")
	}
	var once sync.Once
	finishGC := func() {
		once.Do(func() {
			close(paused.release)
			<-done
		})
	}
	t.Cleanup(finishGC)
	return store, id, stuck, content, finishGC
}

// expectSyncedWith fails the test unless session id is synced and its Drive
// folder holds content.
func expectSyncedWith(t *testing.T, store *storage.SQLiteStore, drive *fakeDrive, id, recording string, content []byte) {
	t.Helper()
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.SyncState != storage.SyncStateSynced || !drive.has(sess.GDriveFolderID, content) {
		t.Errorf("after the retry: %q, Drive has %v; want synced with %s", sess.SyncState, keys(drive.files(sess.GDriveFolderID)), recording)
	}
}

// A sync that read the row while GC had marked it (a recording no longer
// named), and is still uploading when GC puts that recording back because it
// could not remove it, is not marked synced: it uploaded the row as it was,
// not as it is. The retry uploads the recording put back.
func TestOrchestratorDoesNotMarkSyncedARowGCPutAudioBackInDuringTheUpload(t *testing.T) {
	store, id, stuck, content, finishGC := gcRestoreRace(t)
	drive := newFakeDrive()
	drive.onUpload = finishGC // GC puts the recording back during the upload
	o := NewOrchestratorWithUploader(drive, store)

	syncErr := o.SyncSession(context.Background(), id)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != stuck {
		t.Fatalf("GC left audio_path %q, want %q put back", sess.AudioPath, stuck)
	}
	if sess.SyncState == storage.SyncStateSynced {
		t.Errorf("marked synced (%v), but Drive has %v and not %s, which GC put back during the upload", syncErr, keys(drive.files(sess.GDriveFolderID)), stuck)
	}

	drive.onUpload = nil
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	expectSyncedWith(t, store, drive, id, stuck, content)
}

// A sync that ran from start to end while GC had the row marked, so was
// marked synced with the row as it then was, is reopened when GC puts back a
// recording it could not remove: the session names a recording Drive does
// not have. The retry uploads it.
func TestGCReopensASyncThatRanWhileItHadTheRowMarked(t *testing.T) {
	store, id, stuck, content, finishGC := gcRestoreRace(t)
	drive := newFakeDrive()
	o := NewOrchestratorWithUploader(drive, store)
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("sync: %v", err)
	}
	finishGC()

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.AudioPath != stuck {
		t.Fatalf("GC left audio_path %q, want %q put back", sess.AudioPath, stuck)
	}
	if sess.SyncState == storage.SyncStateSynced {
		t.Errorf("still synced after GC put back %s, which Drive does not have (%v)", stuck, keys(drive.files(sess.GDriveFolderID)))
	}

	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	expectSyncedWith(t, store, drive, id, stuck, content)
}
