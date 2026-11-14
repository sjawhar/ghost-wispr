package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestOwedDriveReplacementsAreReadFromTheStore fails if the store cannot say,
// after a restart, which sessions still owe Drive a replacement, since when,
// and how many attempts failed in a row; if a failure of an earlier
// deletion generation counts; or if a finished replacement is still listed.
func TestOwedDriveReplacementsAreReadFromTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owed.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = segmentTestSession(t, store, "owed")
	segments, err := store.GetSegments("owed")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := deleteRefinedAudioTestSegment(t, store, "owed", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	generation, err := store.DeletionGeneration("owed")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.RecordDriveReplacementFailure("owed", generation, "Drive unavailable"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordDriveReplacementFailure("owed", generation-1, "a stale attempt"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owed, err := store.OwedDriveReplacements()
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].SessionID != "owed" {
		t.Fatalf("owed replacements after a restart = %+v, want session owed", owed)
	}
	if owed[0].Failures != 2 || owed[0].LastError != "Drive unavailable" || owed[0].Since.Before(before) || owed[0].Since.After(time.Now().UTC()) {
		t.Errorf("owed replacement = %+v, want 2 failures, the last error, and the time of the deletion", owed[0])
	}

	if err := store.ClearDriveReplacementPending("owed", generation); err != nil {
		t.Fatal(err)
	}
	if owed, err = store.OwedDriveReplacements(); err != nil || len(owed) != 0 {
		t.Errorf("owed replacements once nothing is owed = %+v, %v; want none", owed, err)
	}
}
