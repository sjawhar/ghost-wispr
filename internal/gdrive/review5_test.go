package gdrive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// Two sessions synced to Drive are merged (POST /api/sessions/merge or
// auto-merge), and the merged session syncs into a folder of its own. A
// person then deletes a segment of the merged session. The deleted words
// must leave the shared drive, including the source session's own folder,
// which the merge left on Drive.
func TestReview5DeletionInAMergedSessionLeavesTheSourceFolder(t *testing.T) {
	store := openStore(t)
	recA, _ := round2Session(t, store, "20261106190000", []string{"alpha stays.", "bravo merged secret."})
	_, _ = round2Session(t, store, "20261106193000", []string{"charlie stays."})
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folderA := syncedSession(t, store, o, "20261106190000")
	_ = syncedSession(t, store, o, "20261106193000")

	const merged = "20261106190000-merged"
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	if err := store.MergeSessions(merged, []string{"20261106190000", "20261106193000"}, started, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize(merged); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration(merged, "Merged", "merged summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	mergedFolder := syncedSession(t, store, o, merged)

	segments, err := store.GetSegments(merged)
	if err != nil {
		t.Fatal(err)
	}
	var secretID int64
	for _, s := range segments {
		if strings.Contains(s.Text, "bravo merged secret") {
			secretID = s.ID
		}
	}
	span := storage.DeletedSpan{SessionID: merged, AudioPath: recA, Start: 1, End: 2, SHA256Before: "b", SHA256After: contentHash(&SyncFile{Content: []byte("spliced clean audio")})}
	if err := deleteRefinedAudioForTest(t, store, merged, secretID, &span); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	if err := os.WriteFile(recA, []byte("spliced clean audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'clean', title = 'Clean' WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, merged); err != nil {
		t.Fatal(err)
	}
	wire.allowDeletes = true
	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("replacement: %v", err)
	}

	holds := func(folder string) []string {
		var names []string
		for name, copies := range wire.in(folder) {
			for _, c := range copies {
				if strings.Contains(string(c.Content), "bravo merged secret") || (name == "audio-"+filepath.Base(recA) && strings.Contains(string(c.Content), "UNSPLICED")) {
					names = append(names, name)
				}
			}
		}
		return names
	}
	t.Logf("merged folder %s still holding: %v; source folder %s still holding: %v", mergedFolder, holds(mergedFolder), folderA, holds(folderA))
	if got := holds(mergedFolder); len(got) != 0 {
		t.Errorf("merged folder still holds the deleted segment in %v", got)
	}
	if got := holds(folderA); len(got) != 0 {
		t.Errorf("the merged source's own Drive folder still holds the deleted segment in %v", got)
	}
}

// mergedDeletion merges two sessions synced to Drive, deletes the segment
// holding "bravo merged secret" from the merged session and completes its
// splice, refinement and summary. The merged session is synced into a folder
// of its own when syncMerged is set. It returns the merged id, source A's
// folder and recording.
func mergedDeletion(t *testing.T, store *storage.SQLiteStore, o *Orchestrator, syncMerged bool) (merged, folderA, recA string) {
	t.Helper()
	recA, _ = round2Session(t, store, "20261106190000", []string{"alpha stays.", "bravo merged secret."})
	_, _ = round2Session(t, store, "20261106193000", []string{"charlie stays."})
	folderA = syncedSession(t, store, o, "20261106190000")
	_ = syncedSession(t, store, o, "20261106193000")
	merged = "20261106190000-merged"
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	if err := store.MergeSessions(merged, []string{"20261106190000", "20261106193000"}, started, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize(merged); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration(merged, "Merged", "merged summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if syncMerged {
		_ = syncedSession(t, store, o, merged)
	}
	segments, err := store.GetSegments(merged)
	if err != nil {
		t.Fatal(err)
	}
	var secretID int64
	for i := range segments {
		if strings.Contains(segments[i].Text, "bravo merged secret") {
			secretID = segments[i].ID
		}
	}
	span := storage.DeletedSpan{SessionID: merged, AudioPath: recA, Start: 1, End: 2, SHA256Before: "b", SHA256After: contentHash(&SyncFile{Content: []byte("spliced clean audio")})}
	if err := deleteRefinedAudioForTest(t, store, merged, secretID, &span); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	if err := os.WriteFile(recA, []byte("spliced clean audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = 'clean', title = 'Clean' WHERE id = ?`,
		storage.RefinementCompleted, storage.SummaryCompleted, merged); err != nil {
		t.Fatal(err)
	}
	return merged, folderA, recA
}

// heldSecret names the files in folder that still hold the deleted words or
// the unspliced recording.
func heldSecret(wire *wireDrive, folder, recording string) []string {
	var names []string
	for name, copies := range wire.in(folder) {
		for _, c := range copies {
			if strings.Contains(string(c.Content), "bravo merged secret") || (name == "audio-"+filepath.Base(recording) && strings.Contains(string(c.Content), "UNSPLICED")) {
				names = append(names, name)
			}
		}
	}
	return names
}

// TestReplacementReachesASourceFolderWhenTheMergedSessionHasNone fails if a
// deletion in a merged session that was never synced (it has no folder of
// its own) skips its sources' Drive folders and goes straight to a first
// sync, leaving the words in them.
func TestReplacementReachesASourceFolderWhenTheMergedSessionHasNone(t *testing.T) {
	store := openStore(t)
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	merged, folderA, recA := mergedDeletion(t, store, o, false)
	wire.allowDeletes = true

	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	if got := heldSecret(wire, folderA, recA); len(got) != 0 {
		t.Errorf("the merged source's folder still holds the deleted segment in %v", got)
	}
	sess, err := store.GetSession(merged)
	if err != nil {
		t.Fatal(err)
	}
	if sess.DriveReplacePending {
		t.Error("the replacement of the source folders finished but the merged session still owes one")
	}
	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("first sync of the merged session: %v", err)
	}
	if sess, err = store.GetSession(merged); err != nil || sess.SyncState != storage.SyncStateSynced || sess.GDriveFolderID == "" {
		t.Errorf("after its first sync the merged session is %q in folder %q (%v), want synced in a folder of its own", sess.SyncState, sess.GDriveFolderID, err)
	}
}

// TestReplacementWaitsForATrashedSourceFolder fails if a merged source's
// folder in the Drive trash is skipped (its old files, words and all, come
// back with a restore) or the replacement reports success; once the folder
// is restored the next attempt replaces it.
func TestReplacementWaitsForATrashedSourceFolder(t *testing.T) {
	store := openStore(t)
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	merged, folderA, recA := mergedDeletion(t, store, o, true)
	wire.allowDeletes = true
	wire.trash(folderA)

	if err := o.SyncSession(context.Background(), merged); err == nil {
		t.Fatal("the replacement succeeded with a merged source's folder in the trash")
	}
	if sess, err := store.GetSession(merged); err != nil || !sess.DriveReplacePending {
		t.Fatalf("with a source folder in the trash, pending=%v (%v); want the replacement still owed", sess.DriveReplacePending, err)
	}
	wire.mu.Lock()
	delete(wire.trashed, folderA)
	wire.mu.Unlock()
	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("replacement once the folder was restored: %v", err)
	}
	if got := heldSecret(wire, folderA, recA); len(got) != 0 {
		t.Errorf("the restored source folder still holds the deleted segment in %v", got)
	}
}

// TestReplacementSkipsASourceFolderGoneForGood fails if a merged source's
// folder that was deleted for good (with its old files) blocks the
// replacement of the folders that still hold the words.
func TestReplacementSkipsASourceFolderGoneForGood(t *testing.T) {
	store := openStore(t)
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	merged, folderA, _ := mergedDeletion(t, store, o, true)
	wire.allowDeletes = true
	wire.deleteByHand(folderA)

	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("replacement with a source folder gone for good: %v", err)
	}
	if sess, err := store.GetSession(merged); err != nil || sess.DriveReplacePending {
		t.Errorf("pending=%v (%v), want the replacement finished", sess.DriveReplacePending, err)
	}
}
