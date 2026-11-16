package storage

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// recordSessionRows records, for each of ids, the two rows that name a
// session and that no foreign key deletes with it: a cut "Delete text and
// audio" refused, and a claimed summary request.
func recordSessionRows(t *testing.T, store *SQLiteStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := store.RecordRefusedCut(RefusedCut{SessionID: id, Recording: id + ".mp3", Start: 1, End: 2}); err != nil {
			t.Fatal(err)
		}
		if claimed, err := store.ClaimSummaryRequest(id, "prompt-hash"); err != nil || !claimed {
			t.Fatalf("ClaimSummaryRequest(%s) = %v, %v", id, claimed, err)
		}
	}
}

// assertSessionRowsOf fails unless the refused cuts and the summary requests
// name exactly the sessions in want.
func assertSessionRowsOf(t *testing.T, store *SQLiteStore, want ...string) {
	t.Helper()
	slices.Sort(want)
	for table, query := range map[string]string{
		"refused_cuts":     `SELECT session_id FROM refused_cuts ORDER BY session_id`,
		"summary_requests": `SELECT session_id FROM summary_requests ORDER BY session_id`,
	} {
		rows, err := store.DB().Query(query)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s names %v, want %v", table, got, want)
		}
	}
}

// TestDeleteSessionRemovesTheRowsThatNameIt: a refused cut and a summary
// request name their session, and no foreign key deletes them with it.
// Deleting a merged session deletes them for it and for the sessions merged
// into it, in the delete's transaction; another session's stay.
func TestDeleteSessionRemovesTheRowsThatNameIt(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 16, 14, 4, 49, 0, time.UTC)
	for i, id := range []string{"a", "b", "kept"} {
		at := started.Add(time.Duration(i) * 10 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(5*time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	recordSessionRows(t, store, "a", "b")
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recordSessionRows(t, store, "merged", "kept")
	if _, err := store.DB().Exec(`INSERT INTO summary_requests(session_id, prompt_hash) VALUES ('a', 'source-hash')`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeleteSession("merged", t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	assertSessionRowsOf(t, store, "kept")
}

// TestEveryStartRemovesTheRowsThatNameADeletedSession: an older build's
// delete left a session's refused cuts and summary requests. The next start
// removes them, and keeps those of a session that exists.
func TestEveryStartRemovesTheRowsThatNameADeletedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	endedWith(t, store, "gone", "/audio/gone.mp3", RefinementCompleted)
	endedWith(t, store, "kept", "/audio/kept.mp3", RefinementCompleted)
	recordSessionRows(t, store, "gone", "kept")
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	assertSessionRowsOf(t, store, "gone", "kept")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertSessionRowsOf(t, store, "kept")
}
