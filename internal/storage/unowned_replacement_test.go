package storage

import (
	"testing"
	"time"
)

// deleteMiddleSegment records session id with three segments and deletes the
// middle one, returning the span its recording owes a splice for.
func deleteMiddleSegment(t *testing.T, store *SQLiteStore, id string) DeletedSpan {
	t.Helper()
	segmentTestSession(t, store, id)
	return deleteSegmentAt(t, store, id, 1, 2, 4)
}

// deleteSegmentAt deletes the segment at index of session id, whose words are
// at start to end in recording.mp3, and returns its span.
func deleteSegmentAt(t *testing.T, store *SQLiteStore, id string, index int, start, end float64) DeletedSpan {
	t.Helper()
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	span := DeletedSpan{SessionID: id, AudioPath: "recording.mp3", Start: start, End: end, SHA256Before: "b", SHA256After: "a"}
	if err := deleteRefinedAudioTestSegment(t, store, id, segments[index].ID, span); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	return span
}

func replacementOwed(t *testing.T, store *SQLiteStore, id string) bool {
	t.Helper()
	return storeSession(t, store, id).DriveReplacePending
}

// TestASegmentDeletionWithNoDriveFolderOwesDriveNothingOnceSpliced fails if
// a segment deletion in a session that neither has a Drive folder nor has one
// merged into it stays marked as owing Drive a replacement once its recording
// is spliced: with sync off nothing clears the mark, so merge and delete stay
// refused and the status warning says Drive holds the words. Until the
// splice is done the mark must stay, so a first sync does not upload the
// unspliced recording.
func TestASegmentDeletionWithNoDriveFolderOwesDriveNothingOnceSpliced(t *testing.T) {
	store := newTestSQLiteStore(t)
	span := deleteMiddleSegment(t, store, "unsynced")
	if !replacementOwed(t, store, "unsynced") {
		t.Fatal("control: the deletion owes nothing while its splice is open")
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if replacementOwed(t, store, "unsynced") {
		t.Error("a spliced deletion in a session with no Drive folder still owes Drive a replacement")
	}
	if owed, err := store.OwedDriveReplacements(); err != nil || len(owed) != 0 {
		t.Errorf("owed Drive replacements = %+v, %v; want none", owed, err)
	}
	if _, err := store.DeleteSession("unsynced", ""); err != nil {
		t.Errorf("DeleteSession after the spliced deletion: %v, want the session deleted", err)
	}
}

// TestASegmentDeletionStaysOwedWhileDriveMayHoldItsWords fails if a spliced
// deletion stops owing Drive a replacement while a Drive folder may hold its
// words: the session's own, a merged source's, one a sync under way may have
// made and not recorded yet, or while another splice of the session is open.
// A sync under way that ends without a folder leaves nothing owed.
func TestASegmentDeletionStaysOwedWhileDriveMayHoldItsWords(t *testing.T) {
	t.Run("its own folder", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		segmentTestSession(t, store, "synced")
		syncTo(t, store, "synced", SyncStateSynced, "folder-1", 0, "")
		span := deleteSegmentAt(t, store, "synced", 1, 2, 4)
		if err := store.MarkSpliceComplete(&span); err != nil {
			t.Fatal(err)
		}
		if !replacementOwed(t, store, "synced") {
			t.Error("a spliced deletion owes nothing while the session's Drive folder holds the words")
		}
	})
	t.Run("a merged source's folder", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		base := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
		recordedSession(t, store, "s1", base, base.Add(time.Minute), map[float64]string{1: "s1 keeps", 2: "s1 deleted"})
		recordedSession(t, store, "s2", base.Add(time.Hour), base.Add(61*time.Minute), map[float64]string{1: "s2 keeps"})
		if err := store.MergeSessions("m", []string{"s1", "s2"}, base, base.Add(61*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-s1' WHERE id = 's1'`); err != nil {
			t.Fatal(err)
		}
		segments, err := store.GetSegments("m")
		if err != nil {
			t.Fatal(err)
		}
		span := DeletedSpan{SessionID: "m", AudioPath: "recording-s1.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}
		if err := deleteRefinedAudioTestSegment(t, store, "m", segments[1].ID, span); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkSpliceComplete(&span); err != nil {
			t.Fatal(err)
		}
		if !replacementOwed(t, store, "m") {
			t.Error("a spliced deletion owes nothing while a merged source's Drive folder holds the words")
		}
	})
	t.Run("another splice still open", func(t *testing.T) {
		store := newTestSQLiteStore(t)
		first := deleteMiddleSegment(t, store, "twice")
		second := deleteSegmentAt(t, store, "twice", 1, 4, 6)
		if err := store.MarkSpliceComplete(&first); err != nil {
			t.Fatal(err)
		}
		if !replacementOwed(t, store, "twice") {
			t.Error("a deletion owes nothing while another splice of the session is open")
		}
		if err := store.MarkSpliceComplete(&second); err != nil {
			t.Fatal(err)
		}
		if replacementOwed(t, store, "twice") {
			t.Error("both splices done, no Drive folder: the session still owes Drive a replacement")
		}
	})
	for _, tc := range []struct {
		name   string
		folder string
		owed   bool
	}{
		{"a sync under way that ends with no folder", "", false},
		{"a sync under way that ends with the folder it made", "folder-new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			segmentTestSession(t, store, "syncing")
			const claim = "claim-under-way"
			if _, ok, err := store.ClaimSync("syncing", SyncStatePending, claim, time.Now().UTC()); err != nil || !ok {
				t.Fatalf("ClaimSync = %v, %v", ok, err)
			}
			span := deleteSegmentAt(t, store, "syncing", 1, 2, 4)
			if err := store.MarkSpliceComplete(&span); err != nil {
				t.Fatal(err)
			}
			if !replacementOwed(t, store, "syncing") {
				t.Fatal("a spliced deletion owes nothing while a sync that may have made a folder still holds the session")
			}
			if ended, err := store.EndSync("syncing", claim, SyncStateRetryScheduled, tc.folder, 1, time.Now().UTC(), "upload failed"); err != nil || !ended {
				t.Fatalf("EndSync = %v, %v", ended, err)
			}
			if got := replacementOwed(t, store, "syncing"); got != tc.owed {
				t.Errorf("after the sync ended with folder %q: owed = %v, want %v", tc.folder, got, tc.owed)
			}
		})
	}
}

