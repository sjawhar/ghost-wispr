package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// refusingSplice fails the test: "Delete text" never splices.
func refusingSplice(t *testing.T) func(context.Context, string, float64, float64) (string, error) {
	return func(context.Context, string, float64, float64) (string, error) {
		t.Error("Delete text spliced the recording")
		return "", errors.New("Delete text must not splice")
	}
}

func countRows(t *testing.T, store *storage.SQLiteStore, table string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// TestDeleteTextLeavesTheRecordingAndRecordsTheAlignedSpan fails if "Delete
// text" changes the recording's bytes, splices, owes Drive a replacement, or
// records anything but the segment's aligned word span in the recording.
func TestDeleteTextLeavesTheRecordingAndRecordsTheAlignedSpan(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, started := newDeletionTestStore(t, recording)
	before, err := fileSHA256(recording)
	if err != nil {
		t.Fatal(err)
	}
	var refinedPath string
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: refusingSplice(t),
		refine: func(_ context.Context, sessionID, audioPath string, gotStarted time.Time) error {
			if sessionID != "session" || !gotStarted.Equal(started) {
				t.Errorf("refine called with session %q started %s", sessionID, gotStarted)
			}
			refinedPath = audioPath
			return nil
		},
		summarize: func(context.Context, string, time.Time) error {
			t.Error("Delete text of a session with its audio summarized without refining")
			return nil
		},
	}
	if err := runner.DeleteText(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("DeleteText: %v", err)
	}

	if after, err := fileSHA256(recording); err != nil || after != before {
		t.Errorf("recording SHA-256 after Delete text = %s (%v), want unchanged %s", after, err, before)
	}
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].Text != "before bridge" || segments[1].Text != "after bridge" {
		t.Errorf("segments after Delete text = %+v, want the two neighbours", segments)
	}
	spans, err := store.TextDeletionSpans(recording)
	if err != nil {
		t.Fatal(err)
	}
	// The aligned words "private deletion token" run from 1.1 to 1.6 s.
	if want := (storage.TextDeletionSpan{AudioPath: "recording.mp3", Start: 1.1, End: 1.6, SHA256: before}); len(spans) != 1 || spans[0] != want {
		t.Errorf("recorded spans = %+v, want [%+v]", spans, want)
	}
	sess, err := store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	if sess.DriveReplacePending || sess.AudioPath != recording {
		t.Errorf("drive_replace_pending %v, audio path %q; want false and unchanged %q", sess.DriveReplacePending, sess.AudioPath, recording)
	}
	for table, want := range map[string]int{"drive_replacements": 0, "drive_replacement_folders": 0, "deleted_segments": 0} {
		if got := countRows(t, store, table); got != want {
			t.Errorf("%s has %d rows after Delete text, want %d", table, got, want)
		}
	}
	if refinedPath != recording {
		t.Errorf("refinement read %q, want the unchanged recording %q", refinedPath, recording)
	}
}

// TestDeleteTextRefusesASegmentItCannotLocate fails if "Delete text" changes
// anything when the segment's words cannot be found in the recording:
// without a span, a later refinement would bring them back.
func TestDeleteTextRefusesASegmentItCannotLocate(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: cutWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: refusingSplice(t),
	}
	if err := runner.DeleteText(context.Background(), "session", segmentID); !errors.Is(err, server.ErrSegmentNotFoundInRecording) {
		t.Fatalf("DeleteText error = %v, want %v", err, server.ErrSegmentNotFoundInRecording)
	}
	if segments, err := store.GetSegments("session"); err != nil || len(segments) != 3 {
		t.Errorf("segments after a refused Delete text = %d (%v), want 3", len(segments), err)
	}
	if got := countRows(t, store, "deleted_text_spans"); got != 0 {
		t.Errorf("a refused Delete text recorded %d spans", got)
	}
}

