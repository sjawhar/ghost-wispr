package gdrive

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestADeletionDuringTheDriveLockWaitStopsTheReplacement fails if a
// replacement that claimed its session, then waited for the Drive lock an
// upload of another session held, uploads anything after a second deletion
// of the session committed during the wait. Its snapshot then holds the
// second deletion's words: it must do nothing, as the replacement did before
// the gdrive refactor, and leave the second deletion's replacement to the
// next sync, which removes the words.
func TestADeletionDuringTheDriveLockWaitStopsTheReplacement(t *testing.T) {
	store := openStore(t)
	const id = "20261106170000"
	recording, segments := round2Session(t, store, id, []string{"alpha stays here.", "bravo first secret.", "charlie second secret."})
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folder := syncedSession(t, store, o, id)
	firstAfter := []byte("first spliced recording")
	span := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 1, End: 2, SHA256Before: "b", SHA256After: contentHash(&SyncFile{Content: firstAfter})}
	if err := deleteRefinedAudioForTest(t, store, id, segments[1].ID, &span); err != nil {
		t.Fatalf("delete bravo: %v", err)
	}
	if err := os.WriteFile(recording, firstAfter, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'about charlie', title = 'Charlie' WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	charlieID := current[1].ID
	before := len(wire.in(folder)["transcript.md"])
	uploadsBefore, deletesBefore := wire.uploads, wire.deletes

	wire.allowDeletes = true
	syncer.mu.Lock() // an upload of another session holds the Drive lock
	done := make(chan error, 1)
	go func() { done <- o.SyncSession(context.Background(), id) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if sess, _ := store.GetSession(id); sess.SyncState == storage.SyncStateSyncing {
			break
		}
		if time.Now().After(deadline) {
			syncer.mu.Unlock()
			t.Fatal("the replacement never claimed the session")
		}
	}
	secondAfter := []byte("second spliced recording")
	span2 := storage.DeletedSpan{SessionID: id, AudioPath: recording, Start: 1, End: 2, SHA256Before: span.SHA256After, SHA256After: contentHash(&SyncFile{Content: secondAfter})}
	if err := deleteRefinedAudioForTest(t, store, id, charlieID, &span2); err != nil {
		t.Errorf("delete charlie: %v", err)
	}
	if err := os.WriteFile(recording, secondAfter, 0o644); err != nil {
		t.Errorf("write charlie splice: %v", err)
	}
	if err := store.MarkSpliceComplete(&span2); err != nil {
		t.Errorf("mark charlie spliced: %v", err)
	}
	syncer.mu.Unlock()
	syncErr := <-done

	newWithCharlie := 0
	for i, c := range wire.in(folder)["transcript.md"] {
		if i >= before && strings.Contains(string(c.Content), "charlie second secret") {
			newWithCharlie++
		}
	}
	if uploads, deletes := wire.uploads-uploadsBefore, wire.deletes-deletesBefore; uploads != 0 || deletes != 0 || newWithCharlie != 0 || syncErr != nil {
		t.Fatalf("replacement after a deletion during its lock wait: %d uploads, %d deletes, %d new transcript copies holding the second deletion's words, err %v; want none and no error",
			uploads, deletes, newWithCharlie, syncErr)
	}
	if sess, _ := store.GetSession(id); !sess.DriveReplacePending {
		t.Fatal("the second deletion's replacement is no longer owed")
	}

	// The second deletion's refinement and summary finish; the next sync
	// replaces the files without its words.
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'clean', title = 'Clean' WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, id); err != nil {
		t.Fatal(err)
	}
	if err := o.SyncSession(context.Background(), id); err != nil {
		t.Fatalf("next sync: %v", err)
	}
	for _, c := range wire.in(folder)["transcript.md"] {
		if strings.Contains(string(c.Content), "charlie second secret") {
			t.Fatalf("a transcript.md on Drive still holds the second deletion's words after the next sync")
		}
	}
	if sess, _ := store.GetSession(id); sess.DriveReplacePending {
		t.Fatal("the replacement is still owed after the next sync")
	}
}
