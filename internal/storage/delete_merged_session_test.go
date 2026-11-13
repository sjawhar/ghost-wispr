package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDeleteSessionDeletesMergedSourcesAndReturnsTheirRawPCM fails if deleting
// a merged session leaves a source row with transcript text or leaves the
// source raw recording out of the whole-file erase list.
func TestDeleteSessionDeletesMergedSourcesAndReturnsTheirRawPCM(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "source", started, started.Add(time.Minute), map[float64]string{1: "source text"})
	recordedSession(t, store, "other", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "other text"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	audioDir := t.TempDir()
	paths, err := store.DeleteSession("merged", audioDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "other", "merged"} {
		if _, err := store.GetSession(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("GetSession(%q) = %v, want no row", id, err)
		}
	}
	if !strings.Contains(paths, filepath.Join(audioDir, "source.pcm")) || !strings.Contains(paths, filepath.Join(audioDir, "other.pcm")) {
		t.Fatalf("erase paths = %q, want every source raw PCM", paths)
	}
}
