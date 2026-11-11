package storage

import "testing"

// TestDriveReplacementsRequiresAGeneration fails to compile if
// DriveReplacements accepts an omitted generation, which would read
// generation 0 rather than the deletion the caller is replacing.
func TestDriveReplacementsRequiresAGeneration(t *testing.T) {
	var read func(*SQLiteStore, string, int) ([]DriveReplacement, error) = (*SQLiteStore).DriveReplacements
	if read == nil {
		t.Fatal("DriveReplacements has no explicit-generation method value")
	}
}
