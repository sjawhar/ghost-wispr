package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func r9MergedRestoreSession(t *testing.T, audioPath string) (*storage.SQLiteStore, storage.Session, []string) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	names := []string{"20261113090000.mp3", "20261113100000.mp3", "20261113110000.mp3"}
	ids := make([]string, len(names))
	for i, name := range names {
		id := strings.TrimSuffix(name, ".mp3")
		ids[i] = id
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), filepath.Join("/legacy", name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", ids, started, started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	const removedAt = "2026-11-13T12:00:00Z"
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = ?, audio_removed_at = ? WHERE id = ?`, audioPath, removedAt, "merged"); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	return store, session, names
}

// TestR9SubsetRestoreDoesNotClearTheMarker fails if a Drive restore rebuilds a
// merged session from the recordings Drive still has while another source
// recording remains missing. The audio-removed marker is the guard that keeps
// the persisted refined words from being rebuilt without that source.
func TestR9SubsetRestoreDoesNotClearTheMarker(t *testing.T) {
	dir := t.TempDir()
	store, session, names := r9MergedRestoreSession(t, "")
	restored := []string{filepath.Join(dir, names[0]), filepath.Join(dir, names[2])}

	err := restoreRefinementRecording(context.Background(), store, &session, dir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: restored, Downloaded: restored}, nil
	})
	if err == nil {
		t.Fatal("restored a subset of merged recordings")
	}
	if !strings.Contains(err.Error(), names[1]) || !strings.Contains(err.Error(), "Drive's trash") {
		t.Fatalf("restore refusal = %q, want missing recording and next step", err)
	}
	current, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if current.AudioRemovedAt == "" {
		t.Fatal("subset restoration cleared audio_removed_at")
	}
}

// TestR9MovedAudioDirDoesNotClearTheMarker fails if resolving one surviving
// recording through a moved audio directory makes its new path look like a
// full restore and clears the marker for the other missing source recordings.
func TestR9MovedAudioDirDoesNotClearTheMarker(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	store, session, names := r9MergedRestoreSession(t, filepath.Join(oldDir, "20261113110000.mp3"))
	survivor := filepath.Join(newDir, names[2])
	if err := os.WriteFile(survivor, []byte("surviving recording"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := restoreRefinementRecording(context.Background(), store, &session, newDir, func(ctx context.Context, current storage.Session) (deletionRecordingSet, error) {
		return restoreDeletionRecordings(ctx, store, &current, newDir, nil, "")
	})
	if err == nil {
		t.Fatal("restored only the moved survivor")
	}
	if !strings.Contains(err.Error(), names[0]) {
		t.Fatalf("restore refusal = %q, want missing %s", err, names[0])
	}
	current, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if current.AudioRemovedAt == "" {
		t.Fatal("moved audio directory cleared audio_removed_at")
	}
}
