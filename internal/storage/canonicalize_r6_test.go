package storage

import (
	"strings"
	"testing"
)

func TestR6CanonicalizePairsItsTranscriptWithItsGeneration(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "s")
	revision, segs := publishRefinedSegmentTestSession(t, store, "s", "recording.mp3", "b")
	span := DeletedSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a"}
	if err := store.DeleteRefinedSegmentAudio("s", segs[1].ID, revision, &span); err != nil {
		t.Fatal(err)
	}
	transcript, generation, err := store.Canonicalize("s")
	if err != nil {
		t.Fatal(err)
	}
	storedTranscript, _, readGeneration, err := store.GetCanonicalTranscriptWithGeneration("s")
	if err != nil {
		t.Fatal(err)
	}
	if transcript != storedTranscript || generation != readGeneration || strings.Contains(transcript, "private deletion token") {
		t.Fatalf("canonicalize = (%q, generation %d); stored = (%q, generation %d); want deleted text absent at the written generation", transcript, generation, storedTranscript, readGeneration)
	}
	if err := store.StartSummary("s"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSummaryAtGeneration("s", "Safe title", transcript, SummaryCompleted, "default", "{}", "", generation); err != nil {
		t.Fatalf("store summary at canonical generation: %v", err)
	}
	sess, _ := store.GetSession("s")
	if strings.Contains(sess.Title, "private deletion token") {
		t.Fatal("canonicalize paired old deleted text with its current generation")
	}
}
