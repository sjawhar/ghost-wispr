package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// recordingTranscriber transcribes each recording path as its entry says,
// and records the paths it was asked for.
type recordingTranscriber struct {
	mu      sync.Mutex
	results map[string]transcribe.BatchResult
	asked   []string
}

func (r *recordingTranscriber) Transcribe(_ context.Context, audioPath string) (transcribe.BatchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, audioPath)
	result, ok := r.results[audioPath]
	if !ok {
		return transcribe.BatchResult{}, fmt.Errorf("read audio file: open %s: no such file", audioPath)
	}
	return result, nil
}

// timedWords returns words for text, each word 0.3 s long with 0.05 s
// between, starting at start.
func timedWords(start float64, text string) []transcribe.TimestampedWord {
	var words []transcribe.TimestampedWord
	for _, field := range strings.Fields(text) {
		words = append(words, transcribe.TimestampedWord{Text: field, Start: start, End: start + 0.3})
		start += 0.35
	}
	return words
}

func refinementResult(model string, parts ...[]transcribe.TimestampedWord) transcribe.BatchResult {
	var words []transcribe.TimestampedWord
	var texts []string
	for _, part := range parts {
		words = append(words, part...)
		for _, word := range part {
			texts = append(texts, word.Text)
		}
	}
	return transcribe.BatchResult{Transcript: strings.Join(texts, " "), Model: model, Words: words}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

// deletedTextSession makes ended session id, recorded in a file holding
// audio, with segments "So we met.", "Orange tractor ninety." and "And then
// left.", deletes the middle one with "Delete text" at its span in the
// recording (1.0 to 1.95 s, where deletedTextWords puts it), and returns the
// store and the recording.
func deletedTextSession(t *testing.T, id, audio string) (*storage.SQLiteStore, string, time.Time) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	recording := filepath.Join(t.TempDir(), id+".mp3")
	if err := os.WriteFile(recording, []byte(audio), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"So we met.", "Orange tractor ninety.", "And then left."} {
		if err := store.AppendSegment(id, transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i) + 0.95, Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSegmentText(id, segments[1].ID, storage.TextDeletionSpan{AudioPath: recording, Start: 1.0, End: 1.95, SHA256: fileHash(t, recording)}); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
	}
	return store, recording, started
}

// deletedTextWords is what batch transcription hears in deletedTextSession's
// recording: the deleted segment's words from 1.0 to 1.95 s.
func deletedTextWords() transcribe.BatchResult {
	return refinementResult("nova-3", timedWords(0, "So we met."), timedWords(1.0, "Orange tractor ninety."), timedWords(2.1, "And then left."))
}

// TestRefinementDropsTheWordsOfDeletedText fails if a refinement of a
// session after "Delete text" brings the deleted words back from the
// unchanged recording, or drops a neighbour's.
func TestRefinementDropsTheWordsOfDeletedText(t *testing.T) {
	store, recording, started := deletedTextSession(t, "20261112100000", "audio")
	transcriber := &recordingTranscriber{results: map[string]transcribe.BatchResult{recording: deletedTextWords()}}
	if !strings.Contains(transcriber.results[recording].Transcript, "tractor") {
		t.Fatal("control: the recording's transcription does not hold the deleted words")
	}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	if err := manager.RefineSession(context.Background(), "20261112100000", recording, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}
	sess, err := store.GetSession("20261112100000")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus != storage.RefinementCompleted || sess.RefinedTranscript != "So we met. And then left." || sess.RefinementModel != "nova-3" {
		t.Errorf("refinement = %q %q (model %q), want completed \"So we met. And then left.\" from nova-3", sess.RefinementStatus, sess.RefinedTranscript, sess.RefinementModel)
	}
	if strings.Contains(sess.CanonicalTranscript, "tractor") || !strings.Contains(sess.CanonicalTranscript, "And then left.") {
		t.Errorf("canonical transcript = %q, want the refined transcript without the deleted words", sess.CanonicalTranscript)
	}
	segments, err := store.GetSegments("20261112100000")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].TranscriptRevision != 1 || segments[0].RecordingPath != recording || segments[0].FirstWordOrdinal != 0 || segments[1].FirstWordOrdinal != 6 {
		t.Fatalf("rebuilt refined segments = %#v, want timed segments with the deleted words absent", segments)
	}
	words, err := store.RefinedWords("20261112100000", sess.RefinedRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 6 || strings.Contains(strings.Join([]string{words[0].Text, words[1].Text, words[2].Text, words[3].Text, words[4].Text, words[5].Text}, " "), "tractor") {
		t.Fatalf("persisted refined words = %#v, want only the six retained words", words)
	}
}

