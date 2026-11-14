package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUntimedDeleteDoesNotLeaveFencedRefinementRunning(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "untimed-running.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("a", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("a", transcribeSegment("untimed running text", 0, started)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("a", started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("b", started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("b", started.Add(61*time.Minute), "/audio/b.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, started, started.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	generation, claimed, err := store.ClaimRefinement("merged", "/audio/b.mp3")
	if err != nil || !claimed {
		t.Fatalf("claim refinement = claimed:%t err:%v", claimed, err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || !segments[0].Untimed {
		t.Fatalf("merged untimed segments = %#v", segments)
	}
	if err := store.DeleteRefinedSegmentText("merged", segments[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "/audio/b.mp3", RecordingSHA256: "b", Ordinal: 0, Text: "batch words", Start: 0, End: 1}}
	if _, err := store.PublishRefinementAtGeneration("merged", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), generation); err == nil {
		t.Fatal("fenced refinement published after untimed deletion")
	}
	session, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if session.RefinementStatus == RefinementRunning {
		t.Fatal("untimed deletion left a fenced refinement running")
	}
}
