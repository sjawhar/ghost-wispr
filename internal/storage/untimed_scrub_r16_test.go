package storage

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUntimedDeleteLeavesOtherMergedSourcesAlone(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "untimed-scrub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("n", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("n", transcribeSegment("november private untimed", 0, started)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("n", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("s", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s", transcribeSegment("sierra keeps its words", 0, started.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s", started.Add(61*time.Minute), "/audio/s.mp3"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET refinement_status = 'failed', canonical_transcript = 'sierra keeps its words', audio_path = '', audio_removed_at = '2026-11-13T12:00:00Z', gdrive_folder_id = 'folder-' || id, sync_status = ?, sync_state = ? WHERE id = 's'`, SyncSynced, SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET gdrive_folder_id = 'folder-' || id, sync_status = ?, sync_state = ? WHERE id = 'n'`, SyncSynced, SyncStateSynced); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"n", "s"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, "private untimed") {
			if !segment.Untimed {
				t.Fatal("never-recorded segment is not untimed")
			}
			if err := store.DeleteRefinedSegmentText("merged", segment.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	source, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source.CanonicalTranscript, "sierra keeps") {
		t.Fatalf("untimed deletion scrubbed unrelated source canonical transcript %q", source.CanonicalTranscript)
	}
	owed, err := store.DriveDocumentsOwed("merged")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(owed, "s") {
		t.Fatalf("untimed deletion owes unrelated source Drive documents: %v", owed)
	}
}
