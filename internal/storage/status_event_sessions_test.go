package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The sessions of the status-event tests: statusGone is deleted, and the id
// of statusKept, which stays, begins with it, so a match on part of a message
// would remove statusKept's events with statusGone's.
const (
	statusGone = "20261116140"
	statusKept = "20261116140449"
)

// legacyStatusPayload is a component_status event as builds before events
// carried session_id stored it: the session only at the end of the message.
func legacyStatusPayload(t *testing.T, component, message string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": "component_status", "version": 1, "timestamp": "2026-11-16T14:05:00Z",
		"component": component, "status": "connected", "message": message,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// storeLegacyStatusEvents stores, as an older build did, a component_status
// event in every shape the session manager wrote about statusGone and about
// statusKept, and events that are not in one of those shapes, and returns the
// payloads that name statusGone and the payloads that must stay.
func storeLegacyStatusEvents(t *testing.T, store *SQLiteStore) (gone, kept []string) {
	t.Helper()
	for _, shape := range []struct{ component, prefix string }{
		{"refinement", "Batch refinement completed for session "},
		{"refinement", "Batch refinement failed for session "},
		{"summary", "Summarization failed for session "},
		{"summary", "Failed to store summary for session "},
		{"summary", "Failed to retrieve transcript for session "},
		{"sync", "Google Drive synced for session "},
		{"sync", "Google Drive sync completed for session "},
		{"sync", "Google Drive sync failed for session "},
	} {
		gone = append(gone, legacyStatusPayload(t, shape.component, shape.prefix+statusGone))
		kept = append(kept, legacyStatusPayload(t, shape.component, shape.prefix+statusKept))
	}
	kept = append(kept,
		// Not a shape the manager wrote: text after the id, another
		// component, a status that names no session, a payload that is not
		// JSON, and the current shape of a session that exists.
		legacyStatusPayload(t, "refinement", "Batch refinement completed for session "+statusGone+" again"),
		legacyStatusPayload(t, "mic", "Google Drive synced for session "+statusGone),
		legacyStatusPayload(t, "mic", "Microphone reconnected"),
		`not JSON: Google Drive synced for session `+statusGone,
		`{"type":"component_status","version":1,"timestamp":"2026-11-16T14:06:00Z","component":"sync","status":"connected","message":"Google Drive synced for session `+statusKept+`","session_id":"`+statusKept+`"}`,
	)
	for _, payload := range append(slices.Clone(gone), kept...) {
		if err := store.StoreEvent("component_status", payload); err != nil {
			t.Fatal(err)
		}
	}
	return gone, kept
}

// assertStoredEventsAre fails unless the stored events are exactly want.
func assertStoredEventsAre(t *testing.T, store *SQLiteStore, want []string) {
	t.Helper()
	got := storedEventPayloads(t, store)
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		for _, payload := range got {
			if !slices.Contains(want, payload) {
				t.Errorf("still stored: %s", payload)
			}
		}
		for _, payload := range want {
			if !slices.Contains(got, payload) {
				t.Errorf("removed, but must stay: %s", payload)
			}
		}
	}
}

// TestDeleteSessionRemovesTheStatusEventsAnOlderBuildStoredAboutIt: a build
// before this one stored the session manager's status events ("Batch
// refinement completed for session <id>") with the session only in the
// message. Deleting the session removes each one whose message ends with its
// id in a shape the manager wrote, and none that names another session, even
// one whose id begins with the deleted one's.
func TestDeleteSessionRemovesTheStatusEventsAnOlderBuildStoredAboutIt(t *testing.T) {
	store := newTestSQLiteStore(t)
	endedWith(t, store, statusGone, "/audio/gone.mp3", RefinementCompleted)
	endedWith(t, store, statusKept, "/audio/kept.mp3", RefinementCompleted)
	gone, kept := storeLegacyStatusEvents(t, store)
	assertStoredEventsAre(t, store, append(slices.Clone(gone), kept...))

	if _, err := store.DeleteSession(statusGone, t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	assertStoredEventsAre(t, store, kept)
}

// TestEveryStartRemovesTheStatusEventsAnOlderBuildStoredAboutADeletedSession:
// an older build's delete left the session's status events, with the session
// only in their message. The next start removes each one in a shape the
// session manager wrote, before it takes the pre-migration backup, and none
// that names a session the database has.
func TestEveryStartRemovesTheStatusEventsAnOlderBuildStoredAboutADeletedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	endedWith(t, store, statusGone, "/audio/gone.mp3", RefinementCompleted)
	endedWith(t, store, statusKept, "/audio/kept.mp3", RefinementCompleted)
	gone, kept := storeLegacyStatusEvents(t, store)
	// The older build's delete removes the row and keeps the events.
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = ?`, statusGone); err != nil {
		t.Fatal(err)
	}
	assertStoredEventsAre(t, store, append(slices.Clone(gone), kept...))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertStoredEventsAre(t, store, kept)
	backup, err := os.ReadFile(preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), `"Batch refinement completed for session `+statusKept+`"`) {
		t.Fatal("control: the pre-migration backup does not hold the kept status event, so its absence of the deleted one proves nothing")
	}
	if strings.Contains(string(backup), `"Batch refinement completed for session `+statusGone+`"`) {
		t.Error("the pre-migration backup still holds a status event of the deleted session")
	}
}

// TestStoredEventSessionReadsOnlyTheShapesTheManagerWrote: the session an
// older build's status event names is the id that ends its message, in a
// shape the session manager wrote for that component, as a whole session id.
func TestStoredEventSessionReadsOnlyTheShapesTheManagerWrote(t *testing.T) {
	for _, tc := range []struct {
		component, message, want string
	}{
		{"refinement", "Batch refinement completed for session 20261116140449", "20261116140449"},
		{"sync", "Google Drive sync failed for session merged-20261116_a", "merged-20261116_a"},
		{"summary", "Failed to retrieve transcript for session x", "x"},
		{"refinement", "Batch refinement completed for session ", ""},
		{"refinement", "Batch refinement completed for session 20261116140449 again", ""},
		{"refinement", "Batch refinement completed for session 2026/11/16", ""},
		{"refinement", "batch refinement completed for session 20261116140449", ""},
		{"sync", "Batch refinement completed for session 20261116140449", ""},
		{"mic", "Google Drive synced for session 20261116140449", ""},
		{"deepgram", "Deepgram connected", ""},
	} {
		if got := legacyStatusSession(tc.component, tc.message); got != tc.want {
			t.Errorf("legacyStatusSession(%q, %q) = %q, want %q", tc.component, tc.message, got, tc.want)
		}
	}
}
