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