// TestDeleteTextWithoutLocalAudioSummarizesAndRemovesTheDownload fails if
// "Delete text" of a session whose audio was removed to free space keeps the
// recording it downloaded from Drive to find the words, names it as the
// session's audio, or leaves the summary unmade.
func TestDeleteTextWithoutLocalAudioSummarizesAndRemovesTheDownload(t *testing.T) {
	downloaded := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(downloaded, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, started := newDeletionTestStore(t, downloaded)
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = '2026-11-11T13:00:00Z', refinement_status = ? WHERE id = 'session'`, storage.RefinementCompleted); err != nil {
		t.Fatal(err)
	}
	summarized := false
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{downloaded}, Downloaded: []string{downloaded}}, nil
		},
		splice: refusingSplice(t),
		refine: func(context.Context, string, string, time.Time) error {
			t.Error("Delete text refined a session with no audio")
			return nil
		},
		summarize: func(_ context.Context, sessionID string, gotStarted time.Time) error {
			summarized = sessionID == "session" && gotStarted.Equal(started)
			return nil
		},
	}
	if err := runner.DeleteText(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("DeleteText: %v", err)
	}
	if !summarized {
		t.Error("Delete text of a session without audio did not make its summary again")
	}
	if _, err := os.Stat(downloaded); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the recording downloaded to find the words is still there (stat: %v)", err)
	}
	sess, err := store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	if sess.AudioPath != "" || sess.RefinementStatus != storage.RefinementNoAudio {
		t.Errorf("audio path %q, refinement %q; want none and %q", sess.AudioPath, sess.RefinementStatus, storage.RefinementNoAudio)
	}
}

// refinementWordsFake is a batch transcriber whose recording holds every
// word of initialWords, the deleted ones included.
type refinementWordsFake struct{}

func (refinementWordsFake) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	var words []transcribe.TimestampedWord
	var texts []string
	for _, word := range initialWords() {
		words = append(words, transcribe.TimestampedWord(word))
		texts = append(texts, word.Text)
	}
	return transcribe.BatchResult{Transcript: strings.Join(texts, " "), Model: "nova-3", Words: words}, nil
}

// TestARetriedRefinementDoesNotBringDeletedTextBack fails if a refinement
// after "Delete text", the deletion's own or a "retry refinement", stores
// the deleted words again from the unchanged recording.
func TestARetriedRefinementDoesNotBringDeletedTextBack(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, started := newDeletionTestStore(t, recording)
	manager := session.NewManager(store, nil, nil, nil, session.NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(refinementWordsFake{})
	refined := func() string {
		t.Helper()
		sess, err := store.GetSession("session")
		if err != nil {
			t.Fatal(err)
		}
		return sess.RefinedTranscript + "\n" + sess.CanonicalTranscript
	}
	// Control: a refinement of the recording does bring the words.
	if err := manager.RefineSession(context.Background(), "session", recording, started); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refined(), "private deletion token") {
		t.Fatalf("control: refinement before the deletion = %q, want the words", refined())
	}

	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice:    refusingSplice(t),
		refine:    manager.RefineSession,
		summarize: manager.SummarizeSession,
	}
	if err := runner.DeleteText(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("DeleteText: %v", err)
	}
	if got := refined(); strings.Contains(got, "private") || !strings.Contains(got, "before bridge after bridge") {
		t.Errorf("after Delete text, refined and canonical = %q, want the neighbours without the deleted words", got)
	}
	if err := retryRefinementWithWarningClear(context.Background(), newWarningsRegistry(), store, "session", manager.RefineSession); err != nil {
		t.Fatalf("retry refinement: %v", err)
	}
	sess, err := store.GetSession("session")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefinementStatus != storage.RefinementCompleted || strings.Contains(refined(), "private") {
		t.Errorf("after retry refinement: status %q, refined and canonical %q; want completed without the deleted words", sess.RefinementStatus, refined())
	}
}

// fillerSession makes ended session "session" whose recording holds, in
// order, "before bridge", a seven-word filler segment, "private deletion
// token" and "after bridge", and returns the store and the segments' ids.
func fillerSession(t *testing.T, recording string) (*storage.SQLiteStore, []int64) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("session", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"before bridge", "um so like you know basically right", "private deletion token", "after bridge"} {
		if err := store.AppendSegment("session", transcribe.Segment{Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("session", started.Add(4*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(segments))
	for i := range segments {
		ids[i] = segments[i].ID
	}
	return store, ids
}

func fillerWords() []audio.Word {
	return []audio.Word{
		{Text: "before", Start: 0.1, End: 0.2}, {Text: "bridge", Start: 0.3, End: 0.4},
		{Text: "um", Start: 0.5, End: 0.55}, {Text: "so", Start: 0.6, End: 0.65}, {Text: "like", Start: 0.7, End: 0.75},
		{Text: "you", Start: 0.8, End: 0.85}, {Text: "know", Start: 0.9, End: 0.95}, {Text: "basically", Start: 1.0, End: 1.05},
		{Text: "right", Start: 1.1, End: 1.15},
		{Text: "private", Start: 1.5, End: 1.6}, {Text: "deletion", Start: 1.7, End: 1.8}, {Text: "token", Start: 1.9, End: 2.0},
		{Text: "after", Start: 2.5, End: 2.6}, {Text: "bridge", Start: 2.7, End: 2.8},
	}
}

// TestDeletingTextAndAudioNextToDeletedTextIsNotRefused fails if "Delete
// text and audio" of a segment refuses its cut as an over-cut because the
// words just before it in the recording are a neighbour's that "Delete text"
// already removed: they are no neighbour's words any more.
func TestDeletingTextAndAudioNextToDeletedTextIsNotRefused(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, ids := fillerSession(t, recording)
	// The cut takes 1.25 to 2.25 s; what follows moves back a second.
	var spliced []audio.Word
	for _, word := range fillerWords() {
		switch {
		case word.End <= 1.25:
			spliced = append(spliced, word)
		case word.Start >= 2.25:
			spliced = append(spliced, audio.Word{Text: word.Text, Start: word.Start - 1, End: word.End - 1})
		}
	}
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: fillerWords(), after: spliced},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: fakeSplice,
	}
	if err := runner.DeleteText(context.Background(), "session", ids[1]); err != nil {
		t.Fatalf("DeleteText of the filler: %v", err)
	}
	if err := runner.Delete(context.Background(), "session", ids[2]); err != nil {
		t.Fatalf("Delete text and audio next to the deleted filler: %v", err)
	}
	segments, err := store.GetSegments("session")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].Text != "before bridge" || segments[1].Text != "after bridge" {
		t.Errorf("segments = %+v, want the two kept", segments)
	}
}

// TestDeleteTextWithPartOfTheAudioRemovedSummarizesWithoutRefining fails if
// "Delete text" of a session whose audio was partly removed to free space
// refines what is left: that would drop the words of the removed recordings
// from its transcript.
func TestDeleteTextWithPartOfTheAudioRemovedSummarizesWithoutRefining(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, segmentID, _ := newDeletionTestStore(t, recording)
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_removed_at = '2026-11-11T13:00:00Z' WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}
	summarized := false
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: initialWords()},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: refusingSplice(t),
		refine: func(context.Context, string, string, time.Time) error {
			t.Error("Delete text refined a session with part of its audio removed")
			return nil
		},
		summarize: func(context.Context, string, time.Time) error {
			summarized = true
			return nil
		},
	}
	if err := runner.DeleteText(context.Background(), "session", segmentID); err != nil {
		t.Fatalf("DeleteText: %v", err)
	}
	if !summarized {
		t.Error("Delete text of a session with part of its audio removed did not make its summary again")
	}
}

// TestDeleteTextRefusesASpanThatLeavesWordsBeforeTheRecordingEdge fails if
// the live segment's "is gonna" aligns to the batch words "is going to" but
// LocateWords stops before "to": a later refinement would keep "to".
func TestDeleteTextRefusesASpanThatLeavesWordsBeforeTheRecordingEdge(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	if err := store.CreateSession("edge", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"We met at the office.", "Orange tractor ninety is gonna"} {
		if err := store.AppendSegment("edge", transcribe.Segment{
			Text: text, StartTime: float64(i * 2), EndTime: float64(i*2 + 2), Timestamp: started.Add(time.Duration(i*2) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("edge", started.Add(4*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("edge")
	if err != nil {
		t.Fatal(err)
	}
	words := []audio.Word{
		{Text: "We", Start: 0, End: 0.2},
		{Text: "met", Start: 0.25, End: 0.45},
		{Text: "at", Start: 0.5, End: 0.65},
		{Text: "the", Start: 0.7, End: 0.85},
		{Text: "office.", Start: 0.9, End: 1.15},
		{Text: "Orange", Start: 2, End: 2.25},
		{Text: "tractor", Start: 2.3, End: 2.55},
		{Text: "ninety", Start: 2.6, End: 2.85},
		{Text: "is", Start: 2.9, End: 3.05},
		{Text: "going", Start: 3.6, End: 3.9},
		{Text: "to", Start: 3.95, End: 4.1},
	}
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: words},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: refusingSplice(t),
	}
	if err := runner.DeleteText(context.Background(), "edge", segments[1].ID); !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("DeleteText error = %v, want %v", err, server.ErrSegmentNotRemovedCleanly)
	}
	if got, err := store.GetSegments("edge"); err != nil || len(got) != 2 {
		t.Errorf("segments after refused Delete text = %+v (%v), want both", got, err)
	}
	if spans, err := store.TextDeletionSpans(recording); err != nil || len(spans) != 0 {
		t.Errorf("spans after refused Delete text = %+v (%v), want none", spans, err)
	}
}

// TestDeleteTextUsesNeighbourContextForRepeatedFiller fails if a repeated
// filler word is refused even though its surrounding segments identify one
// occurrence: filler words are the primary Delete text use case.
func TestDeleteTextUsesNeighbourContextForRepeatedFiller(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	if err := store.CreateSession("repeat", started); err != nil {
		t.Fatal(err)
	}
	texts := []string{"We should start", "um", "the unique middle context", "um", "then the unique ending"}
	for i, text := range texts {
		if err := store.AppendSegment("repeat", transcribe.Segment{
			Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("repeat", started.Add(5*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("repeat")
	if err != nil {
		t.Fatal(err)
	}
	words := []audio.Word{
		{Text: "We", Start: 0, End: 0.2}, {Text: "should", Start: 0.25, End: 0.45}, {Text: "start", Start: 0.5, End: 0.7},
		{Text: "um", Start: 1, End: 1.15},
		{Text: "the", Start: 2, End: 2.15}, {Text: "unique", Start: 2.2, End: 2.4}, {Text: "middle", Start: 2.45, End: 2.65}, {Text: "context", Start: 2.7, End: 2.9},
		{Text: "um", Start: 3, End: 3.15},
		{Text: "then", Start: 4, End: 4.15}, {Text: "the", Start: 4.2, End: 4.35}, {Text: "unique", Start: 4.4, End: 4.6}, {Text: "ending", Start: 4.65, End: 4.85},
	}
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: words},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: refusingSplice(t),
	}
	if err := runner.DeleteText(context.Background(), "repeat", segments[3].ID); err != nil {
		t.Fatalf("DeleteText of the context-identified filler: %v", err)
	}
	spans, err := store.TextDeletionSpans(recording)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].Start < 2.9 || spans[0].End > 3.2 {
		t.Errorf("span for the second filler = %+v, want the occurrence at 3.0-3.15", spans)
	}
}

func TestDeleteTextRefusesWrongRepeatedContentCopy(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC)
	if err := store.CreateSession("wrong-content", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"So anyway", "Purple elephant seventeen.", "The budget is fine."} {
		if err := store.AppendSegment("wrong-content", transcribe.Segment{
			Text: text, StartTime: float64(i * 2), EndTime: float64(i*2 + 2), Timestamp: started.Add(time.Duration(i*2) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("wrong-content", started.Add(6*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("wrong-content")
	if err != nil {
		t.Fatal(err)
	}
	words := []audio.Word{
		{Text: "So", Start: 0, End: 0.2}, {Text: "anyway", Start: 0.3, End: 0.6},
		{Text: "purple", Start: 0.8, End: 1.0}, {Text: "elegant", Start: 1.05, End: 1.3}, {Text: "seventeen.", Start: 1.35, End: 1.6},
		{Text: "The", Start: 2, End: 2.2}, {Text: "budget", Start: 2.25, End: 2.5}, {Text: "is", Start: 2.55, End: 2.7}, {Text: "fine.", Start: 2.75, End: 3.0},
		// This exact decoy is more than ten seconds from the real words, so
		// Survivors' own window cannot catch a wrong pick.
		{Text: "We", Start: 14, End: 14.2}, {Text: "said", Start: 14.25, End: 14.45}, {Text: "anyway", Start: 14.5, End: 14.8},
		{Text: "purple", Start: 15.8, End: 16.0}, {Text: "elephant", Start: 16.05, End: 16.3}, {Text: "seventeen", Start: 16.35, End: 16.6},
		{Text: "the", Start: 16.65, End: 16.8}, {Text: "budget", Start: 16.85, End: 17.1}, {Text: "is", Start: 17.15, End: 17.3}, {Text: "fine", Start: 17.35, End: 17.55}, {Text: "again.", Start: 17.6, End: 17.85},
	}
	assertDeleteTextRefusedWithoutSpan(t, store, "wrong-content", segments[1].ID, recording, words)
}

func TestDeleteTextRefusesWrongRepeatedFillerCopy(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)
	if err := store.CreateSession("wrong-filler", started); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"We begin", "Um.", "The budget is fine."} {
		if err := store.AppendSegment("wrong-filler", transcribe.Segment{
			Text: text, StartTime: float64(i * 2), EndTime: float64(i*2 + 2), Timestamp: started.Add(time.Duration(i*2) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("wrong-filler", started.Add(6*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	segments, err := store.GetSegments("wrong-filler")
	if err != nil {
		t.Fatal(err)
	}
	words := []audio.Word{
		{Text: "We", Start: 0, End: 0.2}, {Text: "begin", Start: 0.25, End: 0.5},
		{Text: "uh", Start: 0.8, End: 1.0},
		{Text: "The", Start: 2, End: 2.2}, {Text: "budget", Start: 2.25, End: 2.5}, {Text: "is", Start: 2.55, End: 2.7}, {Text: "fine.", Start: 2.75, End: 3.0},
		{Text: "We", Start: 8, End: 8.2}, {Text: "said", Start: 8.25, End: 8.5}, {Text: "um", Start: 9.8, End: 10.0},
		{Text: "the", Start: 10.1, End: 10.25}, {Text: "budget", Start: 10.3, End: 10.55}, {Text: "is", Start: 10.6, End: 10.75}, {Text: "fine", Start: 10.8, End: 11.0},
	}
	assertDeleteTextRefusedWithoutSpan(t, store, "wrong-filler", segments[1].ID, recording, words)
}

func assertDeleteTextRefusedWithoutSpan(t *testing.T, store *storage.SQLiteStore, sessionID string, segmentID int64, recording string, words []audio.Word) {
	t.Helper()
	runner := segmentDeleteRunner{
		store:       store,
		transcriber: deletionWordsFake{before: words},
		ensureRecordings: func(context.Context, storage.Session) (deletionRecordingSet, error) {
			return deletionRecordingSet{Paths: []string{recording}}, nil
		},
		splice: refusingSplice(t),
	}
	if err := runner.DeleteText(context.Background(), sessionID, segmentID); !errors.Is(err, server.ErrSegmentNotRemovedCleanly) {
		t.Fatalf("DeleteText error = %v, want %v", err, server.ErrSegmentNotRemovedCleanly)
	}
	if spans, err := store.TextDeletionSpans(recording); err != nil || len(spans) != 0 {
		t.Errorf("spans after refused Delete text = %+v (%v), want none", spans, err)
	}
}

func TestDeleteTextRefusesWrongRepeatedFillerRun(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	if err := store.CreateSession("filler-run", started); err != nil {
		t.Fatal(err)
	}
	segments := []string{"Right.", "Yeah.", "Um.", "Yeah.", "So.", "Right.", "Yeah.", "Um.", "Yeah.", "So."}
	for i, text := range segments {
		if err := store.AppendSegment("filler-run", transcribe.Segment{
			Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("filler-run", started.Add(10*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetSegments("filler-run")
	if err != nil {
		t.Fatal(err)
	}
	words := []audio.Word{
		{Text: "Right.", Start: 0, End: 0.2}, {Text: "Yeah.", Start: 0.3, End: 0.5}, {Text: "uh", Start: 0.6, End: 0.8}, {Text: "Yeah.", Start: 0.9, End: 1.1}, {Text: "So.", Start: 1.2, End: 1.4},
		{Text: "Right.", Start: 14, End: 14.2}, {Text: "Yeah.", Start: 14.3, End: 14.5}, {Text: "um", Start: 14.8, End: 15.1}, {Text: "Yeah.", Start: 15.2, End: 15.4}, {Text: "So.", Start: 15.5, End: 15.7},
	}
	assertDeleteTextRefusedWithoutSpan(t, store, "filler-run", stored[2].ID, recording, words)
}

func TestDeleteTextRefusesWrongRepeatedContentRun(t *testing.T) {
	recording := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(recording, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 19, 0, 0, 0, time.UTC)
	if err := store.CreateSession("content-run", started); err != nil {
		t.Fatal(err)
	}
	segments := []string{"Okay.", "Yeah.", "Purple elephant seventeen.", "The budget is fine.", "Great.", "Okay.", "Yeah.", "Purple elephant seventeen.", "The budget is fine.", "Great."}
	for i, text := range segments {
		if err := store.AppendSegment("content-run", transcribe.Segment{
			Text: text, StartTime: float64(i), EndTime: float64(i + 1), Timestamp: started.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("content-run", started.Add(10*time.Second), recording); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetSegments("content-run")
	if err != nil {
		t.Fatal(err)
	}
	words := []audio.Word{
		{Text: "Okay.", Start: 0, End: 0.2}, {Text: "Yeah.", Start: 0.3, End: 0.5}, {Text: "Purple", Start: 0.6, End: 0.8}, {Text: "elegant", Start: 0.85, End: 1.1}, {Text: "seventeen.", Start: 1.15, End: 1.4}, {Text: "The", Start: 1.5, End: 1.7}, {Text: "budget", Start: 1.75, End: 2}, {Text: "is", Start: 2.05, End: 2.2}, {Text: "fine.", Start: 2.25, End: 2.5}, {Text: "Great.", Start: 2.6, End: 2.8},
		{Text: "Okay.", Start: 14, End: 14.2}, {Text: "Yeah.", Start: 14.3, End: 14.5}, {Text: "Purple", Start: 14.6, End: 14.8}, {Text: "elephant", Start: 14.85, End: 15.1}, {Text: "seventeen.", Start: 15.15, End: 15.4}, {Text: "The", Start: 15.5, End: 15.7}, {Text: "budget", Start: 15.75, End: 16}, {Text: "is", Start: 16.05, End: 16.2}, {Text: "fine.", Start: 16.25, End: 16.5}, {Text: "Great.", Start: 16.6, End: 16.8},
	}
	assertDeleteTextRefusedWithoutSpan(t, store, "content-run", stored[2].ID, recording, words)
}
