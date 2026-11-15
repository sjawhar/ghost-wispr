package gdrive

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// lockWaitFixture is a synced session whose first segment deletion is
// spliced, refined and summarized, so its replacement is owed and ready,
// with a second segment ("charlie second secret") left to delete while that
// replacement waits for the Drive lock.
type lockWaitFixture struct {
	store      *storage.SQLiteStore
	wire       *wireDrive
	syncer     *Syncer
	o          *Orchestrator
	id         string
	folder     string
	recording  string
	firstSpan  storage.DeletedSpan
	charlieID  int64
	transcript int // transcript.md copies in the folder before the attempt
}

const secondDeletedWords = "charlie second secret"

func newLockWaitFixture(t *testing.T) *lockWaitFixture {
	t.Helper()
	f := &lockWaitFixture{store: openStore(t), id: "20261106170000"}
	recording, segments := round2Session(t, f.store, f.id, []string{"alpha stays here.", "bravo first secret.", secondDeletedWords + "."})
	f.recording = recording
	f.wire, f.syncer = newWireDrive(t)
	f.o = NewOrchestrator(f.syncer, f.store)
	f.folder = syncedSession(t, f.store, f.o, f.id)
	firstAfter := []byte("first spliced recording")
	f.firstSpan = storage.DeletedSpan{SessionID: f.id, AudioPath: recording, Start: 1, End: 2, SHA256Before: "b", SHA256After: contentHash(&SyncFile{Content: firstAfter})}
	if err := deleteRefinedAudioForTest(t, f.store, f.id, segments[1].ID, &f.firstSpan); err != nil {
		t.Fatalf("delete bravo: %v", err)
	}
	if err := os.WriteFile(recording, firstAfter, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkSpliceComplete(&f.firstSpan); err != nil {
		t.Fatal(err)
	}
	f.refinedAndSummarized(t, "about charlie", "Charlie")
	current, err := f.store.GetSegments(f.id)
	if err != nil {
		t.Fatal(err)
	}
	f.charlieID = current[1].ID
	f.transcript = len(f.wire.in(f.folder)["transcript.md"])
	f.wire.allowDeletes = true
	return f
}

// refinedAndSummarized marks the session's latest deletion refined and
// summarized again, so its replacement is ready.
func (f *lockWaitFixture) refinedAndSummarized(t *testing.T, summary, title string) {
	t.Helper()
	if _, err := f.store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = ?, title = ? WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, summary, title, f.id); err != nil {
		t.Fatal(err)
	}
}

// attempt runs one sync of the session while an upload of another session
// holds the Drive lock: once the replacement has claimed the session, it
// commits and splices the second deletion, runs whileWaiting, then releases
// the lock. It returns the attempt's uploads, deletes and error.
func (f *lockWaitFixture) attempt(t *testing.T, whileWaiting func()) (uploads, deletes int, err error) {
	t.Helper()
	uploadsBefore, deletesBefore := f.counts()
	f.syncer.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- f.o.SyncSession(context.Background(), f.id) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if sess, _ := f.store.GetSession(f.id); sess.SyncState == storage.SyncStateSyncing {
			break
		}
		if time.Now().After(deadline) {
			f.syncer.mu.Unlock()
			t.Fatal("the replacement never claimed the session")
		}
	}
	secondAfter := []byte("second spliced recording")
	span := storage.DeletedSpan{SessionID: f.id, AudioPath: f.recording, Start: 1, End: 2, SHA256Before: f.firstSpan.SHA256After, SHA256After: contentHash(&SyncFile{Content: secondAfter})}
	if err := deleteRefinedAudioForTest(t, f.store, f.id, f.charlieID, &span); err != nil {
		t.Errorf("delete charlie: %v", err)
	}
	if err := os.WriteFile(f.recording, secondAfter, 0o644); err != nil {
		t.Errorf("write charlie splice: %v", err)
	}
	if err := f.store.MarkSpliceComplete(&span); err != nil {
		t.Errorf("mark charlie spliced: %v", err)
	}
	whileWaiting()
	f.syncer.mu.Unlock()
	err = <-done
	uploadsAfter, deletesAfter := f.counts()
	return uploadsAfter - uploadsBefore, deletesAfter - deletesBefore, err
}

