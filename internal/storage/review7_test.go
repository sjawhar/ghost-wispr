package storage

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestAnEmptyLedgerIsRebuiltForAPendingDeletion fails if a pending deletion
// whose ledger is empty (made by a build without one, after this version's
// migration had run) is read as owing no folder, or if a ledger is rebuilt
// for a generation the session is no longer at.
func TestAnEmptyLedgerIsRebuiltForAPendingDeletion(t *testing.T) {
	store := newTestSQLiteStore(t)
	deleteSecret(t, store, nestedMerge(t, store))
	generation, err := store.DeletionGeneration("m2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM drive_replacement_folders WHERE session_id = 'm2'`); err != nil {
		t.Fatal(err)
	}

	folders, err := store.EnsureDriveReplacementFolders("m2", generation)
	if err != nil {
		t.Fatal(err)
	}
	var owed []string
	for _, folder := range folders {
		if folder.State == DriveReplacementFolderOwed {
			owed = append(owed, folder.FolderSessionID)
		}
	}
	if !slices.Equal(owed, []string{"m1", "s1", "s3"}) {
		t.Errorf("rebuilt ledger owes %v, want every folder that can hold the words: m1, s1 and s3", owed)
	}

	// An earlier generation is not rebuilt, and asking for it leaves the
	// current ledger, a folder already settled included, as it is.
	if err := store.MarkDriveReplacementFolder("m2", generation, "m1", DriveReplacementFolderGone); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.EnsureDriveReplacementFolders("m2", generation-1); err != nil || len(stale) != 0 {
		t.Fatalf("ledger of an earlier generation = %+v, %v; want none rebuilt", stale, err)
	}
	if folders, err = store.DriveReplacementFolders("m2", generation); err != nil {
		t.Fatal(err)
	}
	if len(folders) != 3 || folders[0].FolderSessionID != "m1" || folders[0].State != DriveReplacementFolderGone {
		t.Errorf("current ledger after asking for an earlier generation = %+v, want m1 still gone", folders)
	}
}

// TestMarkSyncedOwesTheFolderOfAPendingDeletion fails if a sync that held the
// session while one of its segments was deleted, and marked it synced, leaves
// the folder it made (which holds the words) out of the deletion's ledger.
func TestMarkSyncedOwesTheFolderOfAPendingDeletion(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "marked")
	if _, claimed, err := store.ClaimSync("marked", SyncStatePending, "first-sync", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("ClaimSync = %v, %v", claimed, err)
	}
	segments, err := store.GetSegments("marked")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRefinedAudioTestSegment(t, store, "marked", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	marked, err := store.MarkSynced("marked", "first-sync", "folder-marked-during-the-deletion", func(Session, []transcribe.Segment) bool { return true })
	if err != nil || !marked {
		t.Fatalf("MarkSynced = %v, %v", marked, err)
	}
	generation, err := store.DeletionGeneration("marked")
	if err != nil {
		t.Fatal(err)
	}
	folders, err := store.DriveReplacementFolders("marked", generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].FolderSessionID != "marked" || folders[0].State != DriveReplacementFolderOwed {
		t.Errorf("owed folders = %+v, want the folder the sync made", folders)
	}
}

// TestAReplacedMarkNeedsTheFolderReplacement fails if a folder is recorded
// replaced with no replacement recorded for it: every replacement uploads
// summary.md and transcript.md, each with a row, so a folder with none was
// never replaced.
func TestAReplacedMarkNeedsTheFolderReplacement(t *testing.T) {
	store := newTestSQLiteStore(t)
	deleteSecret(t, store, nestedMerge(t, store))
	generation, err := store.DeletionGeneration("m2")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDriveReplacementFolder("m2", generation, "s3", DriveReplacementFolderReplaced); err == nil {
		t.Fatal("a folder with no replacement rows was recorded replaced")
	}
	// A folder that held no files gets its documents with no old id.
	for _, name := range []string{"summary.md", "transcript.md"} {
		if err := store.StartDriveReplacement("s3", name, generation, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkDriveReplacementUploaded("s3", name, generation, "new-"+name); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkDriveReplacementDeleted("s3", name, "", generation); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkDriveReplacementFolder("m2", generation, "s3", DriveReplacementFolderReplaced); err != nil {
		t.Errorf("a folder that held no files, replaced: %v", err)
	}
}

// TestGCAndMergeCheckTheLedger fails if GC or a merge treats a synced
// session whose Drive folder another deletion still owes a replacement as
// free, as DeleteSession does not.
func TestGCAndMergeCheckTheLedger(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"owed-folder", "other"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, sync_status = ?, sync_state = ? WHERE id = 'owed-folder'`, SummaryCompleted, SyncSynced, SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO drive_replacement_folders(session_id, generation, folder_session_id, state) VALUES('other', 1, 'owed-folder', ?)`, DriveReplacementFolderOwed); err != nil {
		t.Fatal(err)
	}
	recordings, err := store.Recordings()
	if err != nil {
		t.Fatal(err)
	}
	for _, recording := range recordings {
		if recording.ID == "owed-folder" && recording.Removable {
			t.Error("GC may remove the recording of a session whose folder is owed")
		}
	}
	deleted, err := deleteAgeEligibleForTest(store, "owed-folder", "/audio/owed-folder.mp3")
	var block *GCSourceBlockError
	if !errors.As(err, &block) || block.SessionID != "owed-folder" || block.Reason != "Drive folder replacement owed" || deleted {
		t.Fatalf("GC deleted a session whose folder is owed (%v, %+v)", err, block)
	}
	if err := store.MergeSessions("merged", []string{"owed-folder", "other"}, started, started.Add(2*time.Hour)); !errors.Is(err, ErrMergeReplacementPending) {
		t.Errorf("merge of a session whose folder is owed = %v, want %v", err, ErrMergeReplacementPending)
	}
}
