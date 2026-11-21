package storeguard

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestTheGuardFailsAndRecordsAWriteAfterClose is the instrument's control: it
// fails if a write before Close is refused, or if writes after Close (an
// insert, an update and a delete, through the store's own methods) are not
// refused and recorded with the stack of the goroutine that made them.
func TestTheGuardFailsAndRecordsAWriteAfterClose(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	g := New(t, store)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("before", at); err != nil {
		t.Fatalf("a write before Close: %v", err)
	}
	if n := len(g.Writes()); n != 0 {
		t.Fatalf("a write before Close was recorded: %d", n)
	}

	g.Close(t)
	if err := store.CreateSession("after", at); err == nil {
		t.Error("an insert after Close was not refused")
	}
	if err := store.EndSession("before", at.Add(time.Minute), "", ""); err == nil {
		t.Error("an update after Close was not refused")
	}
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'before'`); err == nil {
		t.Error("a delete after Close was not refused")
	}
	writes := g.Writes()
	if len(writes) != 3 {
		t.Fatalf("%d writes recorded after Close, want the insert, the update and the delete", len(writes))
	}
	if !strings.Contains(writes[0], "INSERT on sessions") || !strings.Contains(writes[0], "TestTheGuardFailsAndRecordsAWriteAfterClose") {
		t.Fatalf("the insert's record does not name it and the test that made it:\n%s", writes[0])
	}
	if _, err := store.GetSession("before"); err != nil {
		t.Fatalf("a read after Close: %v", err)
	}
}
