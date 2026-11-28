package storage

import (
	"testing"
	"time"
)

// TestReview195UntimedBufferedQuestionIsErased fails if a delete of an untimed
// segment, of a merged source with no recording, keeps a question heard in its
// words when the segment was stored long after them: an untimed segment holds
// when its live result was stored, which waits for the end of the utterance,
// so a long one stores its first words many seconds late. The control is a
// segment stored a second after its words.
func TestReview195UntimedBufferedQuestionIsErased(t *testing.T) {
	for _, delay := range []time.Duration{time.Second, 11 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			store := newTestSQLiteStore(t)
			at := time.Now().UTC().Add(-time.Hour)
			if err := store.CreateSession("source", at); err != nil {
				t.Fatal(err)
			}
			segment := transcribeSegment("Target.", 0, at.Add(delay))
			segment.EndTime = 1
			if err := store.AppendSegment("source", segment); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession("source", at.Add(time.Minute), "", ""); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateSession("other", at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := store.EndSession("other", at.Add(2*time.Minute), "", ""); err != nil {
				t.Fatal(err)
			}
			askVoiceQuestion(t, store, "source", "deleted-words-marker", at.Add(200*time.Millisecond), at.Add(800*time.Millisecond))
			askVoiceQuestion(t, store, "source", "kept-words-marker", at.Add(30*time.Second), at.Add(31*time.Second))
			if got := voiceQuestionHeard(t, store); len(got) != 2 {
				t.Fatalf("control: questions = %q", got)
			}
			if err := store.MergeSessions("merged", []string{"source", "other"}, at, at.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := store.DeleteRefinedSegmentText("merged", segmentID(t, store, "merged", "Target."), 0); err != nil {
				t.Fatal(err)
			}
			if got := voiceQuestionHeard(t, store); len(got) != 1 || got[0] != "heard kept-words-marker" {
				t.Fatalf("questions after the untimed segment's delete = %q, want only the kept one", got)
			}
		})
	}
}