// TestRefinementRefusesARecordingChangedSinceATextDeletion fails if a
// refinement reads a recording whose bytes are not the ones a "Delete text"
// span was recorded in (the span's times may not match its words any more)
// and stores a transcript: it must fail rather than risk the words.
func TestRefinementRefusesARecordingChangedSinceATextDeletion(t *testing.T) {
	store, recording, started := deletedTextSession(t, "20261112110000", "audio")
	if err := os.WriteFile(recording, []byte("other audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	transcriber := &recordingTranscriber{results: map[string]transcribe.BatchResult{recording: deletedTextWords()}}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	if err := manager.RefineSession(context.Background(), "20261112110000", recording, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}
	sess, err := store.GetSession("20261112110000")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus != storage.RefinementFailed || sess.RefinedTranscript != "" {
		t.Errorf("refinement of a changed recording = %q %q, want failed and empty", sess.RefinementStatus, sess.RefinedTranscript)
	}
	if strings.Contains(sess.CanonicalTranscript, "tractor") {
		t.Errorf("canonical transcript = %q holds the deleted words", sess.CanonicalTranscript)
	}
}

// wordlessTranscriber returns a transcript and no word timestamps.
type wordlessTranscriber struct{ transcript string }

func (w wordlessTranscriber) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	return transcribe.BatchResult{Transcript: w.transcript, Model: "nova-3"}, nil
}

// TestRefinementWithoutWordTimesFailsAfterATextDeletion fails if a
// refinement whose transcriber gives no word times stores its transcript
// for a recording with a "Delete text" span: it cannot tell which words to
// drop.
func TestRefinementWithoutWordTimesFailsAfterATextDeletion(t *testing.T) {
	store, recording, started := deletedTextSession(t, "20261112120000", "audio")
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(wordlessTranscriber{transcript: "So we met. Orange tractor ninety. And then left."})
	if err := manager.RefineSession(context.Background(), "20261112120000", recording, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}
	sess, err := store.GetSession("20261112120000")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus != storage.RefinementFailed || strings.Contains(sess.RefinedTranscript+sess.CanonicalTranscript, "tractor") {
		t.Errorf("refinement %q, refined %q, canonical %q: want failed and no deleted words", sess.RefinementStatus, sess.RefinedTranscript, sess.CanonicalTranscript)
	}
}

// TestRefinementOfAMergedSessionDropsDeletedTextInItsRecordings fails if a
// merged session's refinement does not transcribe each of its sources'
// recordings, or brings back the words "Delete text" removed from one.
func TestRefinementOfAMergedSessionDropsDeletedTextInItsRecordings(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dir := t.TempDir()
	startA := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	startB := startA.Add(time.Hour)
	recordings := map[string]string{"a": filepath.Join(dir, "a.mp3"), "b": filepath.Join(dir, "b.mp3")}
	texts := map[string][]string{"a": {"Alpha stays here."}, "b": {"Bravo stays.", "Orange tractor ninety.", "Charlie stays."}}
	for id, started := range map[string]time.Time{"a": startA, "b": startB} {
		if err := os.WriteFile(recordings[id], []byte("audio of "+id), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		for i, text := range texts[id] {
			if err := store.AppendSegment(id, transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i) + 0.95, Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.EndSession(id, started.Add(time.Minute), recordings[id]); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"a", "b"}, startA, startB.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	merged, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	var orange int64
	for _, segment := range segments {
		if strings.Contains(segment.Text, "tractor") {
			orange = segment.ID
		}
	}
	if err := store.DeleteSegmentText("merged", orange, storage.TextDeletionSpan{AudioPath: recordings["b"], Start: 1.0, End: 1.95, SHA256: fileHash(t, recordings["b"])}); err != nil {
		t.Fatalf("DeleteSegmentText: %v", err)
	}
	transcriber := &recordingTranscriber{results: map[string]transcribe.BatchResult{
		recordings["a"]: refinementResult("nova-3", timedWords(0, "Alpha stays here.")),
		recordings["b"]: refinementResult("nova-3", timedWords(0, "Bravo stays."), timedWords(1.0, "Orange tractor ninety."), timedWords(2.1, "Charlie stays.")),
	}}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	if err := manager.RefineSession(context.Background(), "merged", merged.AudioPath, startA); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}
	if len(transcriber.asked) != 2 || transcriber.asked[0] != recordings["a"] || transcriber.asked[1] != recordings["b"] {
		t.Errorf("transcribed %q, want each recording alone, in session order: %q then %q", transcriber.asked, recordings["a"], recordings["b"])
	}
	sess, err := store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Alpha stays here.\n\nBravo stays. Charlie stays."; sess.RefinementStatus != storage.RefinementCompleted || sess.RefinedTranscript != want {
		t.Errorf("merged refinement = %q %q, want completed %q", sess.RefinementStatus, sess.RefinedTranscript, want)
	}
	refinedSegments, err := store.GetSegments("merged")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range refinedSegments {
		if strings.Contains(segment.Text, "Alpha") && segment.RecordingPath != recordings["a"] {
			t.Fatalf("alpha provenance = %q, want recording %q", segment.RecordingPath, recordings["a"])
		}
		if strings.Contains(segment.Text, "Charlie") && segment.RecordingPath != recordings["b"] {
			t.Fatalf("charlie provenance = %q, want recording %q", segment.RecordingPath, recordings["b"])
		}
	}
}
