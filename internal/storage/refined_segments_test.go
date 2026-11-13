package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestPublishRefinementReplacesLiveSegmentsWithTimedWords(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment("s1", transcribe.Segment{Text: "live segment", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}

	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", RecordingOrdinal: 0, Ordinal: 0, Text: "Um", Speaker: 2, Start: 0, End: 0.2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", RecordingOrdinal: 0, Ordinal: 1, Text: "hello.", Speaker: 2, Start: 0.2, End: 0.7},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", RecordingOrdinal: 0, Ordinal: 2, Text: "Goodbye.", Speaker: 3, Start: 1, End: 1.5},
	}
	segments := []RefinedSegment{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", RecordingOrdinal: 0, FirstWordOrdinal: 0, LastWordOrdinal: 1, Speaker: 2, Text: "Um hello.", Start: 0, End: 0.7},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", RecordingOrdinal: 0, FirstWordOrdinal: 2, LastWordOrdinal: 2, Speaker: 3, Text: "Goodbye.", Start: 1, End: 1.5},
	}

	revision, err := store.PublishRefinementAtGeneration("s1", "Um hello. Goodbye.", "nova-3", words, segments, 0)
	if err != nil {
		t.Fatalf("PublishRefinementAtGeneration: %v", err)
	}
	if revision != 1 {
		t.Fatalf("revision = %d, want 1", revision)
	}

	session, err := store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if session.RefinedRevision != revision || session.RefinedTranscript != "Um hello. Goodbye." || session.CanonicalTranscript != "Um hello. Goodbye." || session.TranscriptSource != TranscriptSourceRefined {
		t.Errorf("published session = %#v, want revision %d and refined canonical transcript", session, revision)
	}
	gotSegments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSegments) != 2 || gotSegments[0].Text != "Um hello." || gotSegments[0].TranscriptRevision != revision || gotSegments[0].RecordingPath != "recording.mp3" || gotSegments[0].FirstWordOrdinal != 0 || gotSegments[1].Speaker != 3 {
		t.Fatalf("refined segments = %#v, want two provenance-carrying segments", gotSegments)
	}
	gotWords, err := store.RefinedWords("s1", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotWords) != len(words) || gotWords[0].Text != "Um" || gotWords[0].Speaker != 2 || gotWords[0].RecordingSHA256 != "hash" || gotWords[2].Ordinal != 2 {
		t.Fatalf("refined words = %#v, want timed word provenance", gotWords)
	}
}

func TestDeleteRefinedSegmentTextDeletesOnlyTheSelectedRepeatedWords(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Text: "Repeat", Start: 0, End: 0.2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 1, Text: "again.", Start: 0.2, End: 0.5},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 2, Text: "Repeat", Start: 1, End: 1.2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 3, Text: "stays.", Start: 1.2, End: 1.5},
	}
	segments := BuildRefinedSegments(words)
	revision, err := store.PublishRefinementAtGeneration("s1", RefinedTranscript(words), "nova-3", words, segments, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("control: segments = %#v, want two repeated phrases", before)
	}

	if err := store.DeleteRefinedSegmentText("s1", before[0].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedSegmentText: %v", err)
	}
	after, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Text != "Repeat stays." || after[0].FirstWordOrdinal != 2 {
		t.Fatalf("remaining segments = %#v, want only the selected occurrence removed", after)
	}
	remaining, err := store.RefinedWords("s1", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].Ordinal != 2 || remaining[1].Ordinal != 3 {
		t.Fatalf("remaining words = %#v, want only the selected occurrence removed", remaining)
	}
	session, err := store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if session.RefinementStatus != RefinementCompleted || session.RefinedTranscript != "Repeat stays." || session.CanonicalTranscript != "Repeat stays." || session.TranscriptSource != TranscriptSourceRefined {
		t.Fatalf("session after refined deletion = %#v, want the rebuilt refined transcript", session)
	}
	spans, err := store.TextDeletionSpans("recording.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0] != (TextDeletionSpan{AudioPath: "recording.mp3", Start: 0, End: 0.5, SHA256: "hash"}) {
		t.Fatalf("text spans = %#v, want selected word span", spans)
	}
}

func TestAudioCutShiftsLaterRefinedWords(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "before", Ordinal: 0, Text: "Cut.", Start: 0, End: 0.5},
		{RecordingPath: "recording.mp3", RecordingSHA256: "before", Ordinal: 1, Text: "Later.", Start: 1, End: 1.5},
	}
	revision, err := store.PublishRefinementAtGeneration("s1", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	span := DeletedSpan{SessionID: "s1", AudioPath: "recording.mp3", SessionAudioPath: "recording.mp3", Start: 0, End: 0.5, SHA256Before: "before", SHA256After: "after"}
	if err := store.DeleteRefinedSegmentAudio("s1", segments[0].ID, revision, span); err != nil {
		t.Fatalf("DeleteRefinedSegmentAudio: %v", err)
	}
	if err := store.MarkSpliceComplete(&span); err != nil {
		t.Fatalf("MarkSpliceComplete: %v", err)
	}
	remaining, err := store.RefinedWords("s1", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Text != "Later." || remaining[0].Start != 0.5 || remaining[0].End != 1 || remaining[0].RecordingSHA256 != "after" {
		t.Fatalf("words after splice = %#v, want later word shifted to the new recording", remaining)
	}
	afterSegments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSegments) != 1 || afterSegments[0].StartTime != 0.5 || afterSegments[0].EndTime != 1 {
		t.Fatalf("segments after splice = %#v, want shifted timed segment", afterSegments)
	}
}

func TestDeleteRefinedSegmentRefusesStaleRevision(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(time.Minute), "recording.mp3"); err != nil {
		t.Fatal(err)
	}
	words := []RefinedWord{{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Text: "Keep.", Start: 0, End: 0.5}}
	revision, err := store.PublishRefinementAtGeneration("s1", RefinedTranscript(words), "nova-3", words, BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefinedSegmentText("s1", segments[0].ID, revision+1); !errors.Is(err, ErrStaleTranscriptRevision) {
		t.Fatalf("stale refined delete = %v, want stale transcript revision", err)
	}
	remaining, err := store.RefinedWords("s1", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("stale revision deleted words: %#v", remaining)
	}
}
