package storage

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

const ledgerSecret = "zebraledgersecret"

// recordedSession records an ended session with one segment per text, each
// stamped at started plus its offset in seconds and with stream times from
// start (2 seconds each).
func recordedSession(t *testing.T, store *SQLiteStore, id string, started, ended time.Time, texts map[float64]string) {
	t.Helper()
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	offsets := make([]float64, 0, len(texts))
	for offset := range texts {
		offsets = append(offsets, offset)
	}
	slices.Sort(offsets)
	for _, offset := range offsets {
		if err := store.AppendSegment(id, transcribe.Segment{Text: texts[offset], StartTime: offset, EndTime: offset + 2,
			Timestamp: started.Add(time.Duration(offset * float64(time.Second)))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, ended, "recording-"+id+".mp3"); err != nil {
		t.Fatal(err)
	}
}

// nestedMerge records s1 (whose segment at 2 s holds ledgerSecret, and whose
// segment at 300 s is stamped after s1 ended), s2 (whose first segment is
// stamped at its very start) and s3, merges s1 and s2 into m1 and m1 and s3
// into m2, gives s1, m1 and s3 Drive folders, and returns the id of the
// secret segment in m2.
func nestedMerge(t *testing.T, store *SQLiteStore) int64 {
	t.Helper()
	base := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	recordedSession(t, store, "s1", base, base.Add(time.Minute), map[float64]string{1: "s1 keeps", 2: "s1 " + ledgerSecret, 300: "s1 late final"})
	recordedSession(t, store, "s2", base.Add(30*time.Minute), base.Add(31*time.Minute), map[float64]string{0: "s2 first", 1: "s2 keeps"})
	recordedSession(t, store, "s3", base.Add(time.Hour), base.Add(61*time.Minute), map[float64]string{1: "s3 keeps"})
	if err := store.MergeSessions("m1", []string{"s1", "s2"}, base, base.Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("m1", "About "+ledgerSecret, "The summary of "+ledgerSecret+".", SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("m2", []string{"m1", "s3"}, base, base.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-' || id WHERE id IN ('s1', 'm1', 's3')`); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{
		`{"type":"live_transcript","session_id":"s1","text":"s1 ` + ledgerSecret + `","start_time":2,"end_time":4}`,
		`{"type":"live_transcript","session_id":"s3","text":"s3 sibling words","start_time":2,"end_time":4}`,
	} {
		if err := store.StoreEvent("live_transcript", event); err != nil {
			t.Fatal(err)
		}
	}
	segments, err := store.GetSegments("m2")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, ledgerSecret) {
			return segment.ID
		}
	}
	t.Fatal("no secret segment in m2")
	return 0
}

func deleteSecret(t *testing.T, store *SQLiteStore, segmentID int64) {
	t.Helper()
	if err := store.DeleteSegment("m2", segmentID, DeletedSpan{AudioPath: "recording-s1.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
}

// TestADeletionInANestedMergeReachesEverySourceThatHeldTheWords fails if a
// deletion in m2 misses a session merged into it through another merge (s1,
// through m1): its row would keep the words and its Drive folder would not be
// owed a replacement.
func TestADeletionInANestedMergeReachesEverySourceThatHeldTheWords(t *testing.T) {
	store := newTestSQLiteStore(t)
	deleteSecret(t, store, nestedMerge(t, store))

	for _, id := range []string{"s1", "m1"} {
		sess := storeSession(t, store, id)
		if strings.Contains(sess.CanonicalTranscript+sess.Summary+sess.Title, ledgerSecret) {
			t.Errorf("%s still holds the deleted words: %+v", id, sess)
		}
		if !strings.Contains(sess.CanonicalTranscript, "s1 keeps") {
			t.Errorf("%s lost the words it keeps: %q", id, sess.CanonicalTranscript)
		}
	}
	generation, err := store.DeletionGeneration("m2")
	if err != nil {
		t.Fatal(err)
	}
	folders, err := store.DriveReplacementFolders("m2", generation)
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
		t.Errorf("owed Drive folders = %v, want every source with a folder: m1, s1 and s3", owed)
	}
}

// TestSegmentsStayWithTheSourceTheyCameFrom fails if a source's rebuilt
// transcript drops a segment stamped after its session ended, gives a
// segment stamped at a source's very start to the source before it, or if
// the deletion removes a sibling source's live transcript events that share
// the deleted segment's stream times.
func TestSegmentsStayWithTheSourceTheyCameFrom(t *testing.T) {
	store := newTestSQLiteStore(t)
	deleteSecret(t, store, nestedMerge(t, store))

	if got := storeSession(t, store, "s1").CanonicalTranscript; got != "s1 keeps\ns1 late final\n" {
		t.Errorf("s1 canonical transcript = %q, want its kept and its late segment", got)
	}
	if got := storeSession(t, store, "s2").CanonicalTranscript; got != "s2 first\ns2 keeps\n" {
		t.Errorf("s2 canonical transcript = %q, want both its segments", got)
	}
	var s1Events, s3Events int
	if err := store.DB().QueryRow(`SELECT count(*) FILTER (WHERE payload LIKE '%"session_id":"s1"%'), count(*) FILTER (WHERE payload LIKE '%"session_id":"s3"%') FROM mcp_events`).Scan(&s1Events, &s3Events); err != nil {
		t.Fatal(err)
	}
	if s1Events != 0 {
		t.Errorf("the source that held the deleted segment keeps %d live events over its span", s1Events)
	}
	if s3Events != 1 {
		t.Errorf("the sibling source has %d live events, want its one event kept", s3Events)
	}
}

// TestDeleteSegmentKeepsTheFirstOwedTime fails if a second deletion, before
// the first is replaced on Drive, restarts the time the session has owed a
// replacement, which the status warning is measured from.
func TestDeleteSegmentKeepsTheFirstOwedTime(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "owed-twice")
	segments, err := store.GetSegments("owed-twice")
	if err != nil {
		t.Fatal(err)
	}
	span := DeletedSpan{AudioPath: "recording.mp3", Start: 0, End: 2, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteSegment("owed-twice", segments[0].ID, span); err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_since = ? WHERE id = 'owed-twice'`, first); err != nil {
		t.Fatal(err)
	}
	span.Start, span.End = 2, 4
	if err := store.DeleteSegment("owed-twice", segments[1].ID, span); err != nil {
		t.Fatal(err)
	}
	var since string
	if err := store.DB().QueryRow(`SELECT drive_replace_since FROM sessions WHERE id = 'owed-twice'`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	if since != first {
		t.Errorf("owed since %q after a second deletion, want the first deletion's %q", since, first)
	}
}

// TestASyncHoldingTheSessionDuringADeletionOwesItsFolder fails if a first
// sync that uploaded the session while one of its segments was deleted loses
// its folder (the deletion took its claim) or leaves that folder, which holds
// the deleted words, out of the replacement's ledger.
func TestASyncHoldingTheSessionDuringADeletionOwesItsFolder(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "in-flight")
	if _, claimed, err := store.ClaimSync("in-flight", SyncStatePending, "first-sync", time.Now().UTC()); err != nil || !claimed {
		t.Fatalf("ClaimSync = %v, %v", claimed, err)
	}
	segments, err := store.GetSegments("in-flight")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegment("in-flight", segments[1].ID, DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}); err != nil {
		t.Fatal(err)
	}
	ended, err := store.EndSync("in-flight", "first-sync", SyncStatePending, "folder-made-during-the-deletion", 0, time.Now().UTC(), "the session changed during the sync")
	if err != nil || !ended {
		t.Fatalf("EndSync of the sync that held the session = %v, %v; want it recorded", ended, err)
	}
	if got := storeSession(t, store, "in-flight").GDriveFolderID; got != "folder-made-during-the-deletion" {
		t.Errorf("session folder = %q, want the one the sync made", got)
	}
	generation, err := store.DeletionGeneration("in-flight")
	if err != nil {
		t.Fatal(err)
	}
	folders, err := store.DriveReplacementFolders("in-flight", generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].FolderSessionID != "in-flight" || folders[0].State != DriveReplacementFolderOwed {
		t.Errorf("owed folders = %+v, want the folder the sync made", folders)
	}
}

// TestFinishWaitsForEveryOwedFolder fails if the deletion finishes while one
// folder it owes (a source's, in the trash) is still owed, if a ledger from
// an earlier generation finishes a later one, or if a folder is recorded
// replaced while an old file of it is left.
func TestFinishWaitsForEveryOwedFolder(t *testing.T) {
	store := newTestSQLiteStore(t)
	deleteSecret(t, store, nestedMerge(t, store))
	generation, err := store.DeletionGeneration("m2")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartDriveReplacement("s3", "transcript.md", generation, []string{"old-s3"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDriveReplacementFolder("m2", generation, "s3", DriveReplacementFolderReplaced); err == nil {
		t.Error("a folder with an old file left was recorded replaced")
	}
	if _, err := store.DB().Exec(`UPDATE drive_replacements SET state = ? WHERE session_id = 's3'`, DriveReplacementDeleted); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDriveReplacementFolder("m2", generation, "s3", DriveReplacementFolderReplaced); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDriveReplacementFolder("m2", generation, "m1", DriveReplacementFolderGone); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDriveReplacement("m2", generation); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FinishDriveReplacement with s1 still owed = %v, want a refusal naming the owed folder", err)
	}
	if !storeSession(t, store, "m2").DriveReplacePending {
		t.Fatal("the deletion finished while a folder is owed")
	}

	// A later deletion's generation is not finished by this one's ledger.
	if err := store.MarkDriveReplacementFolder("m2", generation, "s1", DriveReplacementFolderGone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET deletion_generation = deletion_generation + 1 WHERE id = 'm2'`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDriveReplacement("m2", generation+1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FinishDriveReplacement of a generation with no ledger = %v, want a refusal", err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET deletion_generation = deletion_generation - 1 WHERE id = 'm2'`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDriveReplacement("m2", generation); err != nil {
		t.Fatalf("FinishDriveReplacement once no folder is owed: %v", err)
	}
}

// TestDeleteSessionRefusesAMergedSessionWhoseSourceFolderIsOwed fails if the
// recursive delete removes a source whose Drive folder a deletion still owes
// a replacement: the replacement could no longer reach that folder.
func TestDeleteSessionRefusesAMergedSessionWhoseSourceFolderIsOwed(t *testing.T) {
	store := newTestSQLiteStore(t)
	deleteSecret(t, store, nestedMerge(t, store))
	if _, err := store.DeleteSession("m2", ""); !errors.Is(err, ErrDeleteReplacementPending) {
		t.Fatalf("DeleteSession of a merged session with an owed source folder = %v, want %v", err, ErrDeleteReplacementPending)
	}
}
