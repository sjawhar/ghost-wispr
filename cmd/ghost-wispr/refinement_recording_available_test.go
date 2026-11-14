package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestRefinementRecordingAvailableUsesLocalAudioOrListedDriveCopy(t *testing.T) {
	local := filepath.Join(t.TempDir(), "local.mp3")
	if err := os.WriteFile(local, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, session := range map[string]storage.Session{
		"local recording": {
			AudioPath: local,
		},
		"listed Drive recording": {
			AudioRemovedAt: "2026-11-13T12:00:00Z",
			SyncStatus:     storage.SyncSynced,
			SyncState:      storage.SyncStateSynced,
			GDriveFolderID: "folder-1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := refinementRecordingAvailable(&session, t.TempDir()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefinementRecordingAvailableRejectsRemoteDeletedDriveFolder(t *testing.T) {
	session := storage.Session{
		AudioRemovedAt: "2026-11-13T12:00:00Z",
		SyncStatus:     storage.SyncSynced,
		SyncState:      storage.SyncStateRemoteDeleted,
		GDriveFolderID: "folder-1",
	}
	err := refinementRecordingAvailable(&session, t.TempDir())
	if err == nil {
		t.Fatal("remote-deleted Drive folder was accepted as a recording source")
	}
}

func TestRefinementRecordingAvailableRejectsNeverRecordedDriveSession(t *testing.T) {
	session := storage.Session{
		SyncStatus:     storage.SyncSynced,
		SyncState:      storage.SyncStateSynced,
		GDriveFolderID: "folder-1",
	}
	if err := refinementRecordingAvailable(&session, t.TempDir()); err == nil {
		t.Fatal("never-recorded session was accepted from its Drive folder")
	}
}
