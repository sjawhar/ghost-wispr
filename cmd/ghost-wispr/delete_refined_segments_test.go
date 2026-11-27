package main

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestDeleteRefinedTextUsesStoredWordOccurrence(t *testing.T) {
	runParallel(t)
	recording := filepath.Join(t.TempDir(), "recording.wav")
	if output, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", recording).CombinedOutput(); err != nil {
		t.Fatalf("create test recording: %v: %s", err, output)
	}
	hash, err := fileSHA256(recording)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(time.Minute), recording, ""); err != nil {
		t.Fatal(err)
	}
	words := []storage.RefinedWord{
		{RecordingPath: recording, RecordingSHA256: hash, Ordinal: 0, Text: "Again.", Start: 0, End: 0.5},
		{RecordingPath: recording, RecordingSHA256: hash, Ordinal: 1, Text: "Again.", Start: 1, End: 1.5},
	}
	revision, err := store.PublishRefinementAtGeneration("s1", storage.RefinedTranscript(words), "nova-3", words, storage.BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("control: got %d segments, want two repeated occurrences", len(segments))
	}
	summarized := false
	runner := testDeleteRunner(t, &segmentDeleteRunner{store: store, summarize: func(context.Context, string, time.Time) error {
		summarized = true
		return nil
	}})
	if err := runner.DeleteRefinedText(context.Background(), "s1", segments[0].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedText: %v", err)
	}
	if !summarized {
		t.Fatal("DeleteRefinedText did not regenerate the summary")
	}
	if gotHash, err := fileSHA256(recording); err != nil || gotHash != hash {
		t.Fatalf("DeleteRefinedText changed recording hash to %q (%v), want %q", gotHash, err, hash)
	}
	session, err := store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.RefinedWords("s1", session.RefinedRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Ordinal != 1 {
		t.Fatalf("remaining refined words = %#v, want only the second occurrence", remaining)
	}
}

func TestDeleteRefinedAudioSplicesWithFFmpegAndShiftsWords(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.wav")
	if output, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", recording).CombinedOutput(); err != nil {
		t.Fatalf("create test recording: %v: %s", err, output)
	}
	hash, err := fileSHA256(recording)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("s1", started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("s1", started.Add(2*time.Second), recording, ""); err != nil {
		t.Fatal(err)
	}
	words := []storage.RefinedWord{
		{RecordingPath: recording, RecordingSHA256: hash, Ordinal: 0, Text: "Cut.", Start: 0.2, End: 0.5},
		{RecordingPath: recording, RecordingSHA256: hash, Ordinal: 1, Text: "Later.", Start: 1, End: 1.5},
	}
	revision, err := store.PublishRefinementAtGeneration("s1", storage.RefinedTranscript(words), "nova-3", words, storage.BuildRefinedSegments(words), 0)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("s1")
	if err != nil {
		t.Fatal(err)
	}
	summarized := false
	runner := testDeleteRunner(t, &segmentDeleteRunner{
		store: store,
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		duration: audio.Duration,
		splice:   audio.Splice,
		summarize: func(context.Context, string, time.Time) error {
			summarized = true
			return nil
		},
	})
	if err := runner.DeleteRefinedAudio(context.Background(), "s1", segments[0].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedAudio: %v", err)
	}
	if !summarized {
		t.Fatal("DeleteRefinedAudio did not regenerate the summary")
	}
	session, err := store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.RefinedWords("s1", session.RefinedRevision)
	if err != nil {
		t.Fatal(err)
	}
	// The cut runs from 0 to 0.5 + deletionAudioPad, so "Later." moves back by
	// that much.
	cutEnd := 0.5 + deletionAudioPad
	if len(remaining) != 1 || remaining[0].Text != "Later." || math.Abs(remaining[0].Start-(1-cutEnd)) > 1e-9 || math.Abs(remaining[0].End-(1.5-cutEnd)) > 1e-9 {
		t.Fatalf("remaining words = %#v, want the later word shifted back by the padded cut, %.2f s", remaining, cutEnd)
	}
	if duration, err := audio.Duration(context.Background(), recording); err != nil || duration >= 2 {
		t.Fatalf("spliced recording duration = %v, %v; want a shorter recording", duration, err)
	}
}

// TestRefinedSpliceBoundsPadIntoNeighbours fails if the cut stops at the
// neighbouring words' times, where a transcription of the cut recording
// still heard the deleted segment's onset or tail (#65 review, B1), rather
// than reaching deletionAudioPad past the segment's words on each side.
func TestRefinedSpliceBoundsPadIntoNeighbours(t *testing.T) {
	selected := transcribe.Segment{RecordingPath: "recording.mp3", RecordingSHA256: "hash", StartTime: 1, EndTime: 2}
	words := []storage.RefinedWord{
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 0, Start: 0.4, End: 0.9},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 1, Start: 1, End: 2},
		{RecordingPath: "recording.mp3", RecordingSHA256: "hash", Ordinal: 2, Start: 2.1, End: 2.5},
	}
	start, end := refinedSpliceBoundsWithPad(words, &selected, deletionAudioPad)
	if math.Abs(start-(1-deletionAudioPad)) > 1e-9 || math.Abs(end-(2+deletionAudioPad)) > 1e-9 {
		t.Fatalf("splice bounds = %v..%v, want %v past the segment's words on each side", start, end, deletionAudioPad)
	}
	// With no pad, the cut keeps the earlier bounds: the neighbouring words'
	// edges, or 0.25 s past the segment where no neighbour is closer.
	if start, end := refinedSpliceBoundsWithPad(words, &selected, 0); start != 0.9 || end != 2.1 {
		t.Fatalf("unpadded splice bounds = %v..%v, want both neighbouring word boundaries", start, end)
	}
}
