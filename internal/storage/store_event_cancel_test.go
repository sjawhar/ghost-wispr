package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestAStoreWaitingOnAWriteLockEndsWithinBusyTimeout fails if a cancelled
// StoreEvent waits on another connection's write lock past busy_timeout.
// SQLite's busy handler sleeps without heeding the driver's interrupt, so a
// cancel does not end that wait: it ends, stored or failed, when the lock is
// released or busy_timeout runs out. The Deepgram client's Close relies on
// that bound for a status store still running when it returns. It also fails
// if a store whose context lives does not wait for the lock to be released.
func TestAStoreWaitingOnAWriteLockEndsWithinBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	locker, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Close() }()
	writer, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()

	hold := func() func() {
		tx, err := locker.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO mcp_events(event_type, payload) VALUES('lock', '{}')`); err != nil {
			t.Fatal(err)
		}
		return func() { _ = tx.Rollback() }
	}
	busyTimeout := busyTimeoutMillis * time.Millisecond

	// Cancelled while it waits, with the lock held throughout: it ends within
	// busy_timeout of its start.
	release := hold()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- writer.StoreEvent(ctx, "component_status", `{"component":"deepgram","status":"connected"}`)
	}()
	select {
	case err := <-done:
		release()
		cancel()
		t.Fatalf("the store did not wait for the other connection's write lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		t.Logf("the cancelled store ended %v after it started, with %v", time.Since(start), err)
	case <-time.After(busyTimeout + time.Second):
		release()
		t.Fatalf("a cancelled store still waited on the write lock %v after it started, past busy_timeout (%v)", time.Since(start), busyTimeout)
	}
	release()

	// Not cancelled: it waits for the lock and then stores.
	release = hold()
	stored := make(chan error, 1)
	go func() {
		stored <- writer.StoreEvent(t.Context(), "component_status", `{"component":"deepgram","status":"draining"}`)
	}()
	time.Sleep(150 * time.Millisecond)
	release()
	select {
	case err := <-stored:
		if err != nil {
			t.Fatalf("a store that waited for the lock failed once it was released: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a store that waited for the lock did not finish within 3 s of its release")
	}
}
