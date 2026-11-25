package voice

import "testing"

// TestANameSaidAfterTheWakeStaysInTheTurn (Sol's review of #192 at
// 4e52421): a question may open with a person's name or address, said right
// after the wake phrase ("Wispr, Miss Burr fixed the realtime voice"). It
// fails if the turn the Concierge is told loses the name.
func TestANameSaidAfterTheWakeStaysInTheTurn(t *testing.T) {
	for _, question := range []string{
		"Miss Burr fixed the realtime voice. What changed?",
		"Mister Lopez fixed the realtime voice. What changed?",
	} {
		t.Run(question, func(t *testing.T) {
			r := newRig(t, newFakeConcierge("fact"))
			r.heard("Wispr, "+question, true, true)
			r.waitSession()
			if got := r.waitTurn(); got != question {
				t.Fatalf("the turn = %q, want %q", got, question)
			}
		})
	}
}
