package gdrive

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestADeletionMadeByARolledBackBuildIsStillReplaced fails if a segment
// deletion made by a build without the owed-folder ledger, after this
// version's one-time migration had already run (deploy/pi/update.sh rolls
// back automatically), is read as owing Drive nothing: the sweep would clear
// the marker and the session's Drive folder would keep the words.
func TestADeletionMadeByARolledBackBuildIsStillReplaced(t *testing.T) {
	fixture := newReplacementFixture(t)
	var seq int
	var name, path string
	if err := fixture.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}

	// The rolled-back build deletes a segment: it sets the marker and moves
	// the generation, and knows no ledger.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE sessions SET drive_replace_pending = 1, deletion_generation = deletion_generation + 1,
			title = 'Regenerated title', summary = 'clean summary', canonical_transcript = 'clean transcript',
			refinement_status = 'completed', summary_status = 'completed', sync_state = 'PENDING' WHERE id = '` + fixture.id + `'`,
		`INSERT INTO deleted_segments(session_id, audio_path, start, end, sha256_before, sha256_after, deleted_at, splice_complete)
			VALUES('` + fixture.id + `', '` + fixture.recording + `', 1, 2, 'before', 'after', '` + time.Now().UTC().Format(time.RFC3339Nano) + `', 1)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// This version starts again: its migration does not run a second time.
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var migrated, ledger int
	if err := store.DB().QueryRow(`SELECT count(*) FROM migrations WHERE name = 'record-owed-drive-replacement-folders'`).Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM drive_replacement_folders WHERE session_id = ?`, fixture.id).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if migrated != 1 || ledger != 0 {
		t.Fatalf("control: migration recorded %d times, %d ledger rows; want the migration already run and no ledger", migrated, ledger)
	}
	expected, err := fileSHA256(fixture.recording)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE deleted_segments SET sha256_after = ? WHERE session_id = ?`, expected, fixture.id); err != nil {
		t.Fatal(err)
	}
	o := NewOrchestrator(fixture.syncer, store)
	fixture.wire.allowDeletes = true

	if err := o.SyncSession(context.Background(), fixture.id); err != nil {
		t.Fatalf("sweep after the rollback: %v", err)
	}
	if !sameIDs(fixture.wire.deletedIDs, fixture.oldIDs) {
		t.Errorf("deleted %v, want the old files that hold the words %v", fixture.wire.deletedIDs, fixture.oldIDs)
	}
	sess, err := store.GetSession(fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.DriveReplacePending {
		t.Error("the deletion is still owed after its replacement")
	}
}
