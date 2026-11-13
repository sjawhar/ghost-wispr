package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestR10OldMergeWithoutProvenanceRefusesRestore confirms that a database
// without recording_sources refuses rather than guessing its recordings.
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
		t.Fatal("old merged session accepted a restore without provenance")
	}
	if !strings.Contains(err.Error(), "recording provenance is unavailable") {
		t.Fatalf("old merge restore refusal = %q, want provenance error", err)
	}
	after, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if after.AudioRemovedAt == "" {
		t.Fatal("old merged session cleared audio_removed_at after a subset restore")
	}
}

// TestR10RecordingSourcesPreserveCustomNames fails if a merge's recorded
// provenance is discarded. A source recording need not use its session
// identifier as its basename.
func TestR10RecordingSourcesPrecedeLegacyNames(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "source-names.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("source-session", started); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(t.TempDir(), "meeting-recording.mp3")
	if err := store.EndSession("source-session", started.Add(time.Minute), custom); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source-session"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = ? WHERE id = 'merged'`, "2026-11-13T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	err = restoreRefinementRecording(context.Background(), store, &session, filepath.Dir(custom), func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: []string{custom}, Downloaded: []string{custom}}, nil
	})
	if err != nil {
		t.Fatalf("restore through stored source recording = %v", err)
	}
}
