package concierge

import (
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestEachTurnNamesHowItEnded fails if an answer leaves on its exchange
// (voice.Exchange.Outcome, which the question log keeps) anything but how
// the turn ended as the Concierge saw it: answered from the record; asked
// back ("Did you mean …?", "What is its key?"); a reply that picked an
// offered issue, by its place or by its key said alone; a "no" to the
// offer; or the question sent to the issue's agent.
func TestEachTurnNamesHowItEnded(t *testing.T) {
	type turn struct {
		said  string
		model string // the model's facts for the turn; "" when it is not called
		want  voice.QuestionOutcome
	}
	const asked = "what is happening with the realtime voice"
	for _, tc := range []struct {
		name  string
		setup func(r *rig)
		turns []turn
	}{
		{"answered", func(r *rig) { heldIssue(r, "none", false) }, []turn{
			{"what is happening with ABC-12", "ABC-12 is in progress.", voice.OutcomeAnswered},
		}},
		{"asked an agent", func(r *rig) { heldIssue(r, "session", false) }, []turn{
			{"ask the agent on ABC-12 whether the tap is merged", "ABC-12 is in progress.", voice.OutcomeAskedAgent},
		}},
		{"a pick by its place", unclearSearch, []turn{
			{asked, "", voice.OutcomeAskedBack},
			{"the first one", "PR 80 is in review.", voice.OutcomePicked},
		}},
		{"a no to the offer", unclearSearch, []turn{
			{asked, "", voice.OutcomeAskedBack},
			{"no", "", voice.OutcomeNeverMind},
		}},
		{"a key said alone", func(r *rig) {
			heldIssue(r, "session", false)
			r.records.hits = nil
		}, []turn{
			{"what is happening with the purple submarine", "", voice.OutcomeAskedBack},
			{"ABC-12", "ABC-12 is in progress.", voice.OutcomePicked},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(r)
			for _, step := range tc.turns {
				r.model.mu.Lock()
				r.model.turns = nil
				if step.model != "" {
					r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(step.model))}}
				}
				r.model.mu.Unlock()
				r.exchange.Outcome = ""
				spoken := answerTurn(t, r, step.said)
				if r.exchange.Outcome != step.want {
					t.Fatalf("%q: outcome %q, spoken %+v; want %q", step.said, r.exchange.Outcome, spoken, step.want)
				}
			}
		})
	}
}
