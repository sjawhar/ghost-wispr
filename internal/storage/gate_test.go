package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
)

// gatedStore is a store at a fresh path, and the path to open it again.
func gatedStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

// titleOf is session id's title in the database at path, read through a
// store of its own.
func titleOf(t *testing.T, path, id string) string {
	t.Helper()
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Title
}

// TestAClosedStoreRefusesEveryStatementAtOnce fails if a write after Close
// reaches SQLite, or fails with any error but stopping.ErrStopped, or if a
// read does: the store refuses every statement once it is closing.
func TestAClosedStoreRefusesEveryStatementAtOnce(t *testing.T) {
	store, path := gatedStore(t)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", at); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTitle("s1", "after the close"); !errors.Is(err, stopping.ErrStopped) {
		t.Errorf("a write after Close = %v, want stopping.ErrStopped", err)
	}
	if _, err := store.DB().Exec(`DELETE FROM sessions`); !errors.Is(err, stopping.ErrStopped) {
		t.Errorf("a statement on the closed store's handle = %v, want stopping.ErrStopped", err)
	}
	if _, err := store.GetSession("s1"); !errors.Is(err, stopping.ErrStopped) {
		t.Errorf("a read after Close = %v, want stopping.ErrStopped", err)
	}
	if got := titleOf(t, path, "s1"); got != "" {
		t.Errorf("the session's title after a write refused at Close = %q, want none", got)
	}
}

// TestCloseWaitsForAWriteUnderWay fails if Close returns, or closes the
// database, while a transaction it admitted is still open: the transaction
// must commit, and only then may Close return.
func TestCloseWaitsForAWriteUnderWay(t *testing.T) {
	store, path := gatedStore(t)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", at); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET title = 'in flight' WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a transaction it admitted was open", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := tx.Exec(`UPDATE sessions SET summary = 'still in flight' WHERE id = 's1'`); err != nil {
		t.Fatalf("a statement of the open transaction once Close began = %v, want it run: the transaction was admitted", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("the transaction's commit while Close waits = %v", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the transaction committed")
	}
	if got := titleOf(t, path, "s1"); got != "in flight" {
		t.Errorf("the title the transaction wrote = %q, want it kept", got)
	}
}

// TestCloseNeverClosesTheDatabaseUnderAWriterPastItsDeadline fails if
// CloseWithin closes the database while a transaction outlasts its deadline,
// if it does not report the deadline, or if the store takes another statement
// after it: the writer must still commit, on the open database, and every
// other statement be refused, one waiting for the store's one connection once
// it gets it. It is the shutdown's last step when store work outlasts its
// deadline.
func TestCloseNeverClosesTheDatabaseUnderAWriterPastItsDeadline(t *testing.T) {
	store, path := gatedStore(t)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", at); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET title = 'past the deadline' WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := store.CloseWithin(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseWithin with a writer past its deadline = %v, want the deadline", err)
	}
	other := make(chan error, 1)
	go func() { other <- store.UpdateTitle("s1", "another writer") }() // waits for the connection
	if err := tx.Commit(); err != nil {
		t.Fatalf("the writer's commit after the deadline = %v, want it committed on the database left open", err)
	}
	select {
	case err := <-other:
		if !errors.Is(err, stopping.ErrStopped) {
			t.Errorf("another write once the store is closing = %v, want stopping.ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another write once the store is closing did not end")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(t, path, "s1"); got != "past the deadline" {
		t.Errorf("the title the writer committed = %q, want it kept", got)
	}
}
