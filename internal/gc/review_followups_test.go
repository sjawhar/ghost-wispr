package gc

import (
	"errors"
	"testing"
	"time"
)

func TestGCAlwaysHoldsWithoutDrive(t *testing.T) {
	store := newStore(t)
	const id = "20261001120000"
	summarized(t, store, id, time.Now().UTC().AddDate(0, 0, -40))

	collector := New(store, Config{MaxAgeDays: 30})
	deleted, err := collector.Run()
	if err != nil {
		t.Fatalf("run GC: %v", err)
	}
	if !errors.Is(collector.Held(), ErrDriveNotRunning) {
		t.Fatalf("GC hold = %v, want %v", collector.Held(), ErrDriveNotRunning)
	}
	if len(deleted) != 0 {
		t.Fatalf("GC deleted %v without Drive confirmation", deleted)
	}
	if _, err := store.GetSession(id); err != nil {
		t.Fatalf("GC removed the session without Drive confirmation: %v", err)
	}
}
