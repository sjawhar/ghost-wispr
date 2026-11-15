package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mergeRefusalStore(t *testing.T, ended ...string) (*SQLiteStore, time.Time) {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "merge-refusals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range ended {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	return store, started
}

func assertMergeRefusedUnchanged(t *testing.T, store *SQLiteStore, newID string, sources []string, err error, sentinel error, words ...string) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("merge = %v, want %v", err, sentinel)
	}
	for _, word := range words {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("merge refusal %q does not name %q", err, word)
		}
	}
	if newID != "" {
		if _, getErr := store.GetSession(newID); getErr == nil {
			t.Errorf("the refused merge created session %s", newID)
		}
	}
	for _, id := range sources {
		session, getErr := store.GetSession(id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if session.Status == SessionMerged && session.MergedInto == newID {
			t.Errorf("the refused merge merged source %s", id)
		}
	}
}

func TestMergeSessionsRefusesASourceAlreadyMerged(t *testing.T) {
	store, started := mergeRefusalStore(t, "a", "b", "c")
	if err := store.MergeSessions("m", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := store.MergeSessions("x", []string{"a", "c"}, started, started.Add(3*time.Hour))
	assertMergeRefusedUnchanged(t, store, "x", []string{"c"}, err, ErrMergeSourceUnavailable, "session a", "merged into m")
}

func TestMergeSessionsRefusesADiscardedSource(t *testing.T) {
	store, started := mergeRefusalStore(t, "a", "c")
	if err := store.DiscardSession("a"); err != nil {
		t.Fatal(err)
	}
	err := store.MergeSessions("x", []string{"a", "c"}, started, started.Add(2*time.Hour))
	assertMergeRefusedUnchanged(t, store, "x", []string{"c"}, err, ErrMergeSourceUnavailable, "session a", "discarded")
}

func TestMergeSessionsRefusesARecordingSourceByName(t *testing.T) {
	store, started := mergeRefusalStore(t, "c")
	if err := store.CreateSession("live", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := store.MergeSessions("x", []string{"c", "live"}, started, started.Add(2*time.Hour))
	assertMergeRefusedUnchanged(t, store, "x", []string{"c"}, err, ErrMergeSessionActive, "session live", "still recording")
}

func TestMergeSessionsRefusesANewIDThatAlreadyExists(t *testing.T) {
	store, started := mergeRefusalStore(t, "a", "b", "c")
	if err := store.MergeSessions("a-merged", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := store.MergeSessions("a-merged", []string{"a-merged", "c"}, started, started.Add(3*time.Hour))
	assertMergeRefusedUnchanged(t, store, "", []string{"c"}, err, ErrMergeSessionExists, "a-merged")
	merged, getErr := store.GetSession("a-merged")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if merged.Status != SessionEnded {
		t.Errorf("the refused re-merge changed session a-merged to %q", merged.Status)
	}
}

func TestEndSessionOfAMergedSessionNamesItsMerge(t *testing.T) {
	store, started := mergeRefusalStore(t, "a", "b")
	if err := store.MergeSessions("m", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := store.EndSession("a", started.Add(3*time.Hour), "/audio/a.mp3")
	if !errors.Is(err, ErrSessionMerged) {
		t.Fatalf("end merged session = %v, want %v", err, ErrSessionMerged)
	}
	for _, word := range []string{"session a", "merged into m"} {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("end refusal %q does not name %q", err, word)
		}
	}
}
