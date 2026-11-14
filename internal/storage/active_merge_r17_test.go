package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMergeSessionsRefusesActiveSourceAndEndDoesNotResurrectMergedRow(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "active-merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("active", started); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("ended", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("ended", started.Add(61*time.Minute), "/audio/ended.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("refused", []string{"active", "ended"}, started, started.Add(2*time.Hour)); err == nil {
		t.Fatal("merge accepted an active source")
	}
	if err := store.EndSession("active", started.Add(time.Minute), "/audio/active.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"active", "ended"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("active", started.Add(2*time.Minute), "/audio/active.mp3"); !errors.Is(err, ErrSessionMerged) {
		t.Fatalf("end merged source = %v, want merged session refusal", err)
	}
	active, err := store.GetSession("active")
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != SessionMerged || active.AudioPath != "" {
		t.Fatalf("ended merged source = %+v, want merged with no audio path", active)
	}
}
