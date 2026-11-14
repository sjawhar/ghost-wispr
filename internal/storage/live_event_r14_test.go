package storage

import (
	"path/filepath"
	"testing"
)

func TestDropLiveTranscriptEventsLeavesUnparseableEventsOfOtherSessions(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.StoreEvent("live_transcript", "not valid JSON"); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("live_transcript", `{"session_id":"target","text":"delete me"}`); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := dropLiveTranscriptEvents(tx, []string{"target"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var invalid, target int
	if err := store.DB().QueryRow(`SELECT count(*) FROM mcp_events WHERE payload = 'not valid JSON'`).Scan(&invalid); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM mcp_events WHERE payload LIKE '%delete me%'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if invalid != 1 || target != 0 {
		t.Fatalf("live events after target scrub = invalid:%d target:%d, want invalid retained and target removed", invalid, target)
	}
}
