package concierge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAStaleRecordSpeaksItsAgeFirst (R6; run 8j): on a status question whose
// record has no event from the last hour, the answer still comes first from
// the record: its status with its age, then the routing line that says the
// Concierge asks the holder, and the asked line last. The model is not waited
// for.
func TestAStaleRecordSpeaksItsAgeFirst(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", true)
	r.model.onCall = func(int) { time.Sleep(300 * time.Millisecond) }
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "what is happening with ABC-12")
	if len(spoken) < 3 {
		t.Fatalf("spoken = %+v", spoken)
	}
	if want := (said{"As of 2 hours ago, ABC-12 was in progress.", voice.Fact}); spoken[0] != want {
		t.Fatalf("first = %+v, want %+v", spoken[0], want)
	}
	if want := (said{"Asking the agent on ABC-12.", voice.Ack}); spoken[1] != want {
		t.Fatalf("second = %+v, want %+v", spoken[1], want)
	}
	if last := spoken[len(spoken)-1].text; !strings.Contains(last, "asked the agent on ABC-12") {
		t.Fatalf("spoken = %+v, want the asked line last", spoken)
	}
	r.agents.mu.Lock()
	sent := len(r.agents.sent)
	r.agents.mu.Unlock()
	if sent != 1 {
		t.Fatalf("%d questions went to the holder, want 1", sent)
	}

	// Control: a fresh record gets neither line, and no question.
	r = newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken = answerTurn(t, r, "what is happening with ABC-12")
	if slices.ContainsFunc(spoken, func(s said) bool { return strings.HasPrefix(s.text, "As of ") || strings.HasPrefix(s.text, "Asking ") }) {
		t.Fatalf("a fresh record: spoken = %+v", spoken)
	}
}
