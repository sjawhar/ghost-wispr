package gdrive

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestReview3StaleRowsDoNotFinishANewerGeneration proves that replacement
// rows from an older deletion cannot finish the current deletion generation.
func TestReview3StaleRowsDoNotFinishANewerGeneration(t *testing.T) {
	store := openStore(t)
	const id = "replacement-row-generation"
	if err := store.CreateSession(id, time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1, deletion_generation = 2 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"summary.md", "transcript.md", "audio-recording.mp3"} {
		if _, err := store.DB().Exec(`INSERT INTO drive_replacements(session_id, generation, name, old_id, new_id, state) VALUES(?, 1, ?, ?, ?, ?)`,
			id, name, "old-"+name, "new-"+name, storage.DriveReplacementDeleted); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := store.DriveReplacements(id, 2); err != nil || len(rows) != 0 {
		t.Errorf("DriveReplacements(gen 2) = %d rows, %v; want none", len(rows), err)
	}
	if err := store.FinishDriveReplacement(id, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FinishDriveReplacement(gen 2) with only gen-1 rows = %v, want a refusal", err)
	}
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.DriveReplacePending {
		t.Errorf("stale generation-1 rows cleared the generation-2 marker")
	}
}
