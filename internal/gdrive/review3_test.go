package gdrive

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
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
	if err := store.FinishDriveReplacement(id, 2, []string{id}); !errors.Is(err, sql.ErrNoRows) {
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

// TestReview3ReadySessionWithOpenSpliceUploadsNothing proves a replacement
// cannot put the unspliced recording on Drive before its splice completes.
func TestReview3ReadySessionWithOpenSpliceUploadsNothing(t *testing.T) {
	for _, withFolder := range []bool{false, true} {
		t.Run(map[bool]string{false: "no folder", true: "synced folder"}[withFolder], func(t *testing.T) {
			store := openStore(t)
			const id = "20261106170000"
			recording, segments := round2Session(t, store, id, []string{"alpha stays.", "bravo secret."})
			wire, syncer := newWireDrive(t)
			o := NewOrchestrator(syncer, store)
			folder := ""
			if withFolder {
				folder = syncedSession(t, store, o, id)
			}
			span := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 1, End: 2, SHA256Before: "b", SHA256After: "a"}
			if err := store.DeleteSegment(id, segments[1].ID, span); err != nil {
				t.Fatalf("DeleteSegment: %v", err)
			}
			if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'clean', title = 'Clean' WHERE id = ?`,
				storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
				t.Fatal(err)
			}
			wire.allowDeletes = true
			if err := o.SyncSession(context.Background(), id); err != nil {
				t.Errorf("SyncSession: %v", err)
			}
			sess, _ := store.GetSession(id)
			if sess.GDriveFolderID != "" {
				folder = sess.GDriveFolderID
			}
			var audio string
			if folder != "" {
				for name, copies := range wire.in(folder) {
					if strings.HasPrefix(name, "audio-") {
						for _, copy := range copies {
							audio += string(copy.Content) + "|"
						}
					}
				}
			}
			if len(wire.deletedIDs) != 0 || !sess.DriveReplacePending {
				t.Errorf("an open splice still replaced or cleared: deleted=%v pending=%v", wire.deletedIDs, sess.DriveReplacePending)
			}
			if !withFolder && strings.Contains(audio, "UNSPLICED") {
				t.Errorf("the unspliced recording reached Drive")
			}
			_ = os.Remove(recording)
		})
	}
}

type spliceHookStore struct {
	*storage.SQLiteStore
	hook func()
}

func (s *spliceHookStore) HasPendingSplice(sessionID string) (bool, error) {
	open, err := s.SQLiteStore.HasPendingSplice(sessionID)
	if s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	return open, err
}

// TestReview3NoFolderBranchDoesNotClearANewerDeletion proves a no-folder
// replacement cannot clear a deletion that commits while it checks readiness.
func TestReview3NoFolderBranchDoesNotClearANewerDeletion(t *testing.T) {
	store := openStore(t)
	const id = "20261106180000"
	recording, segments := round2Session(t, store, id, []string{"alpha stays.", "bravo secret.", "charlie secret."})
	wire, syncer := newWireDrive(t)
	span1 := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 1, End: 2, SHA256Before: "b1", SHA256After: "a1"}
	if err := store.DeleteSegment(id, segments[1].ID, span1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recording, []byte("audio still holding CHARLIE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(&span1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ? WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	hooked := &spliceHookStore{SQLiteStore: store}
	hooked.hook = func() {
		span2 := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 2, End: 3, SHA256Before: "b2", SHA256After: "a2"}
		if err := store.DeleteSegment(id, segments[2].ID, span2); err != nil {
			t.Errorf("delete charlie: %v", err)
		}
	}
	o := NewOrchestrator(syncer, hooked)
	_ = o.SyncSession(context.Background(), id)
	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	var audio string
	if sess.GDriveFolderID != "" {
		for name, copies := range wire.in(sess.GDriveFolderID) {
			if strings.HasPrefix(name, "audio-") {
				for _, copy := range copies {
					audio += string(copy.Content) + "|"
				}
			}
		}
	}
	if strings.Contains(audio, "CHARLIE") || !sess.DriveReplacePending {
		t.Errorf("a newer deletion's marker was cleared and its unspliced audio synced: pending=%v audio=%q", sess.DriveReplacePending, audio)
	}
}
