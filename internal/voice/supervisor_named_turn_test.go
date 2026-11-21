package voice

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// drivingTurnAfterYes opens a driving exchange with a bare "Wispr,", lets the
// voice say "Yes?", then hears text after it, as one final whose first word,
// with repeated, is the exchange's own wake, captured before "Yes?", and
// returns the turn the voice's answer tool hands the Concierge.
func drivingTurnAfterYes(t *testing.T, text string, repeated bool) string {
	t.Helper()
	c := newFakeConcierge("An answer.")
	r := newDrivingRigWith(t, time.Minute, c)
	r.s.cfg.ProjectKeys = func() []string { return []string{"INFRA"} }
	r.s.cfg.FollowUpWait = time.Minute
	old := time.Now().Add(-2 * time.Second)
	r.s.OnTranscript(timedResult("Wispr,", 0, false, from(old)))
	sess := r.waitSession()
	speakAs(t, sess, "Yes?")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the driving follow-up window", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.followUpTimer != nil && !r.s.voiceSpeaking
	})
	r.s.mu.Lock()
	next := r.s.ex.turnEnd.Add(100 * time.Millisecond)
	r.s.mu.Unlock()
	if repeated {
		text = "Wispr, " + text
	}
	tr := timedResult(text, 0, true, from(next))
	for i := range tr.Words {
		switch {
		case repeated && i == 0:
			tr.Words[i].At = old
		case repeated:
			tr.Words[i].At = next.Add(time.Duration(i-1) * 200 * time.Millisecond)
		}
		tr.Words[i].Start = tr.Words[i].At.Sub(old).Seconds()
		tr.Words[i].End = tr.Words[i].Start + 0.15
	}
	time.Sleep(time.Until(tr.Words[len(tr.Words)-1].At.Add(150 * time.Millisecond)))
	tr.At = time.Now()
	r.s.OnTranscript(tr)
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "t1", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-c.asked:
		return got
	case <-time.After(testWait):
		t.Fatal("the answer tool did not ask the Concierge")
		return ""
	}
}

// TestARepeatedOldWakeDoesNotNameTheNextTurn fails if a driving voice's next
// turn, said with no wake phrase after the voice spoke, is read as words
// after the name because Deepgram's result repeats the exchange's own wake,
// said before the voice spoke: "In for five thousand dollars?" reached the
// Concierge as "INFRA-5000 dollars?" (Sol's review of #155 at 1ce434e). The
// control has no repeated wake; a fresh "Wispr, in for 10/27?" still reads
// as INFRA-1027.
func TestARepeatedOldWakeDoesNotNameTheNextTurn(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		if got := drivingTurnAfterYes(t, "In for five thousand dollars?", repeated); got != "In for five thousand dollars?" {
			t.Errorf("the next turn, its old wake repeated %v, reached the Concierge as %q, want the amount as said", repeated, got)
		}
	}
	if got := drivingTurnAfterYes(t, "Wispr, in for 10/27?", false); got != "INFRA-1027?" {
		t.Errorf("a fresh call reached the Concierge as %q, want INFRA-1027?", got)
	}
}
