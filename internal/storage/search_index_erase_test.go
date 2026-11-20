package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// indexSecret is one token: SQLite's search index (sessions_fts) keeps each
// token as one term, so the index holds these exact bytes. A secret with a
// hyphen is split into several terms and would never be found whole. No other
// word in these tests starts with "zy", so the term is not stored as a suffix
// of a neighbouring term.
const indexSecret = "zyxquorbelfin"

// keptIndexText is in the session that stays: every read below must see it,
// so a read that sees nothing cannot pass as absence.
const keptIndexText = "kept conversation text"

// fileHolds reports whether the file at path holds text, and false when the
// file does not exist.
func fileHolds(t *testing.T, path, text string) bool {
	t.Helper()
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(contents, []byte(text))
}

// indexedSummarySession ends session id with a summary holding summary,
// which the search index then holds.
func indexedSummarySession(t *testing.T, store *SQLiteStore, id string, started time.Time, summary string) {
	t.Helper()
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRefinementAtGeneration(id, "", RefinementCompleted, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration(id, "Title", summary, SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
}

// openWithIndexedSecret makes, at path, session "gone", whose summary holds
// indexSecret, and session "kept", whose summary holds keptIndexText, then
// reopens the store, which takes the pre-migration backup. ready runs on each
// session before the store closes. It checks the control: the database (or
// its log) and the backup hold the secret before anything is deleted.
func openWithIndexedSecret(t *testing.T, path string, ready func(store *SQLiteStore, id string)) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	indexedSummarySession(t, store, "gone", started, "The team named the project "+indexSecret+".")
	indexedSummarySession(t, store, "kept", started.Add(time.Hour), "The team said "+keptIndexText+".")
	for _, id := range []string{"gone", "kept"} {
		if ready != nil {
			ready(store, id)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if !fileHolds(t, path, indexSecret) && !fileHolds(t, path+"-wal", indexSecret) {
		t.Fatal("control: neither the database nor its log holds the secret before the delete")
	}
	if !fileHolds(t, preMigrationBackupPath(path), indexSecret) {
		t.Fatal("control: the backup does not hold the secret before the delete")
	}
	return store
}

// assertSecretErased fails unless the database, its log, its index and the
// pre-migration backup lack indexSecret, while the database and the backup
// still hold keptIndexText, so each read is shown able to see text.
func assertSecretErased(t *testing.T, path string) {
	t.Helper()
	assertDatabaseFilesLack(t, path, indexSecret, keptIndexText)
	backup := preMigrationBackupPath(path)
	if !fileHolds(t, backup, keptIndexText) {
		t.Fatal("no backup holds the kept text: the read saw no data")
	}
	if fileHolds(t, backup, indexSecret) {
		t.Errorf("the pre-migration backup still holds the deleted conversation's word %q", indexSecret)
	}
}

// TestDeleteSessionErasesItsWordsFromTheSearchIndex: the search index's
// delete only marks a row's terms deleted, and SQLite keeps the old index
// entries in the file (FTS5's secure-delete is off), so a whole-conversation
// delete left the conversation's words in the database and in the backup it
// takes again. The delete rebuilds the index in its transaction, as a segment
// deletion does.
func TestDeleteSessionErasesItsWordsFromTheSearchIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := openWithIndexedSecret(t, path, nil)
	if _, err := store.DeleteSession("gone", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	assertSecretErased(t, path)
}

// TestAgeDeleteErasesTheSessionFromTheIndexTheLogAndTheBackup: garbage
// collection's age delete removes a session for good, so its words go from
// the search index, the write-ahead log and the pre-migration backup, as
// they do for a person's delete.
func TestAgeDeleteErasesTheSessionFromTheIndexTheLogAndTheBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := openWithIndexedSecret(t, path, func(store *SQLiteStore, id string) {
		syncTo(t, store, id, SyncStateSynced, "folder-"+id, 0, "")
	})
	if deleted, err := deleteAgeEligibleForTest(store, "gone", "/audio/gone.mp3"); err != nil || !deleted {
		t.Fatalf("age delete = %v, %v; want deleted", deleted, err)
	}
	assertSecretErased(t, path)
}

// TestEveryStartErasesDeletedSessionsFromTheSearchIndex: a delete by a build
// before this one left the conversation's words in the search index, and so
// does a delete under the old build after an update rolls back, after this
// build has already started once (openWithIndexedSecret starts it). Every
// start rebuilds the index, and removes the deleted session's events, before
// it takes the pre-migration backup, so neither the database nor the backup
// keeps them.
func TestEveryStartErasesDeletedSessionsFromTheSearchIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store := openWithIndexedSecret(t, path, func(store *SQLiteStore, id string) {
		if err := store.StoreEvent(t.Context(), "summary_ready", `{"type":"summary_ready","session_id":"`+id+`","summary":"summary of `+id+`"}`); err != nil {
			t.Fatal(err)
		}
	})
	// The old build's delete: the rows go, and the index and the events keep
	// the conversation.
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !fileHolds(t, path, indexSecret) {
		t.Fatal("control: the old delete left no word in the database")
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertEventsOf(t, store, []string{"gone"}, []string{"kept"})
	assertSecretErased(t, path)
}
