package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMergeRefinementKeepsAndDeletesAnUntimedSourceSegment(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "merge-untimed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("without-recording", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("without-recording", transcribeSegment("alpha words without a recording", 0, started)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("without-recording", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("with-recording", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("with-recording", started.Add(61*time.Minute), "/audio/with-recording.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"without-recording", "with-recording"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/audio/with-recording.mp3", RecordingSHA256: "recording", Ordinal: 0, Text: "Bravo words.", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	var untimedID int64
	for _, segment := range segments {
		if strings.Contains(segment.Text, "alpha words") {
			untimedID = segment.ID
		}
	}
	if untimedID == 0 {
		t.Fatal("refinement dropped text from the source with no recording")
	}
	if err := store.DeleteRefinedSegmentText("merged", untimedID, 0); err != nil {
		t.Fatalf("delete untimed text: %v", err)
	}
	generation, err := store.DeletionGeneration("merged")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), generation); err != nil {
		t.Fatalf("publish after untimed deletion: %v", err)
	}
	segments, err = store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, "alpha words") {
			t.Fatal("refinement brought back deleted untimed text")
		}
	}
}

func TestMergeFencesSourcesAndKeepsTheirSegmentsHidden(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "merge-fence.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if merged.RefinementStatus != RefinementPending {
		t.Fatalf("merged refinement status = %q, want pending", merged.RefinementStatus)
	}
	generation, err := store.DeletionGeneration("a")
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("merged source deletion generation = %d, want 1", generation)
	}
	if _, claimed, err := store.ClaimRefinement("a", "/audio/a.mp3"); err != nil || claimed {
		t.Fatalf("claim refinement for merged source = claimed:%t err:%v, want refused", claimed, err)
	}
	if err := store.StartSummary("a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("start summary for merged source = %v, want refusal", err)
	}
	if _, err := store.DB().Exec(`INSERT INTO segments(session_id, speaker, text, start_time, end_time, timestamp) VALUES('a', 0, 'stale hidden text', 0, 1, ?)`, started.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("merged source GET segments = %#v, want no stale segments", segments)
	}
}

func TestMergeWithoutRecordingsIsNoAudio(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("source", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("source", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"source"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if merged.RefinementStatus != RefinementNoAudio {
		t.Fatalf("merged refinement status = %q, want no_audio", merged.RefinementStatus)
	}
}

func TestMergeCarriesAudioRemovalFromAGarbageCollectedSource(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("gc-removed", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-removed", started.Add(time.Minute), "/audio/gc-removed.mp3"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = ? WHERE id = 'gc-removed'`, started.Add(time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"gc-removed"}, started, started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if merged.AudioRemovedAt == "" {
		t.Fatal("merge with garbage-collected audio lost the removal marker")
	}
}

func TestLateRefinementPublicationAfterRemergeIsRefused(t *testing.T) {
	store := newTestSQLiteStore(t)
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), "/audio/"+id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("middle", []string{"b", "c"}, started.Add(time.Hour), started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	generation, claimed, err := store.ClaimRefinement("middle", "/audio/b.mp3,/audio/c.mp3")
	if err != nil || !claimed {
		t.Fatalf("claim middle refinement = claimed:%t err:%v", claimed, err)
	}
	if err := store.MergeSessions("top", []string{"a", "middle"}, started, started.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/audio/b.mp3", RecordingSHA256: "b", Ordinal: 0, Text: "late secret", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("middle", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), generation); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("late refinement publication = %v, want refusal", err)
	}
}
