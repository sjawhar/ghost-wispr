package voice

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestAWaitingDelegationOutlastsTheFollowUpTurnRule (Main, after Sol's
// nineteenth review of #191; the design note, section 4.3): in the follow-up
// window after an answer, the answer tool waits followUpTurnWait (1.5 s) for
// the turn's final and, with none, takes the turn for side talk and ends the
// exchange. Voice B's delegation waits for the room's own request, which
// holds finals only, and claims it before it calls the tool, so its final has
// always come by then: a final later than the 1.5 s rule is still asked, and
// the exchange stays open. The rule itself is unchanged: the control, a vendor
// that calls the tool on the interim before the final, as voice C may, still
// meets it, and voice A never calls the tool.
func TestAWaitingDelegationOutlastsTheFollowUpTurnRule(t *testing.T) {
	const follow = "when will ABC-12 ship"
	for _, owned := range []bool{true, false} {
		name := "a delegation that waits for its owned final"
		if !owned {
			name = "control: the tool called on the interim, before the final"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge("It ships in May.")
			r := newDrivingRigWith(t, time.Minute, c)
			woke := time.Now().Add(-4 * time.Second)
			r.s.OnTranscript(timedResult("Wispr, what about ABC-42?", 192.93, true, from(woke)))
			sess := r.waitSession()
			// Voice B's first delegation claims the opening before it is
			// answered; a claim nobody made would leave it pending, and the
			// next request would ask it again.
			if open := sess.x.Room(); open.Request == "" || !sess.x.Claim(open.Turn, open.Revision) {
				t.Fatalf("the opening request %+v could not be claimed", open)
			}
			speakAs(t, sess, "ABC-42 is in review.")
			waitSpeechAfter(t, r, time.Time{})
			r.speaker.finish(audio.InputVoice) // the answer plays out: the follow-up window opens
			eventually(t, "the follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
			r.s.OnTranscript(timedResult(follow, 199.0, false, from(time.Now()))) // the follow-up under way
			args := func(q string) json.RawMessage { b, _ := json.Marshal(AnswerArgs{Question: q}); return b }
			done := make(chan error, 1)
			go func() {
				if !owned {
					_, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: args(follow)})
					done <- err
					return
				}
				// Voice B's contract: wait for the room's request, claim it,
				// then call the tool.
				for {
					room := sess.x.Room()
					if !room.Live {
						done <- errors.New("the exchange ended while the delegation waited for its final")
						return
					}
					if room.Request != "" && sess.x.Claim(room.Turn, room.Revision) {
						_, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: args(room.Request)})
						done <- err
						return
					}
					select {
					case <-room.Changed:
					case <-time.After(time.Second):
					}
				}
			}()
			time.Sleep(followUpTurnWait + 200*time.Millisecond)
			r.s.OnTranscript(timedResult(follow+"?", 199.0, true, from(time.Now()))) // its final, late
			select {
			case err := <-done:
				if owned && err != nil {
					t.Fatalf("the waiting delegation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the delegation never finished")
			}
			closed := func() bool { _, _, closed := sess.state(); return closed }
			if !owned {
				eventually(t, "the 1.5 s rule to end the exchange as side talk", closed)
				return
			}
			if closed() {
				t.Fatal("the exchange ended on the 1.5 s rule while the delegation waited for its final")
			}
			c.mu.Lock()
			turns := slices.Clone(c.turns)
			c.mu.Unlock()
			if !slices.Contains(turns, follow+"?") {
				t.Errorf("the Concierge was asked %q, want the late follow-up %q", turns, follow+"?")
			}
		})
	}
}
