package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type resolvedPathsRestoreStore struct {
	called           bool
	before, restored string
}

func (s *resolvedPathsRestoreStore) RestoreAudioForRefinement(_, before, restored string) (bool, error) {
	s.called, s.before, s.restored = true, before, restored
	return true, nil
}

func (*resolvedPathsRestoreStore) DriveRestoreAllowed(string) (bool, error) { return true, nil }

func (*resolvedPathsRestoreStore) RefinementRecordingPaths(string) ([]string, error) { return nil, nil }

func TestRestoreRefinementNamesRecordingsAlreadyOnDisk(t *testing.T) {
	dir := t.TempDir()
	removed := filepath.Join(dir, "20261106100000.mp3")
	remaining := filepath.Join(dir, "20261106110000.mp3")
	for _, path := range []string{removed, remaining} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	drive := &deletionDriveFake{files: []*drive.File{
		{Name: recordingname.FileName(removed), MimeType: "audio/mpeg"},
		{Name: recordingname.FileName(remaining), MimeType: "audio/mpeg"},
	}}
	session := storage.Session{ID: "merged", AudioPath: remaining, AudioRemovedAt: "2026-11-13T12:00:00Z", GDriveFolderID: "folder", SyncStatus: storage.SyncSynced, SyncState: storage.SyncStateSynced}
	store := &resolvedPathsRestoreStore{}
	err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, &current, dir, drive)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := removed + "," + remaining
	if !store.called || store.before != remaining || store.restored != want {
		t.Fatalf("restoration write = called:%t before:%q restored:%q, want %q", store.called, store.before, store.restored, want)
	}
}

func TestRestoreRefinementRefusesWhenDriveLacksARemovedRecording(t *testing.T) {
	dir := t.TempDir()
	remaining := filepath.Join(dir, "20261106110000.mp3")
	if err := os.WriteFile(remaining, []byte("remaining"), 0o600); err != nil {
		t.Fatal(err)
	}
	drive := &deletionDriveFake{files: []*drive.File{{Name: recordingname.FileName(remaining), MimeType: "audio/mpeg"}}}
	session := storage.Session{ID: "merged", AudioPath: remaining, AudioRemovedAt: "2026-11-13T12:00:00Z", GDriveFolderID: "folder", SyncStatus: storage.SyncSynced, SyncState: storage.SyncStateSynced}
	store := &resolvedPathsRestoreStore{}
	err := restoreRefinementRecording(context.Background(), store, &session, dir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, &current, dir, drive)
	})
	if err == nil {
		t.Fatal("restore succeeded after Drive omitted a removed recording")
	}
	if store.called {
		t.Fatal("restore cleared the removal marker without recovering every recording")
	}
}
