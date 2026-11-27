package concierge

import (
	"strings"
	"testing"
	"time"
)

// Sol's thirteenth review of #191 at 0f22df5
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6067724093),
// from Sol's probes, through the real Supervisor and Concierge.

// recentWords is the room's recent words in the model's last prompt.
func recentWords(t *testing.T, r *cutRig) string {
	t.Helper()
	recent, _, found := strings.Cut(lastPrompt(r.rig), "The question to answer, said after the wake word: ")
	if !found {
		t.Fatal("control: the prompt holds no question")
	}
	return recent
}

// TestAQuotationClosedLaterKeepsItsHistory (Sol's thirteenth review of #191,
// P1): the room's recent words are cut at a take-back once the words the
// answer cuts are complete, never on each final. At 0f22df5, after Wispr's
// question, `does ABC-12 show "Please` / "cancel" / `then retry" in the
// tooltip?` lost its first two finals from the recent words: "cancel" was
// heard while the quotation was open, the history was cut then, and the
// closing mark did not bring them back. Controls: no command inside, one
// final, and a real take-back after the closed quotation, whose words stay
// out of a later exchange too.
func TestAQuotationClosedLaterKeepsItsHistory(t *testing.T) {
	first := `does ABC-12 show "Please`
	for _, tc := range []struct {
		name   string
		finals []string
		want   string
		cut    bool // the first final leaves the history
	}{
		{"cancel inside", []string{first, "cancel", `then retry" in the tooltip?`}, "", false},
		{"never mind inside", []string{first, "never mind", `then retry" in the tooltip?`}, "", false},
		{"control: no command inside", []string{first, "retry", `then retry" in the tooltip?`}, "", false},
		{"control: a take-back after the quotation", []string{first, "cancel", `then retry" in the tooltip?`, "never mind", "what about ABC-12?"}, "what about ABC-12?", true},
	} {
		for _, endpoints := range []bool{false, true} {
			t.Run(endpointNames(tc.name, endpoints), func(t *testing.T) {
				want := tc.want
				if want == "" {
					want = strings.Join(tc.finals, " ")
				}
				r := askedBackRig(t)
				r.hearReply(tc.finals, endpoints)
				if out := r.answer(t, "reply", tc.finals[len(tc.finals)-1]); out.Say == "" {
					t.Fatal("the reply was not answered")
				}
				wantAsked(t, r, want)
				if kept := strings.Contains(recentWords(t, r), first); kept == tc.cut {
					t.Errorf("the answer's recent words hold %q: %v, want %v", first, kept, !tc.cut)
				}
				r.sup.Stop("a new exchange") // keeps the room's recent words
				r.hearNow("Wispr, what is the status of ABC-12?", true)
				select {
				case r.sess = <-r.driver.opened:
				case <-time.After(r7Deadline):
					t.Fatal("control: no new exchange opened")
				}
				if out := r.answer(t, "later", "what is the status of ABC-12?"); out.Say == "" {
					t.Fatal("control: the later question was not answered")
				}
				if kept := strings.Contains(recentWords(t, r), first); kept == tc.cut {
					t.Errorf("a later exchange's recent words hold %q: %v, want %v", first, kept, !tc.cut)
				}
			})
		}
	}
}

// TestAnOpenerThatEndsAFinalOpens (Sol's thirteenth review of #191, P2): a
// mark that opens or closes is told by what comes before it. At 0f22df5 an
// opener that ended a final, `does ABC-12 show "` before "Please" /
// "cancel" / `then retry" in the tooltip?`, was no opener, as a line or a
// space followed it, and the model was asked `then retry" in the tooltip?`.
func TestAnOpenerThatEndsAFinalOpens(t *testing.T) {
	for _, pair := range [][2]string{{`"`, `"`}, {"“", "”"}, {"「", "」"}, {"«", "»"}} {
		for _, endpoints := range []bool{false, true} {
			t.Run(endpointNames(pair[0]+pair[1], endpoints), func(t *testing.T) {
				finals := []string{"does ABC-12 show " + pair[0], "Please", "cancel", "then retry" + pair[1] + " in the tooltip?"}
				r := askedBackRig(t)
				r.hearReply(finals, endpoints)
				if out := r.answer(t, "reply", finals[len(finals)-1]); out.Say == "" {
					t.Fatal("the reply was not answered")
				}
				wantAsked(t, r, strings.Join(finals, " "))
			})
		}
	}
}

// TestUnlistedQuoteMarksAreEachTheirOwnKind (Sol's thirteenth review of
// #191, P3): a quotation mark of no listed kind closes only a quotation it
// opened itself. At 0f22df5 every initial mark and every final one shared a
// kind, so ⸂ and ⸅ made a quotation of `⸂Please, cancel, what about ABC-12⸅`
// and the model was asked the cancelled ABC-42 question. Control: a listed
// pair, 「 and 」.
func TestUnlistedQuoteMarksAreEachTheirOwnKind(t *testing.T) {
	for _, tc := range []struct{ name, words, want string }{
		{"⸂ then ⸅", "does ABC-42 show ⸂Please, cancel, what about ABC-12⸅ in the tooltip?", "what about ABC-12⸅ in the tooltip?"},
		{"control: 「 then 」", "does ABC-12 show 「Please, cancel, then retry」 in the tooltip?", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.words
			}
			r := newCutRigOpening(t, []string{"Wispr, " + tc.words}, false, answers(2)...)
			if out := r.answer(t, "quoted", tc.words); out.Say == "" {
				t.Fatal("the question was not answered")
			}
			wantAsked(t, r, want)
		})
	}
}