// counts reads Drive's upload and delete counters.
func (f *lockWaitFixture) counts() (uploads, deletes int) {
	f.wire.mu.Lock()
	defer f.wire.mu.Unlock()
	return f.wire.uploads, f.wire.deletes
}

// transcriptsHolding counts the folder's transcript.md copies, from the
// from'th, that hold the second deletion's words.
func (f *lockWaitFixture) transcriptsHolding(from int) int {
	n := 0
	for i, c := range f.wire.in(f.folder)["transcript.md"] {
		if i >= from && strings.Contains(string(c.Content), secondDeletedWords) {
			n++
		}
	}
	return n
}

func (f *lockWaitFixture) pending(t *testing.T) bool {
	t.Helper()
	sess, err := f.store.GetSession(f.id)
	if err != nil {
		t.Fatal(err)
	}
	return sess.DriveReplacePending
}

// TestADeletionDuringTheDriveLockWaitStopsTheReplacement fails if a
// replacement that claimed its session, then waited for the Drive lock an
// upload of another session held, uploads anything after a second deletion
// of the session committed during the wait and is not yet refined and
// summarized. Its snapshot then holds the second deletion's words: it must
// do nothing, as the replacement did before the gdrive refactor, and leave
// the second deletion's replacement to the next sync, which removes the
// words.
func TestADeletionDuringTheDriveLockWaitStopsTheReplacement(t *testing.T) {
	f := newLockWaitFixture(t)
	uploads, deletes, err := f.attempt(t, func() {})
	if held := f.transcriptsHolding(f.transcript); uploads != 0 || deletes != 0 || held != 0 || err != nil {
		t.Fatalf("replacement after a deletion during its lock wait: %d uploads, %d deletes, %d new transcript copies holding the second deletion's words, err %v; want none and no error",
			uploads, deletes, held, err)
	}
	if !f.pending(t) {
		t.Fatal("the second deletion's replacement is no longer owed")
	}

	// The second deletion's refinement and summary finish; the next sync
	// replaces the files without its words.
	f.refinedAndSummarized(t, "clean", "Clean")
	if err := f.o.SyncSession(context.Background(), f.id); err != nil {
		t.Fatalf("next sync: %v", err)
	}
	if held := f.transcriptsHolding(0); held != 0 {
		t.Fatalf("%d transcript.md copies on Drive still hold the second deletion's words after the next sync", held)
	}
	if f.pending(t) {
		t.Fatal("the replacement is still owed after the next sync")
	}
}

// TestAReadyDeletionDuringTheDriveLockWaitIsReplacedAtOnce fails if a
// replacement that waited for the Drive lock, while a second deletion of
// its session committed and was refined and summarized, does not replace
// for that second deletion in the same attempt, as the replacement did
// before the gdrive refactor: the session's own sync after the summary
// found it claimed, so otherwise both deletions' words stay on Drive until
// the next sweep. Like the base, it uploads the transcript, the summary and
// the recording, deletes the three old files, and leaves nothing owed.
func TestAReadyDeletionDuringTheDriveLockWaitIsReplacedAtOnce(t *testing.T) {
	f := newLockWaitFixture(t)
	uploads, deletes, err := f.attempt(t, func() { f.refinedAndSummarized(t, "clean", "Clean") })
	held, owed := f.transcriptsHolding(0), f.pending(t)
	if uploads != 3 || deletes != 3 || held != 0 || owed || err != nil {
		t.Fatalf("replacement after a ready deletion during its lock wait: %d uploads, %d deletes, %d transcript.md copies holding the second deletion's words, replacement owed %v, err %v; want 3 uploads, 3 deletes, none holding the words, nothing owed, no error",
			uploads, deletes, held, owed, err)
	}
}
