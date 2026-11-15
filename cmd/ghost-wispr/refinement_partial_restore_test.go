package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type partialRestoreStore struct{ called bool }

func (s *partialRestoreStore) RestoreAudioForRefinement(_, _, _ string) (bool, error) {
	s.called = true
	return true, nil
}

func (*partialRestoreStore) DriveRestoreAllowed(string) (bool, error) { return true, nil }

func (*partialRestoreStore) RefinementRecordingPaths(string) ([]string, error) { return nil, nil }

func TestPartlyRemovedSessionRestoresBeforeRefining(t *testing.T) {
	dir := t.TempDir()
	remaining := filepath.Join(dir, "20261106110000.mp3")
	if err := os.WriteFile(remaining, []byte("remaining"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := storage.Session{
		ID:             "merged",
		AudioPath:      remaining,
		AudioRemovedAt: "2026-11-13T12:00:00Z",
		GDriveFolderID: "folder",
		SyncStatus:     storage.SyncSynced,
		SyncState:      storage.SyncStateSynced,
	}
	restoreCalled := false
	store := &partialRestoreStore{}
	err := restoreRefinementRecording(context.Background(), store, &session, dir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		restoreCalled = true
		removed := filepath.Join(dir, "20261106100000.mp3")
		return deletionRecordingSet{Paths: []string{removed, remaining}, Downloaded: []string{removed}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restoreCalled || !store.called {
		t.Fatal("partly removed session was refined without restoring its removed recording")
	}
}
