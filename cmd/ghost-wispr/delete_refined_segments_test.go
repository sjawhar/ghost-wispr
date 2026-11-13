package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestDeleteRefinedTextUsesStoredWordOccurrence(t *testing.T) {
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
	if err := store.EndSession("s1", started.Add(time.Minute), recording); err != nil {
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
	runner := segmentDeleteRunner{store: store, summarize: func(context.Context, string, time.Time) error {
		summarized = true
		return nil
	}}
	if err := runner.DeleteRefinedText(context.Background(), "s1", segments[0].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedText: %v", err)
	}
	if !summarized {
		t.Fatal("DeleteRefinedText did not regenerate the summary")
	}
	if gotHash, err := fileSHA256(recording); err != nil || gotHash != hash {
		t.Fatalf("DeleteRefinedText changed recording hash to %q (%v), want %q", gotHash, err, hash)
	}
	remaining, err := store.RefinedWords("s1", revision)
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
	if err := store.EndSession("s1", started.Add(2*time.Second), recording); err != nil {
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
	runner := segmentDeleteRunner{
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
	}
	if err := runner.DeleteRefinedAudio(context.Background(), "s1", segments[0].ID, revision); err != nil {
		t.Fatalf("DeleteRefinedAudio: %v", err)
	}
	if !summarized {
		t.Fatal("DeleteRefinedAudio did not regenerate the summary")
	}
	remaining, err := store.RefinedWords("s1", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Text != "Later." || remaining[0].Start != 0.25 || remaining[0].End != 0.75 {
		t.Fatalf("remaining words = %#v, want later word shifted by the ffmpeg cut", remaining)
	}
	if duration, err := audio.Duration(context.Background(), recording); err != nil || duration >= 2 {
		t.Fatalf("spliced recording duration = %v, %v; want a shorter recording", duration, err)
	}
}
