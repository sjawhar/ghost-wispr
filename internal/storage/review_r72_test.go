package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestDeletingAMergedSessionScrubsItsRecordingSourcesFromAnOlderBackup: the
// first start of a build with a newer schema keeps the pre-migration backup in
// the older schema and scrubs a delete from it in place. recording_sources
// names a merged session (merged_session_id), its sources
// (source_session_id) and their recordings, with no session_id column; the
// scrub removes its rows of the deleted sessions too.
func TestDeletingAMergedSessionScrubsItsRecordingSourcesFromAnOlderBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"source-a", "source-b", "kept-a", "kept-b"} {
		at := started.Add(time.Duration(i) * 10 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(5*time.Minute), "/audio/"+id+".mp3", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"source-a", "source-b"}, started, started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("kept-merged", []string{"kept-a", "kept-b"}, started.Add(20*time.Minute), started.Add(35*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The database as an older build left it: one column fewer.
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

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backup, err := sql.Open("sqlite", preMigrationBackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	count := func(merged string) int {
		t.Helper()
		var n int
		if err := backup.QueryRow(`SELECT count(*) FROM recording_sources WHERE merged_session_id = ?`, merged).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("merged") != 2 || count("kept-merged") != 2 {
		t.Fatalf("control: the backup's recording_sources hold %d rows of merged and %d of kept-merged, want 2 each", count("merged"), count("kept-merged"))
	}

	if _, err := store.DeleteSession("merged", t.TempDir()); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var schemaKept int
	if err := backup.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name = 'drive_replace_error'`).Scan(&schemaKept); err != nil {
		t.Fatal(err)
	}
	if schemaKept != 0 {
		t.Fatal("control: the backup was taken again instead of scrubbed in place")
	}
	if n := count("merged"); n != 0 {
		t.Errorf("the backup keeps %d recording_sources rows of the deleted merged session", n)
	}
	if n := count("kept-merged"); n != 2 {
		t.Errorf("the backup keeps %d recording_sources rows of the merged session that stays, want 2", n)
	}
}

// TestDeleteTextClearsTheSummaryErrorOfTheMergedSourcesItScrubs: "Delete
// text" on a merged session empties the title, summary and transcripts of the
// merged sources that held the words, and the reason a summary of theirs
// failed too, which is about the summary it took away.
func TestDeleteTextClearsTheSummaryErrorOfTheMergedSourcesItScrubs(t *testing.T) {
	store := newTestSQLiteStore(t)
	secret := mergedSessionWithSecretAfter(t, store, func() {
		if _, err := store.DB().Exec(`UPDATE sessions SET summary_error = 'summarization failed: the model quoted the purple words' WHERE id = 'source-a'`); err != nil {
			t.Fatal(err)
		}
	})
	if got := storeSession(t, store, "source-a").SummaryError; got == "" {
		t.Fatal("control: the merged source has no summary error before Delete text")
	}
	if err := store.DeleteRefinedSegmentText("merged", secret.id, secret.revision); err != nil {
		t.Fatalf("DeleteRefinedSegmentText: %v", err)
	}
	if got := storeSession(t, store, "source-a"); got.Summary != "" || got.SummaryError != "" {
		t.Errorf("after Delete text the merged source keeps summary %q and summary error %q", got.Summary, got.SummaryError)
	}
}
