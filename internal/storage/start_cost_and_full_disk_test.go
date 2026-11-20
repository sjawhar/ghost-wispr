package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// eraseOwed reports whether an erase of the write-ahead log and the backup
// is owed (owedEraseKey), whatever deletion number the marker holds.
func eraseOwed(t *testing.T, store *SQLiteStore) bool {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM app_state WHERE key = ?`, owedEraseKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestAStartWithNoSessionDeletedDoesNoExtraWork: the clean-up of what a
// deleted session left runs at every start, so on a start with no session
// deleted since the last one it must cost nothing: no second rebuild of the
// search index, and no erase of the write-ahead log marked owed, although the
// sessions' summaries changed the index since it was last rebuilt. A delete
// made the way an older build makes it (the rows only) is still cleared.
func TestAStartWithNoSessionDeletedDoesNoExtraWork(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	indexedSummarySession(t, store, "first", started, "The team met about the yard.")
	indexedSummarySession(t, store, "second", started.Add(time.Hour), "The team met about the fence.")
	if err := store.UpdateSummaryAtGeneration("first", "Title", "The team met about the yard and the fence.", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}

	if err := store.dropWhatDeletedSessionsLeft(); err != nil {
		t.Fatal(err)
	}
	if eraseOwed(t, store) {
		t.Error("a start with no session deleted marked an erase of the write-ahead log owed")
	}

	// Control: the rows of a session deleted the way an older build deletes
	// them are cleared at the next start.
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'second'`); err != nil {
		t.Fatal(err)
	}
	if err := store.dropWhatDeletedSessionsLeft(); err != nil {
		t.Fatal(err)
	}
	if !eraseOwed(t, store) {
		t.Error("control: a start after a session was deleted outside this build owes no erase")
	}

	// A delete by this build clears what it leaves in its own transaction,
	// so the next start has nothing to do.
	if err := store.RetryOwedErasure(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession("first", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if eraseOwed(t, store) {
		t.Fatal("this build's delete left its erase owed")
	}
	if err := store.dropWhatDeletedSessionsLeft(); err != nil {
		t.Fatal(err)
	}
	if eraseOwed(t, store) {
		t.Error("a start after this build's own delete marked an erase owed")
	}
}

// TestTheFirstStartClearsWhatAnOlderBuildAloneLeft: a database this build
// has never opened holds no record of the sessions deleted on it, so the
// first start clears what they left.
func TestTheFirstStartClearsWhatAnOlderBuildAloneLeft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := openWithIndexedSecret(t, path, func(store *SQLiteStore, id string) {
		if err := store.StoreEvent(t.Context(), "summary_ready", `{"type":"summary_ready","session_id":"`+id+`","summary":"summary of `+id+`"}`); err != nil {
			t.Fatal(err)
		}
	})
	// The database as an older build left it: no record of this build, and
	// a delete that removed the rows only.
	if _, err := store.DB().Exec(`DROP TRIGGER IF EXISTS ` + sessionsDeletedTrigger); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertEventsOf(t, store, []string{"gone"}, []string{"kept"})
	assertSecretErased(t, path)
}

// TestAnInAppDeleteClearsWhatAnOutsideDeleteLeftBeforeItClearsTheRecord: a
// session deleted outside the app (with sqlite3, while the app runs) leaves
// its events, and the trigger records the delete. A later delete in the app
// clears that record, so it must also clear what the outside delete left;
// the next start no longer would.
func TestAnInAppDeleteClearsWhatAnOutsideDeleteLeftBeforeItClearsTheRecord(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"outside", "in-app", "kept"} {
		recordedSession(t, store, id, started.Add(time.Duration(i)*time.Hour), started.Add(time.Duration(i)*time.Hour+time.Minute), map[float64]string{1: "words of " + id})
	}
	storeSessionEvents(t, store, "outside", "in-app", "kept")
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'outside'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession("in-app", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	assertEventsOf(t, store, []string{"outside", "in-app"}, []string{"kept"})
}

// failWriteAheadLogOverwrite makes every overwrite of the write-ahead log
// fail as a full disk does.
func failWriteAheadLogOverwrite(t *testing.T) {
	t.Helper()
	original := postDeleteMaintenance
	t.Cleanup(func() { postDeleteMaintenance = original })
	postDeleteMaintenance = func(*sql.DB, string) error {
		return fmt.Errorf("overwrite the write-ahead log: %w", syscall.ENOSPC)
	}
}

// TestADeleteOnAFullDiskDoesNotKeepTheBackupUntilSpaceComesBack: on a full
// disk the overwrite of the write-ahead log fails, and so does taking the
// backup again, so the delete's erase stays owed. Its first retry, with the
// disk still full, erases the backup that holds the deleted conversation,
// which also frees the space the overwrite needs; it does not wait for the
// overwrite to succeed first.
func TestADeleteOnAFullDiskDoesNotKeepTheBackupUntilSpaceComesBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := openWithIndexedSecret(t, path, nil)
	failWriteAheadLogOverwrite(t)
	failVacuumInto(t, 100)

	if _, err := store.DeleteSession("gone", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if !eraseOwed(t, store) {
		t.Fatal("the erase of a delete on a full disk is not owed")
	}
	if err := store.RetryOwedErasure(); err == nil {
		t.Error("the retry reported no failure on a full disk")
	}
	if fileHolds(t, preMigrationBackupPath(path), indexSecret) {
		t.Errorf("on a full disk the retry left the pre-migration backup holding the deleted conversation's word %q", indexSecret)
	}
	if !eraseOwed(t, store) {
		t.Error("the erase of the write-ahead log, which still fails, is no longer owed")
	}
}

// TestAnOwedEraseOnAFullDiskStillErasesTheBackup: the retry of an owed erase,
// at a start or a sync sweep, erases the backup even when the overwrite of
// the write-ahead log fails, and reports the failure.
func TestAnOwedEraseOnAFullDiskStillErasesTheBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := openWithIndexedSecret(t, path, nil)
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	failWriteAheadLogOverwrite(t)
	failVacuumInto(t, 100)

	if err := store.eraseEveryOwed(false); err == nil {
		t.Error("the owed erase reported no failure on a full disk")
	}
	if fileHolds(t, preMigrationBackupPath(path), indexSecret) {
		t.Errorf("on a full disk the pre-migration backup still holds the deleted conversation's word %q", indexSecret)
	}
}
