package storage

import (
	"path/filepath"
	"testing"
)

// TestAppStateRoundTrip fails if a pause value cannot be stored, overwritten,
// or distinguished from an unset state.
func TestAppStateRoundTrip(t *testing.T) {
	store := newTestSQLiteStore(t)

	if value, ok, err := store.GetAppState(AppStateCapturePaused); err != nil || ok || value != "" {
		t.Fatalf("unset state = (%q, %t, %v), want (\"\", false, nil)", value, ok, err)
	}
	if err := store.SetAppState(AppStateCapturePaused, "1"); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	if value, ok, err := store.GetAppState(AppStateCapturePaused); err != nil || !ok || value != "1" {
		t.Fatalf("stored paused state = (%q, %t, %v), want (\"1\", true, nil)", value, ok, err)
	}
	if err := store.SetAppState(AppStateCapturePaused, "0"); err != nil {
		t.Fatalf("set resumed: %v", err)
	}
	if value, ok, err := store.GetAppState(AppStateCapturePaused); err != nil || !ok || value != "0" {
		t.Fatalf("stored resumed state = (%q, %t, %v), want (\"0\", true, nil)", value, ok, err)
	}
}

// TestAppStateSurvivesReopen fails if the pause switch is kept only in memory.
func TestAppStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SetAppState(AppStateCapturePaused, "1"); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if value, ok, err := store.GetAppState(AppStateCapturePaused); err != nil || !ok || value != "1" {
		t.Fatalf("reopened pause state = (%q, %t, %v), want (\"1\", true, nil)", value, ok, err)
	}
}
