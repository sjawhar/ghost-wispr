package storage

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// TestSolFollowup195CapturedBeforeRecording fails if a delete of an untimed
// segment keeps a question whose words were captured before its source
// session's recording began, or lets the same question be written again: the
// transcriber replays audio older than the recording after a long outage
// (transcribe.ResilientClient.MaxBehind), and an untimed segment keeps no
// capture time, so its erasure has no lower bound. The control's question was
// heard after the session began. (Sol's review of #195 at 451690d6.)
func TestSolFollowup195CapturedBeforeRecording(t *testing.T) {
	for _, before := range []bool{false, true} {
		name := "heard within the session control"
		if before {
			name = "replayed words captured before the recording"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestSQLiteStore(t)
			at := time.Now().UTC().Add(-time.Hour)
			if err := s.CreateSession("source", at); err != nil {
				t.Fatal(err)
			}
			target := transcribeSegment("Target delayed utterance.", 0, at.Add(30*time.Second))
			target.StartTime, target.EndTime = 0, 2
			if err := s.AppendSegment("source", target); err != nil {
				t.Fatal(err)
			}
			if err := s.EndSession("source", at.Add(time.Minute), "", ""); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSession("other", at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.EndSession("other", at.Add(2*time.Minute), "", ""); err != nil {
				t.Fatal(err)
			}
			from, to := at.Add(500*time.Millisecond), at.Add(time.Second)
			if before {
				from, to = at.Add(-2*time.Second), at.Add(-1500*time.Millisecond)
			}
			q := VoiceQuestion{SessionID: "source", AskedAt: at.Add(30 * time.Second), HeardFrom: from, HeardTo: to, Heard: "deleted-words-marker", Answer: "deleted answer words", Outcome: VoiceQuestionAnswered}
			if err := s.RecordVoiceQuestion(&q); err != nil {
				t.Fatal(err)
			}
			askVoiceQuestion(t, s, "source", "kept-words-marker", at.Add(40*time.Second), at.Add(41*time.Second))
			if got := voiceQuestionHeard(t, s); len(got) != 2 {
				t.Fatalf("control: questions = %q", got)
			}
			if err := s.MergeSessions("merged", []string{"source", "other"}, at, at.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteRefinedSegmentText("merged", segmentID(t, s, "merged", "Target delayed utterance."), 0); err != nil {
				t.Fatal(err)
			}
			if got := voiceQuestionHeard(t, s); !slices.Equal(got, []string{"heard kept-words-marker"}) {
				t.Errorf("questions after the untimed segment's delete = %q, want only the kept one", got)
			}
			if err := s.RecordVoiceQuestion(&q); !errors.Is(err, ErrVoiceQuestionErased) {
				t.Errorf("writing the deleted question again: %v, want ErrVoiceQuestionErased", err)
			}
		})
	}
}
