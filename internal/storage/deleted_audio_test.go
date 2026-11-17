package storage

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Re-review of #76, P1: a person's delete and GC's age delete record, in the
// transaction that deletes the rows, each session they delete and the audio
// path its row named (DeletedSessionAudio), the merged sources included: the
// kept recordings pass erases the audio of those sessions alone, so a
// restored backup, which has no row for the sessions recorded since it, does
// not make them look deleted. A session start never takes a recorded id, and
// a cleared record frees it.
func TestDeletesRecordTheSessionsWhoseAudioThePassErases(t *testing.T) {
	store := newTestSQLiteStore(t)
	audioDir := t.TempDir()
	at := func(id string) string { return filepath.Join(audioDir, id+".mp3") }
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	const deleted, source, gcDeleted, kept = "20261106140000", "20261106141000", "20261106150000", "20261106160000"
	endedWith(t, store, deleted, at(deleted), RefinementCompleted)
	endedWith(t, store, source, at(source), RefinementCompleted)
	if _, err := store.DB().Exec(`UPDATE sessions SET status = ?, merged_into = ? WHERE id = ?`, SessionMerged, deleted, source); err != nil {
		t.Fatal(err)
	}
	endedAndSynced(t, store, gcDeleted, at(gcDeleted), RefinementCompleted)
	endedWith(t, store, kept, at(kept), RefinementCompleted)

	if _, err := store.DeleteSession(deleted, audioDir); err != nil {
		t.Fatalf("delete %s: %v", deleted, err)
	}
	if _, removed, err := store.DeleteEndedSessionAndSources(gcDeleted, at(gcDeleted), audioDir); err != nil || !removed {
		t.Fatalf("age delete %s = %v, %v; want deleted", gcDeleted, removed, err)
	}

	records, err := store.DeletedSessionAudio()
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(records, func(a, b DeletedSessionAudio) int { return strings.Compare(a.SessionID, b.SessionID) })
	want := []DeletedSessionAudio{
		{SessionID: deleted, AudioPath: at(deleted)},
		{SessionID: source, AudioPath: at(source)},
		{SessionID: gcDeleted, AudioPath: at(gcDeleted)},
	}
	if !slices.Equal(records, want) {
		t.Errorf("DeletedSessionAudio = %+v, want %+v", records, want)
	}

	if err := store.CreateSession(deleted, started); !errors.Is(err, ErrSessionExists) {
		t.Errorf("a session start at a recorded deleted id = %v, want ErrSessionExists", err)
	}
	if err := store.ClearDeletedSessionAudio(deleted); err != nil {
		t.Fatal(err)
	}
	if records, err := store.DeletedSessionAudio(); err != nil || slices.ContainsFunc(records, func(r DeletedSessionAudio) bool { return r.SessionID == deleted }) {
		t.Errorf("after the clear DeletedSessionAudio = %+v (%v), want no %s", records, err, deleted)
	}
	if err := store.CreateSession(deleted, started); err != nil {
		t.Errorf("a session start at a cleared id = %v, want it created", err)
	}
}
