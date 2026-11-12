package gdrive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const textSecret = "bravo text secret"

// recordingHash is the SHA-256 of the recording at path.
func recordingHash(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

// folderFiles returns the id and content of each file in folder, by name,
// failing the test if a name has another number of copies than one.
func folderFiles(t *testing.T, wire *wireDrive, folder string) map[string]wireFile {
	t.Helper()
	files := map[string]wireFile{}
	for name, copies := range wire.in(folder) {
		if len(copies) != 1 {
			t.Fatalf("folder %s holds %d copies of %s, want 1", folder, len(copies), name)
		}
		files[name] = *copies[0]
	}
	return files
}

// deleteTextFrom deletes, with "Delete text", the segment of session id
// holding textSecret, which recording holds from 1 to 2 s, and stands in for
// the refinement and summary that follow: refinement done, and the summary
// made again as summary ("" for one that came back empty).
func deleteTextFrom(t *testing.T, store *storage.SQLiteStore, id, recording, summary string) {
	t.Helper()
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	var secretID int64
	for _, segment := range segments {
		if strings.Contains(segment.Text, textSecret) {
			secretID = segment.ID
		}
	}
	if secretID == 0 {
		t.Fatalf("control: session %s has no segment holding the secret", id)
	}
	if err := store.DeleteSegmentText(id, secretID, storage.TextDeletionSpan{AudioPath: recording, Start: 1, End: 2, SHA256: recordingHash(t, recording)}); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
	}
	status := storage.SummaryCompleted
	if summary == "" {
		status = storage.SummaryFailed
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = ?, summary_status = ?, summary = ? WHERE id = ?`,
		storage.RefinementCompleted, status, summary, id); err != nil {
		t.Fatal(err)
	}
}

// TestDeleteTextUpdatesTheDriveDocumentsInPlace fails if the sync after
// "Delete text" replaces or deletes a Drive file, leaves the words in the
// current transcript or summary, or touches the recording: the three file
// ids stay, and the documents are updated in place.
func TestDeleteTextUpdatesTheDriveDocumentsInPlace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary string
	}{
		{name: "summary made again", summary: "The standup summary, made again."},
		{name: "summary came back empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t)
			const id = "20261112090000"
			recording, _ := round2Session(t, store, id, []string{"alpha stays.", textSecret + ".", "charlie stays."})
			if err := store.UpdateSummary(id, "Standup", "The summary names the "+textSecret+".", storage.SummaryCompleted, "default", "{}"); err != nil {
				t.Fatal(err)
			}
			wire, syncer := newWireDrive(t)
			o := NewOrchestrator(syncer, store)
			folder := syncedSession(t, store, o, id)
			before := folderFiles(t, wire, folder)
			for _, name := range []string{"transcript.md", "summary.md"} {
				if !strings.Contains(string(before[name].Content), textSecret) {
					t.Fatalf("control: Drive's %s does not hold the words before the deletion: %q", name, before[name].Content)
				}
			}
			recordingName := "audio-" + id + ".mp3"
			if _, ok := before[recordingName]; !ok || len(before) != 3 {
				t.Fatalf("control: the folder holds %d files, want transcript, summary and %s", len(before), recordingName)
			}
			recordingBefore := recordingHash(t, recording)

			deleteTextFrom(t, store, id, recording, tc.summary)
			if err := o.SyncSession(context.Background(), id); err != nil {
				t.Fatalf("sync after Delete text: %v", err)
			}

			after := folderFiles(t, wire, folder)
			if len(after) != 3 {
				t.Errorf("the folder holds %d files after Delete text, want the same 3", len(after))
			}
			for name, file := range before {
				if after[name].ID != file.ID {
					t.Errorf("%s has id %s after Delete text, want the same id %s", name, after[name].ID, file.ID)
				}
			}
			for _, name := range []string{"transcript.md", "summary.md"} {
				if strings.Contains(string(after[name].Content), textSecret) {
					t.Errorf("Drive's current %s still holds the deleted words: %q", name, after[name].Content)
				}
			}
			if !strings.Contains(string(after["transcript.md"].Content), "charlie stays.") {
				t.Errorf("Drive's transcript lost the words it keeps: %q", after["transcript.md"].Content)
			}
			if string(after[recordingName].Content) != string(before[recordingName].Content) || after[recordingName].RevisionCount != before[recordingName].RevisionCount {
				t.Error("the sync after Delete text changed the recording on Drive")
			}
			if got := recordingHash(t, recording); got != recordingBefore {
				t.Errorf("the local recording's SHA-256 is %s after Delete text, want %s", got, recordingBefore)
			}
			if state, _ := sessionState(t, store, id); state != storage.SyncStateSynced {
				t.Errorf("sync state %q after the sync, want synced", state)
			}
			if wire.deletes != 0 || len(wire.deletedIDs) != 0 {
				t.Errorf("the sync after Delete text deleted Drive files: %v", wire.deletedIDs)
			}
		})
	}
}

// TestDeleteTextUpdatesMergedSourceFoldersInPlace fails if "Delete text" in
// a merged session leaves the words in the current documents of a source
// session's own Drive folder, which the merge left on Drive, or replaces
// any file there.
func TestDeleteTextUpdatesMergedSourceFoldersInPlace(t *testing.T) {
	store := openStore(t)
	recA, _ := round2Session(t, store, "20261112080000", []string{"alpha stays.", textSecret + "."})
	if err := store.UpdateSummary("20261112080000", "Standup", "The summary names the "+textSecret+".", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatal(err)
	}
	_, _ = round2Session(t, store, "20261112083000", []string{"charlie stays."})
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folderA := syncedSession(t, store, o, "20261112080000")
	_ = syncedSession(t, store, o, "20261112083000")
	const merged = "20261112080000-merged"
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	if err := store.MergeSessions(merged, []string{"20261112080000", "20261112083000"}, started, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize(merged); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummary(merged, "Merged", "merged summary", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatal(err)
	}
	mergedFolder := syncedSession(t, store, o, merged)
	sourceBefore := folderFiles(t, wire, folderA)
	if !strings.Contains(string(sourceBefore["transcript.md"].Content), textSecret) || !strings.Contains(string(sourceBefore["summary.md"].Content), textSecret) {
		t.Fatal("control: the source folder's documents do not hold the words before the deletion")
	}

	deleteTextFrom(t, store, merged, recA, "merged summary, made again")
	if err := o.SyncSession(context.Background(), merged); err != nil {
		t.Fatalf("sync after Delete text: %v", err)
	}

	for folder, before := range map[string]map[string]wireFile{folderA: sourceBefore} {
		after := folderFiles(t, wire, folder)
		for name, file := range before {
			if after[name].ID != file.ID {
				t.Errorf("source folder %s: %s has id %s, want the same id %s", folder, name, after[name].ID, file.ID)
			}
			if strings.Contains(string(after[name].Content), textSecret) {
				t.Errorf("source folder %s: the current %s still holds the deleted words: %q", folder, name, after[name].Content)
			}
		}
		if !strings.Contains(string(after["transcript.md"].Content), "alpha stays.") {
			t.Errorf("source folder %s: the transcript lost the words it keeps: %q", folder, after["transcript.md"].Content)
		}
	}
	for name, file := range folderFiles(t, wire, mergedFolder) {
		if name != "transcript.md" && name != "summary.md" {
			continue
		}
		if strings.Contains(string(file.Content), textSecret) {
			t.Errorf("merged folder: the current %s still holds the deleted words", name)
		}
	}
	if len(wire.deletedIDs) != 0 {
		t.Errorf("Delete text deleted Drive files: %v", wire.deletedIDs)
	}
	// Refreshed once, the source owes nothing: the sweep leaves it alone.
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "20261112080000" {
			t.Errorf("the refreshed source is still listed for the sweep: %v", ids)
		}
	}
}

// TestASourceFolderTheSyncCouldNotReachIsRefreshedByTheSweep fails if a
// merged source's Drive folder that was in the trash when the merged
// session synced after "Delete text" is forgotten: once restored, the
// sweep must update its documents in place.
func TestASourceFolderTheSyncCouldNotReachIsRefreshedByTheSweep(t *testing.T) {
	store := openStore(t)
	recA, _ := round2Session(t, store, "20261112070000", []string{"alpha stays.", textSecret + "."})
	_, _ = round2Session(t, store, "20261112073000", []string{"charlie stays."})
	wire, syncer := newWireDrive(t)
	o := NewOrchestrator(syncer, store)
	folderA := syncedSession(t, store, o, "20261112070000")
	_ = syncedSession(t, store, o, "20261112073000")
	const merged = "20261112070000-merged"
	started := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	if err := store.MergeSessions(merged, []string{"20261112070000", "20261112073000"}, started, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Canonicalize(merged); err != nil {
		t.Fatal(err)
	}
	_ = syncedSession(t, store, o, merged)

	wire.trash(folderA)
	deleteTextFrom(t, store, merged, recA, "merged summary")
	_ = o.SyncSession(context.Background(), merged)
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, id := range ids {
		listed = listed || id == "20261112070000"
	}
	if !listed {
		t.Fatalf("a source whose folder the sync could not reach is not listed for the sweep: %v", ids)
	}

	wire.mu.Lock()
	delete(wire.trashed, folderA)
	wire.mu.Unlock()
	sweep(t, store, o)
	if got := string(folderFiles(t, wire, folderA)["transcript.md"].Content); strings.Contains(got, textSecret) {
		t.Errorf("after the folder came back and the sweep ran, its transcript still holds the words: %q", got)
	}
}
