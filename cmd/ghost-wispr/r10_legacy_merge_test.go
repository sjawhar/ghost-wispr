package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestR10OldMergeSubsetRestoreKeepsMarker fails if a merge made before
// recording_sources existed accepts a subset after garbage collection removed
// every recording.
func TestR10OldMergeSubsetRestoreKeepsMarker(t *testing.T) {
	dir := t.TempDir()
	store, session, names := r9MergedRestoreSession(t, "")
	if _, err := store.DB().Exec(`DELETE FROM recording_sources`); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	restored := []string{filepath.Join(dir, strings.TrimSuffix(names[0], ".mp3")+".wav"), filepath.Join(dir, names[2])}
	err = restoreRefinementRecording(context.Background(), store, &session, dir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: restored, Downloaded: restored}, nil
	})
	if err == nil {
		t.Fatal("old merged session accepted a subset restore")
	}
	if !strings.Contains(err.Error(), names[1]) || strings.Contains(err.Error(), names[0]) {
		t.Fatalf("old merge restore refusal = %q, want only missing %s", err, names[1])
	}
	after, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if after.AudioRemovedAt == "" {
		t.Fatal("old merged session cleared audio_removed_at after a subset restore")
	}
}
