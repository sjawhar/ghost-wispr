package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sol's review of #195 at 48ca91a1 left these probes; each failed there.

// TestReview195DeleteAfterEarlierSplice fails if a segment delete, after an
// earlier "Delete text and audio" cut ten seconds from the start of the same
// recording, places the segment by its spliced time rather than the time its
// words were said: the question heard in it stays, and its words stay in the
// database file. The control, with no earlier cut, erases it.
func TestReview195DeleteAfterEarlierSplice(t *testing.T) {
	for _, spliced := range []bool{false, true} {
		name := "original recording control"
		if spliced {
			name = "recording shortened by ten seconds"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "questions.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			at := time.Now().UTC().Add(-time.Hour)
			if err := store.CreateSession("s", at); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession("s", at.Add(time.Minute), "recording.mp3", ""); err != nil {
				t.Fatal(err)
			}
			words := []RefinedWord{
				{RecordingPath: "recording.mp3", RecordingSHA256: "before", Ordinal: 0, Text: "Cut.", Start: 0, End: 10},
				{RecordingPath: "recording.mp3", RecordingSHA256: "before", Ordinal: 1, Text: "Target.", Start: 20, End: 21},
				{RecordingPath: "recording.mp3", RecordingSHA256: "before", Ordinal: 2, Text: "Kept.", Start: 50, End: 51},
			}
			revision, err := store.PublishRefinementAtGeneration("s", RefinedTranscript(words), "test", words, BuildRefinedSegments(words), 0)
			if err != nil {
				t.Fatal(err)
			}
			askVoiceQuestion(t, store, "s", "deleted-words-marker", at.Add(20200*time.Millisecond), at.Add(20700*time.Millisecond))
			askVoiceQuestion(t, store, "s", "kept-words-marker", at.Add(50200*time.Millisecond), at.Add(50700*time.Millisecond))
			if got := voiceQuestionHeard(t, store); len(got) != 2 {
				t.Fatalf("control: questions before the deletes = %q", got)
			}
			if spliced {
				span := DeletedSpan{SessionID: "s", AudioPath: "recording.mp3", Start: 0, End: 10, SHA256Before: "before", SHA256After: "after"}
				if err := store.DeleteRefinedSegmentAudio("s", segmentID(t, store, "s", "Cut."), revision, &span); err != nil {
					t.Fatal(err)
				}
				if err := store.MarkSpliceComplete(&span); err != nil {
					t.Fatal(err)
				}
			}
			session, err := store.GetSession("s")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DeleteRefinedSegmentText("s", segmentID(t, store, "s", "Target."), session.RefinedRevision); err != nil {
				t.Fatal(err)
			}
			if got := voiceQuestionHeard(t, store); len(got) != 1 || got[0] != "heard kept-words-marker" {
				t.Errorf("questions after Target's delete = %q, want only the kept one", got)
			}
			for _, suffix := range []string{"", "-wal"} {
				contents, err := os.ReadFile(path + suffix)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if bytes.Contains(contents, []byte("deleted-words-marker")) {
					t.Errorf("the deleted words remain in the database file%s", suffix)
				}
			}
		})
	}
}

// TestReview195DelayedInsertCannotRestoreDeletedSegment fails if a question
// whose write lands after a delete of the segment its words were heard in is
// stored: the question log writes apart from the answer, so the write can come
// late, and it must not bring back words a person deleted. The control: the
// same question asked outside the deleted segment is stored.
func TestReview195DelayedInsertCannotRestoreDeletedSegment(t *testing.T) {
	store := newTestSQLiteStore(t)
	segmentTestSessionWith(t, store, "s", func(time.Time) {})
	session, err := store.GetSession("s")
	if err != nil {
		t.Fatal(err)
	}
	late := func(words string, from time.Duration) VoiceQuestion {
		at := session.StartedAt.Add(from)
		return VoiceQuestion{
			SessionID: "s", AskedAt: at.Add(500 * time.Millisecond), HeardFrom: at, HeardTo: at.Add(time.Second),
			Heard: "late " + words, VendorWording: "vendor late " + words, Answer: "answer late " + words, Outcome: VoiceQuestionStopped,
		}
	}
	if err := deleteRefinedTextTestSegment(t, store, "s", segmentID(t, store, "s", "private deletion token"), TextDeletionSpan{AudioPath: "recording.mp3", Start: 2, End: 4, SHA256: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := voiceQuestionHeard(t, store); len(got) != 0 {
		t.Fatalf("control: questions after the delete = %q, want none", got)
	}
	inside := late("deleted-words-marker", 2500*time.Millisecond)
	err = store.RecordVoiceQuestion(&inside)
	for _, heard := range voiceQuestionHeard(t, store) {
		if strings.Contains(heard, "deleted-words-marker") {
			t.Fatalf("a late write brought back the deleted segment's words (write error %v): %q", err, heard)
		}
	}
	outside := late("kept-words-marker", 10*time.Second)
	if err := store.RecordVoiceQuestion(&outside); err != nil {
		t.Fatalf("control: a question outside the deleted segment: %v", err)
	}
	if got := voiceQuestionHeard(t, store); len(got) != 1 || got[0] != "late kept-words-marker" {
		t.Errorf("control: questions = %q, want the one outside the deleted segment", got)
	}
}
