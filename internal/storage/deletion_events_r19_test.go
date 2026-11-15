package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

const deletionEventPhrase = "okay deletion phrase"

// deletionEventMerge merges n, which never recorded and holds the phrase in
// an untimed row, with s, which recorded. Both have a stored live transcript
// event; the merged session has a stored session_ended event whose payload
// holds the phrase. It returns the id of n's row in the merge.
func deletionEventMerge(t *testing.T, store *SQLiteStore) int64 {
	t.Helper()
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"n", "s"} {
		if err := store.CreateSession(id, start); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendSegment("n", transcribeSegment(deletionEventPhrase, 0, start)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("n", start.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", transcribeSegment("sierra words", 0, start)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", start.Add(time.Minute), "/audio/s.mp3"); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct{ eventType, payload string }{
		{"live_transcript", `{"type":"live_transcript","session_id":"n","text":"` + deletionEventPhrase + `","start_time":1,"end_time":2}`},
		{"live_transcript", `{"type":"live_transcript","session_id":"s","text":"sierra words","start_time":1,"end_time":2}`},
	} {
		if err := store.StoreEvent(event.eventType, event.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"n", "s"}, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEvent("session_ended", deletionEventEndedPayload); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if segments[i].Text == deletionEventPhrase {
			return segments[i].ID
		}
	}
	t.Fatalf("control: n's row is missing from the merge: %+v", segments)
	return 0
}

const deletionEventEndedPayload = `{"type":"session_ended","session_id":"merged","duration":60,"note":"` + deletionEventPhrase + `"}`

func countEvents(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

const (
	holderLiveEvents = `SELECT count(*) FROM mcp_events WHERE event_type = 'live_transcript' AND json_extract(payload, '$.session_id') IN ('n', 'merged')`
	otherLiveEvents  = `SELECT count(*) FROM mcp_events WHERE event_type = 'live_transcript' AND json_extract(payload, '$.session_id') = 's' AND payload LIKE '%sierra words%'`
	endedEventKept   = `SELECT count(*) FROM mcp_events WHERE event_type = 'session_ended' AND payload = ?`
)

// assertDeletionEvents fails unless db dropped every live transcript event of
// the sessions that held n's row (n and the merge), by session, kept s's,
// and left the session_ended event exactly as stored: no event is edited by
// matching its text.
func assertDeletionEvents(t *testing.T, where string, db *sql.DB) {
	t.Helper()
	if live := countEvents(t, db, holderLiveEvents); live != 0 {
		t.Errorf("%s keeps %d live transcript events of the sessions that held the segment, want none", where, live)
	}
	if live := countEvents(t, db, otherLiveEvents); live != 1 {
		t.Errorf("%s holds %d live events of s, which did not hold the segment, want 1 unchanged", where, live)
	}
	if kept := countEvents(t, db, endedEventKept, deletionEventEndedPayload); kept != 1 {
		t.Errorf("%s edited or removed the session_ended event (%d left as stored), want it unchanged: a deletion does not match event text", where, kept)
	}
}

func TestDeletionDropsLiveEventsBySessionAndEditsNoEventText(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	target := deletionEventMerge(t, store)
	if err := store.DeleteRefinedSegmentText("merged", target, 0); err != nil {
		t.Fatal(err)
	}
	assertDeletionEvents(t, "the database", store.DB())
}

// The scrub of an older-schema backup drops the live transcript events of
// every session that held the segment, as the database does, not only those
// of the session its row names there, and edits no event text.
func TestBackupScrubDropsLiveEventsBySessionAndEditsNoEventText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-backup.db")
	first, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	target := deletionEventMerge(t, first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	migrateAfterTheBackup(t, store, path)
	backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	if live := countEvents(t, backup, holderLiveEvents); live != 1 {
		t.Fatalf("control: the backup holds %d live events of n before the deletion, want 1", live)
	}
	if err := store.DeleteRefinedSegmentText("merged", target, 0); err != nil {
		t.Fatal(err)
	}
	assertDeletionEvents(t, "the backup", backup)
}