// TestSettlementKeepsADeletionOwedWhenAMergedSourceGainsAFolder fails if
// settlement trusts an empty ledger but ignores a merged source's folder. A
// sync can record that folder after the deletion made its ledger; it still
// holds the deleted words until the replacement finishes.
func TestSettlementKeepsADeletionOwedWhenAMergedSourceGainsAFolder(t *testing.T) {
	store := newTestSQLiteStore(t)
	base := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "source", base, base.Add(time.Minute), map[float64]string{1: "kept", 2: "deleted"})
	recordedSession(t, store, "other", base.Add(time.Hour), base.Add(61*time.Minute), map[float64]string{1: "other"})
	if err := store.MergeSessions("merged", []string{"source", "other"}, base, base.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	span := DeletedSpan{SessionID: "merged", AudioPath: "recording-source.mp3", Start: 1, End: 3, SHA256Before: "before", SHA256After: "after"}
	if err := deleteRefinedAudioTestSegment(t, store, "merged", segments[0].ID, span); err != nil {
		t.Fatalf("delete refined segment: %v", err)
	}
	generation, err := store.DeletionGeneration("merged")
	if err != nil {
		t.Fatal(err)
	}
	if folders, err := store.DriveReplacementFolders("merged", generation); err != nil || len(folders) != 0 {
		t.Fatalf("deletion ledger before the source sync = %+v, %v; want empty", folders, err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'source-folder' WHERE id = 'source'`); err != nil {
		t.Fatalf("record merged source folder: %v", err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatal(err)
	}
	if !replacementOwed(t, store, "merged") {
		t.Fatal("the deletion settled though its merged source's Drive folder can hold the deleted words")
	}
}
