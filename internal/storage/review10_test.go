package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// owedCount reads the store-wide owed-erase count, "" when none is owed.
func owedCount(t *testing.T, store *SQLiteStore) string {
	t.Helper()
	var value string
	err := store.DB().QueryRow(`SELECT value FROM app_state WHERE key = ?`, owedEraseKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// waitOwed waits until an erase is owed, and returns the count it reads.
func waitOwed(t *testing.T, store *SQLiteStore) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if value := owedCount(t, store); value != "" {
			return value
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no erase was ever owed")
	return ""
}

// TestReview10ACompareAndClearKeepsALaterDeletionOwed fails if an erase that
// read the owed count clears a count a later deletion recorded from scratch
// (the same value, "1", after the row was deleted and recreated): that later
// deletion's own erase may then fail with nothing left owed.
//
// Order, forced with the store's eraseMu and the vacuumInto hook:
//  1. Deletion A (session "other") commits; the count is "1"; A waits for eraseMu.
//  2. A retry (the sweep, run here under the held eraseMu as retryOwedErasure
//     runs it) erases everything and clears "1"; the row is gone.
//  3. A gets eraseMu and refreshes the backup; just after that copy, deletion D
//     (the secret segment of "backed-up") commits and records "1" anew.
//  4. A clears "1": D's count.
//  5. D's own erase fails (a full disk). The control: the count must still be
//     owed, and a retry must then erase the backup.
func TestReview10ACompareAndClearKeepsALaterDeletionOwed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	openWithSecret(t, path)
	store := openStoreWithBackup(t, path)
	segmentTestSession(t, store, "other")
	other, err := store.GetSegments("other")
	if err != nil || len(other) < 2 {
		t.Fatalf("segments of other: %d, %v", len(other), err)
	}
	secretSegments, err := store.GetSegments("backed-up")
	if err != nil {
		t.Fatal(err)
	}
	span := DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}

	original := vacuumInto
	t.Cleanup(func() { vacuumInto = original })
	dCommitted := make(chan error, 1)
	calls := 0
	vacuumInto = func(db *sql.DB, p string) error {
		calls++
		switch calls {
		case 1: // the retry's refresh in step 2
			return original(db, p)
		case 2: // A's refresh in step 3: copy, then let D commit
			if err := original(db, p); err != nil {
				return err
			}
			go func() { dCommitted <- store.DeleteSegment("backed-up", secretSegments[1].ID, span) }()
			waitOwed(t, store)
			return nil
		case 3: // D's own refresh in step 5: a full disk
			return fmt.Errorf("copy the database: %w", syscall.ENOSPC)
		default: // a later retry
			return original(db, p)
		}
	}

	store.eraseMu.Lock()
	aDone := make(chan error, 1)
	go func() { aDone <- store.DeleteSegment("other", other[1].ID, span) }()
	select {
	case err := <-aDone:
		store.eraseMu.Unlock()
		t.Fatalf("deletion A returned before its erase: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	read := waitOwed(t, store)
	// Step 2: what retryOwedErasure does once it holds eraseMu.
	if err := store.eraseEveryOwed(false); err != nil {
		t.Fatalf("retry erase: %v", err)
	}
	if err := clearOwedErase(store.db, read); err != nil {
		t.Fatal(err)
	}
	if got := owedCount(t, store); got != "" {
		t.Fatalf("owed count after the retry = %q, want none", got)
	}
	store.eraseMu.Unlock()

	if err := <-aDone; err != nil {
		t.Fatalf("deletion A: %v", err)
	}
	dErr := <-dCommitted
	if !errors.Is(dErr, ErrSegmentPostCommitMaintenance) {
		t.Fatalf("deletion D with a full disk = %v, want %v", dErr, ErrSegmentPostCommitMaintenance)
	}
	assertBackupHolds(t, path, backupSecret, true)
	t.Logf("owed count after D's failed erase: %q", owedCount(t, store))
	if err := store.RetryOwedErasure(); err != nil {
		t.Fatalf("RetryOwedErasure: %v", err)
	}
	assertBackupHolds(t, path, backupSecret, false)
}
