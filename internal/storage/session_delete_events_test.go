package storage

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDeleteSessionRemovesItsStoredEvents: deleting a conversation from the unit removes the
// events that carry its text. On the office Pi on 2026-11-16, DELETE /api/sessions/{id} answered
// 204 and the session read back 404. But /api/events still served its summary_ready payloads
// (the full summary) and its live_transcript events, and the database file and the
// pre-migration backup still held its summary. Slice 3's precheck then refused a new run with
// "a copy of a phrase is already on the unit", whose own remedy is to delete that conversation.
func TestDeleteSessionRemovesItsStoredEvents(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 16, 7, 50, 0, 0, time.UTC)
	for _, id := range []string{"gone", "kept"} {
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	events := []struct{ eventType, payload string }{
		{"live_transcript", `{"type":"live_transcript","session_id":"gone","text":"Purple elephant seventeen."}`},
		{"summary_ready", `{"type":"summary_ready","session_id":"gone","summary":"The team named the project Purple Elephant 17."}`},
		{"summary_ready", `{"type":"summary_ready","session_id":"kept","summary":"Another conversation."}`},
	}
	for _, e := range events {
		if err := store.StoreEvent(e.eventType, e.payload); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.DeleteSession("gone", ""); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	left, err := store.GetEventsSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	keptSeen := false
	for _, e := range left {
		if strings.Contains(e.Payload, `"session_id":"gone"`) || strings.Contains(e.Payload, "Purple") {
			t.Errorf("event %d (%s) of the deleted session is still stored: %s", e.ID, e.EventType, e.Payload)
		}
		if strings.Contains(e.Payload, `"session_id":"kept"`) {
			keptSeen = true
		}
	}
	if !keptSeen {
		t.Error("the other session's event was removed too")
	}
}

// storedEventPayloads returns the payload of every stored event.
func storedEventPayloads(t *testing.T, store *SQLiteStore) []string {
	t.Helper()
	events, err := store.GetEventsSince(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	payloads := make([]string, 0, len(events))
	for _, e := range events {
		payloads = append(payloads, e.Payload)
	}
	return payloads
}

// assertEventsOf fails unless the stored events name each session in kept and
// none in gone.
func assertEventsOf(t *testing.T, store *SQLiteStore, gone, kept []string) {
	t.Helper()
	payloads := storedEventPayloads(t, store)
	for _, id := range gone {
		for _, payload := range payloads {
			if strings.Contains(payload, `"session_id":"`+id+`"`) {
				t.Errorf("an event of deleted session %s is still stored: %s", id, payload)
			}
		}
	}
	for _, id := range kept {
		found := false
		for _, payload := range payloads {
			found = found || strings.Contains(payload, `"session_id":"`+id+`"`)
		}
		if !found {
			t.Errorf("the events of session %s, which stays, were removed too", id)
		}
	}
}

func storeSessionEvents(t *testing.T, store *SQLiteStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		for _, e := range []struct{ eventType, payload string }{
			{"session_started", `{"type":"session_started","session_id":"` + id + `"}`},
			{"live_transcript", `{"type":"live_transcript","session_id":"` + id + `","text":"words of ` + id + `"}`},
			{"summary_ready", `{"type":"summary_ready","session_id":"` + id + `","summary":"summary of ` + id + `"}`},
		} {
			if err := store.StoreEvent(e.eventType, e.payload); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestDeleteSessionRemovesItsMergedSourcesStoredEvents: deleting a merged
// conversation deletes the sessions merged into it, so their events go too.
func TestDeleteSessionRemovesItsMergedSourcesStoredEvents(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 16, 7, 50, 0, 0, time.UTC)
	recordedSession(t, store, "source", started, started.Add(time.Minute), map[float64]string{1: "source text"})
	recordedSession(t, store, "other", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "other text"})
	recordedSession(t, store, "kept", started.Add(2*time.Hour), started.Add(121*time.Minute), map[float64]string{1: "kept text"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	storeSessionEvents(t, store, "source", "other", "merged", "kept")

	if _, err := store.DeleteSession("merged", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	assertEventsOf(t, store, []string{"source", "other", "merged"}, []string{"kept"})
}

// TestAgeDeleteRemovesTheDeletedSessionsStoredEvents: garbage collection's
// age policy removes a session's rows; it removes the session's events with
// them.
func TestAgeDeleteRemovesTheDeletedSessionsStoredEvents(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedAndSynced(t, store, "20261106140000", "/audio/a.mp3", RefinementCompleted)
	endedAndSynced(t, store, "20261106150000", "/audio/b.mp3", RefinementCompleted)
	storeSessionEvents(t, store, "20261106140000", "20261106150000")

	if deleted, err := deleteAgeEligibleForTest(store, "20261106140000", "/audio/a.mp3"); err != nil || !deleted {
		t.Fatalf("age delete = %v, %v; want deleted", deleted, err)
	}
	assertEventsOf(t, store, []string{"20261106140000"}, []string{"20261106150000"})
}

// TestDeleteSessionErasesItsTextFromTheWriteAheadLog: as a segment deletion
// does, deleting a conversation overwrites the write-ahead log, whose older
// frames still hold the rows and events it deleted.
func TestDeleteSessionErasesItsTextFromTheWriteAheadLog(t *testing.T) {
	const secret = "whole-conversation-delete-phrase-3b7f"
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 16, 7, 50, 0, 0, time.UTC)
	recordedSession(t, store, "gone", started, started.Add(time.Minute), map[float64]string{1: secret})
	recordedSession(t, store, "kept", started.Add(time.Hour), started.Add(61*time.Minute), map[float64]string{1: "kept conversation text"})
	if err := store.StoreEvent("live_transcript", `{"type":"live_transcript","session_id":"gone","text":"`+secret+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSession("gone", t.TempDir()); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	assertDatabaseFilesLack(t, path, secret, "kept conversation text")
}

// TestDeleteSessionErasesItFromThePreMigrationBackup: the pre-migration
// backup holds the database as this start found it. Deleting a conversation
// takes its rows and its stored events out of the backup too, as a segment
// deletion does: the backup is taken again when it has the database's
// schema, and is scrubbed in place, keeping its schema, when it is older.
func TestDeleteSessionErasesItFromThePreMigrationBackup(t *testing.T) {
	const kept = "kept-conversation-event-5e21"
	for _, olderSchema := range []bool{false, true} {
		name := "same schema"
		if olderSchema {
			name = "older schema"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ghost-wispr.db")
			// "backed-up" holds backupSecret in a segment, its transcript,
			// its summary and a live transcript event.
			openWithSecret(t, path)
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			endedWith(t, store, "kept", "/audio/kept.mp3", RefinementCompleted)
			if err := store.StoreEvent("summary_ready", `{"type":"summary_ready","session_id":"kept","summary":"`+kept+`"}`); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if olderSchema {
				old, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := old.Exec(`ALTER TABLE sessions DROP COLUMN drive_replace_error`); err != nil {
					t.Fatal(err)
				}
				if err := old.Close(); err != nil {
					t.Fatal(err)
				}
			}

			store = openStoreWithBackup(t, path)
			if _, err := store.DeleteSession("backed-up", t.TempDir()); err != nil {
				t.Fatalf("delete session: %v", err)
			}
			assertBackupOmits(t, path, backupSecret)
			backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = backup.Close() }()
			var rows, keptEvents, columns int
			if err := backup.QueryRow(`SELECT (SELECT count(*) FROM sessions WHERE id = 'backed-up') + (SELECT count(*) FROM segments WHERE session_id = 'backed-up') + (SELECT count(*) FROM mcp_events WHERE payload LIKE '%"session_id":"backed-up"%')`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Errorf("the backup keeps %d rows or events of the deleted session, want none", rows)
			}
			if err := backup.QueryRow(`SELECT count(*) FROM mcp_events WHERE payload LIKE ?`, "%"+kept+"%").Scan(&keptEvents); err != nil {
				t.Fatal(err)
			}
			if keptEvents != 1 {
				t.Errorf("the backup holds %d events of the session that stays, want 1", keptEvents)
			}
			if err := backup.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name = 'drive_replace_error'`).Scan(&columns); err != nil {
				t.Fatal(err)
			}
			if olderSchema && columns != 0 {
				t.Error("the backup of the older schema was replaced by the current database")
			}
		})
	}
}

// TestEveryStartRemovesEventsOfSessionsThatNoLongerExist: a whole-conversation
// delete by a build before this one left the conversation's events, which
// /api/events served for up to 7 days (the office Pi held them after its
// acceptance run). So does a delete under the old build after an update rolls
// back (deploy/pi/update.sh), after this build has already started once. Every
// start removes every event that names a session the database no longer has,
// before it takes the pre-migration backup, and erases them from the
// write-ahead log. Events of a session that exists, and events that name no
// session, stay.
func TestEveryStartRemovesEventsOfSessionsThatNoLongerExist(t *testing.T) {
	const secret = "orphaned-event-phrase-0c4d"
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	endedWith(t, store, "kept", "/audio/kept.mp3", RefinementCompleted)
	endedWith(t, store, "deleted-before", "/audio/deleted-before.mp3", RefinementCompleted)
	storeSessionEvents(t, store, "kept")
	for _, e := range []struct{ eventType, payload string }{
		{"summary_ready", `{"type":"summary_ready","session_id":"deleted-before","summary":"` + secret + `"}`},
		{"live_transcript", `{"type":"live_transcript","session_id":"deleted-before","text":"` + secret + `"}`},
		{"status_changed", `{"type":"status_changed","paused":true}`},
	} {
		if err := store.StoreEvent(e.eventType, e.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// This build has started once; the update then rolls back, and the old
	// build's delete removes the rows and keeps the events.
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'deleted-before'`); err != nil {
		t.Fatal(err)
	}
	orphans := 0
	for _, payload := range storedEventPayloads(t, store) {
		if strings.Contains(payload, secret) {
			orphans++
		}
	}
	if orphans != 2 {
		t.Fatalf("control: %d events of the deleted session are stored before the next start, want 2", orphans)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertEventsOf(t, store, []string{"deleted-before"}, []string{"kept"})
	paused := false
	for _, payload := range storedEventPayloads(t, store) {
		paused = paused || strings.Contains(payload, `"paused":true`)
	}
	if !paused {
		t.Error("the start removed an event that names no session")
	}
	assertBackupOmits(t, path, secret)
	assertDatabaseFilesLack(t, path, secret, "summary of kept")
}

// TestStoreEventKeepsNoEventOfASessionThatIsGone: a summary is committed
// first and broadcast after, and the broadcast stores its event. A delete
// that commits between the two must not leave that event behind, with the
// summary in it. An event of a session that exists, and an event that names
// no session (or that is not JSON), are stored.
func TestStoreEventKeepsNoEventOfASessionThatIsGone(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, "kept", "/audio/kept.mp3", RefinementCompleted)
	for _, payload := range []string{
		`{"type":"summary_ready","session_id":"gone","summary":"The deleted conversation's summary."}`,
		`{"type":"summary_ready","session_id":"kept","summary":"summary of kept"}`,
		`{"type":"status_changed","paused":true}`,
		`not valid JSON`,
	} {
		if err := store.StoreEvent("summary_ready", payload); err != nil {
			t.Fatalf("StoreEvent(%s): %v", payload, err)
		}
	}
	payloads := storedEventPayloads(t, store)
	assertEventsOf(t, store, []string{"gone"}, []string{"kept"})
	for _, want := range []string{`"paused":true`, `not valid JSON`} {
		found := false
		for _, payload := range payloads {
			found = found || strings.Contains(payload, want)
		}
		if !found {
			t.Errorf("the event %s, which names no session, was not stored", want)
		}
	}
}
