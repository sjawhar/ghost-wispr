package storage

import "testing"

// TestDriveReplacementsRequiresAGeneration fails to compile if
// DriveReplacements accepts an omitted generation, which would read
// generation 0 rather than the deletion the caller is replacing.
func TestDriveReplacementsRequiresAGeneration(t *testing.T) {
	store := newTestSQLiteStore(t)
	readGeneration := func(read func(*SQLiteStore, string, int) ([]DriveReplacement, error)) ([]DriveReplacement, error) {
		return read(store, "missing-session", 1)
	}
	rows, err := readGeneration((*SQLiteStore).DriveReplacements)
	if err != nil || len(rows) != 0 {
		t.Fatalf("DriveReplacements(missing-session, 1) = %d rows, %v; want none", len(rows), err)
	}
}
