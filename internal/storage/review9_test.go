package storage

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestReview9AnOwedEraseOutlivesItsSession fails if deleting a session (the
// route, or GC's age policy) while a segment deletion's erase from the
// write-ahead log and the backup is still owed drops that owed erase: the
// owed flag lives on the session's deleted_segments row, which the session's
// deletion cascades away. A control first shows the same retry erases the
// backup when the session is kept.
func TestReview9AnOwedEraseOutlivesItsSession(t *testing.T) {
	for _, deleteSessionFirst := range []bool{false, true} {
		name := map[bool]string{false: "control: session kept", true: "session deleted while the erase is owed"}[deleteSessionFirst]
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ghost-wispr.db")
			openWithSecret(t, path)
			store := openStoreWithBackup(t, path)
			failVacuumInto(t, 1)
			segments, err := store.GetSegments("backed-up")
			if err != nil {
				t.Fatal(err)
			}
			err = store.DeleteSegment("backed-up", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"})
			if !errors.Is(err, ErrSegmentPostCommitMaintenance) {
				t.Fatalf("DeleteSegment with a full disk = %v, want %v", err, ErrSegmentPostCommitMaintenance)
			}
			assertBackupHolds(t, path, backupSecret, true)
			if deleteSessionFirst {
				// The Drive replacement has finished, so the session may go.
				if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 0 WHERE id = 'backed-up'`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE drive_replacement_folders SET state = ? WHERE session_id = 'backed-up'`, DriveReplacementFolderReplaced); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DeleteSession("backed-up", ""); err != nil {
					t.Fatalf("DeleteSession: %v", err)
				}
				t.Logf("owed erasures after the session's deletion: %d", owedErasures(t, store))
				// Another session keeps the text assertBackupHolds reads as its
				// control, since the backup taken again no longer has this one.
				_, _ = segmentTestSession(t, store, "kept")
			}
			if err := store.RetryOwedErasure(); err != nil {
				t.Fatalf("RetryOwedErasure: %v", err)
			}
			assertBackupHolds(t, path, backupSecret, false)
		})
	}
}
