package storage

import (
	"testing"
	"time"
)

// R7 proof C: with SetMaxOpenConns(1), an open transaction holds the only
// connection, so DeleteSegment cannot commit until it ends. Canonicalize's
// read and write sit inside one such transaction.
func TestR7OpenTransactionExcludesDeleteSegment(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "s")
	segs, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getSegments(tx, "s"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- store.DeleteSegment("s", segs[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"})
	}()
	select {
	case err := <-done:
		t.Fatalf("DeleteSegment committed (%v) while a transaction held the connection", err)
	case <-time.After(300 * time.Millisecond):
		t.Log("DeleteSegment waited while the transaction was open")
	}
	if store.DB().Stats().WaitCount == 0 {
		t.Fatal("DeleteSegment did not wait for the transaction's only connection")
	}
	_ = tx.Rollback()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
