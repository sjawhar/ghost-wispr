package concierge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// Sol's eleventh review of #191 at 83aaf8b
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6066661854),
// from Sol's probes, through the real Supervisor and Concierge.

// question is what the model was asked in its last prompt.
func question(r *cutRig) string {
	_, q, _ := strings.Cut(lastPrompt(r.rig), "The question to answer, said after the wake word: ")
	q, _, _ = strings.Cut(q, "\n")
	return q
}

// holderMessages is every message sent to a holder so far.
func holderMessages(r *cutRig) []string {
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	out := make([]string, 0, len(r.agents.sent))
	for _, m := range r.agents.sent {
		out = append(out, m.Message)
	}
	return out
}

// TestTheCurrentTurnsFinalsCutAVendorsParaphrase (Sol's eleventh review of
// #191, P2 1): the current turn's own finals decide the cut, however the
// vendor words the question. At 83aaf8b they were read only when the
// vendor's words were the turn's own, so "what is the status of ABC-12?" for
// "Wispr, what about ABC-42? never mind" / "what about ABC-12?" gave the
// model the cancelled ABC-42. Control: the vendor's words the turn's own.
func TestTheCurrentTurnsFinalsCutAVendorsParaphrase(t *testing.T) {
	for _, vendor := range []string{
		"what about ABC-42? never mind what is the status of ABC-12?",
		"what about ABC-42? never mind what about ABC-12?",
	} {
		t.Run(vendor, func(t *testing.T) {
			r := newCutRigOpening(t, []string{"Wispr, what about ABC-42? never mind", "what about ABC-12?"}, false, answers(2)...)
			if out := r.answer(t, "current", vendor); out.Say == "" {
				t.Fatal("control: the question was not answered")
			}
			if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
				t.Errorf("the model was given the cancelled words:\n%s", prompt)
			}
			for _, m := range holderMessages(r) {
				if strings.Contains(m, "ABC-42") {
					t.Errorf("the holder was sent the cancelled words: %q", m)
				}
			}
		})
	}
}

// TestAReplysTakeBackLeavesTheHistoryAcrossAnEndpoint (Sol's eleventh review
// of #191, P2 2): the room's recent words lose what the answer cuts, the
// whole reply to Wispr's question across the endpoints inside it. At 83aaf8b
// the history was cut by the turn alone, which an endpoint restarts: after
// "what about ABC-42?", an endpoint and "never mind. what about ABC-12?", a
// later exchange's model read "what about ABC-42?". Control: no endpoint.
func TestAReplysTakeBackLeavesTheHistoryAcrossAnEndpoint(t *testing.T) {
	for _, endpoint := range []bool{true, false} {
		name := "control: no endpoint"
		if endpoint {
			name = "an endpoint inside the reply"
		}
		t.Run(name, func(t *testing.T) {
			r := askedBackRig(t)
			r.hearNow("what about ABC-42?", endpoint)
			r.hearNow("never mind. what about ABC-12?", true)
			if out := r.answer(t, "current", "what about ABC-12?"); out.Say == "" {
				t.Fatal("control: the question after the take-back was not answered")
			}
			if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
				t.Errorf("the answer's model was given the cancelled words:\n%s", prompt)
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
			if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
				t.Errorf("a later exchange's model was given the cancelled words:\n%s", prompt)
			}
		})
	}
}

// TestAQuotedPassageHoldsNoTakeBack (Sol's eleventh review of #191, P2 3): a
// quotation is found over the whole of the room's words before take-backs
// are, across commas and finals, and no clause of it is one. At 83aaf8b
// `does ABC-12 show "Please, cancel, then retry" in the tooltip?` sent the
// model and the holder `then retry" in the tooltip?`, and the same passage
// split across finals after Wispr's question was side talk. Controls: a
// question split across finals, a take-back across finals, and a take-back
// after a quotation.
func TestAQuotedPassageHoldsNoTakeBack(t *testing.T) {
	for _, tc := range []struct{ name, words, want string }{
		{"straight quotes", `does ABC-12 show "Please, cancel, then retry" in the tooltip?`, `does ABC-12 show "Please, cancel, then retry" in the tooltip?`},
		{"curly quotes", "does ABC-12 show “Please, cancel, then retry” in the tooltip?", "does ABC-12 show “Please, cancel, then retry” in the tooltip?"},
		{"single quotes", "does ABC-12 show 'Please, cancel, then retry' in the tooltip?", "does ABC-12 show 'Please, cancel, then retry' in the tooltip?"},
		{"guillemets", "does ABC-12 show «Please, cancel, then retry» in the tooltip?", "does ABC-12 show «Please, cancel, then retry» in the tooltip?"},
		{"control: a take-back after a quotation", `did ABC-42 show "Please, cancel, then retry"? never mind. what about ABC-12?`, "what about ABC-12?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRigOpening(t, []string{"Wispr, " + tc.words}, false, answers(2)...)
			out := r.answer(t, "quoted", tc.words)
			if out.Say == "" {
				t.Fatal("the question was not answered")
			}
			if got := question(r); got != tc.want {
				t.Errorf("the model was asked %q, want %q", got, tc.want)
			}
			if !slices.ContainsFunc(holderMessages(r), func(m string) bool { return strings.Contains(strings.ToLower(m), strings.ToLower(tc.want)) }) {
				t.Errorf("the holder was sent %q, want %q", holderMessages(r), tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		finals []string
		want   string
	}{
		{"a quotation across finals", []string{`does ABC-12 show "Please`, "cancel", `then retry" in the tooltip`}, `does ABC-12 show "Please cancel then retry" in the tooltip`},
		{"a curly quotation across finals", []string{"does ABC-12 show “Please", "cancel", "then retry” in the tooltip"}, "does ABC-12 show “Please cancel then retry” in the tooltip"},
		{"control: a question across finals", []string{"does ABC-12 show", "the retry option", "in the tooltip"}, "does ABC-12 show the retry option in the tooltip"},
		{"control: a take-back across finals", []string{"what about ABC-42?", "never mind", "what about ABC-12?"}, "what about ABC-12?"},
	} {
		for _, endpoints := range []bool{false, true} {
			name := tc.name
			if endpoints {
				name += ", endpoints between"
			}
			t.Run(name, func(t *testing.T) {
				r := newCutRig(t,
					[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress. Do you mean its status or its holder?"))},
					[]*genai.GenerateContentResponse{chunk(text("ABC-12 is in progress."))},
				)
				if initial := r.answer(t, "initial", ""); !strings.HasSuffix(initial.Say, "?") {
					t.Fatalf("control: the first answer asks nothing: %q", initial.Say)
				}
				p0 := r.speak(t).Add(20 * time.Millisecond)
				for i, final := range tc.finals {
					at := make([]time.Time, len(strings.Fields(final)))
					for j := range at {
						at[j] = p0.Add(time.Duration(i)*600*time.Millisecond + time.Duration(j)*50*time.Millisecond)
					}
					r.hearEnding(final, true, endpoints || i == len(tc.finals)-1, at)
				}
				if out := r.answer(t, "reply", strings.Join(tc.finals, " ")); out.Say == "" {
					t.Fatal("the reply was not answered")
				}
				if got := question(r); got != tc.want {
					t.Errorf("the model was asked %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// TestEveryQuoteMarkIsAQuote (Sol's eleventh review of #191, P2 4): a quote
// mark is any of Unicode's, not a list. At 83aaf8b `did ABC-12, ‹Cancel›,
// ship?` sent the model `ship?`, and so did 「」, 『』, ＂＂, ＇＇ and 〝〞.
// Controls: straight quotes, and the apostrophe inside a word, in "did ABC-12
// ship? that's all", which takes back what came before it.
func TestEveryQuoteMarkIsAQuote(t *testing.T) {
	for _, pair := range [][2]string{{"‹", "›"}, {"「", "」"}, {"『", "』"}, {"＂", "＂"}, {"＇", "＇"}, {"〝", "〞"}, {`"`, `"`}} {
		t.Run(pair[0]+pair[1], func(t *testing.T) {
			words := "did ABC-12, " + pair[0] + "Cancel" + pair[1] + ", ship?"
			r := newCutRigOpening(t, []string{"Wispr, " + words}, false, answers(2)...)
			if out := r.answer(t, "quoted", words); out.Say == "" {
				t.Fatal("the question was not answered")
			}
			if got := question(r); got != words {
				t.Errorf("the model was asked %q, want %q", got, words)
			}
		})
	}
	t.Run("control: an apostrophe inside a word", func(t *testing.T) {
		for _, words := range []string{"did ABC-12 ship? that's all", "did ABC-12 ship? that’s all"} {
			if _, taken := TakeBack(words); !taken {
				t.Errorf("%q takes nothing back, want everything", words)
			}
		}
	})
}

// TestWisprNeverMindLeavesTheHistory (Sol's eleventh review of #191, a
// diagnostic): "Wispr, never mind" cancels the question asked, as a
// take-back does, and its words leave the room's recent words. At 83aaf8b a
// fresh exchange's model read "Wispr, what about ABC-42?". Controls: with no
// cancellation the question stays, and a take-back said with it removes it.
func TestWisprNeverMindLeavesTheHistory(t *testing.T) {
	for _, tc := range []struct {
		name, opening string
		neverMind     bool
		kept          bool
	}{
		{"control: nothing cancelled", "Wispr, what about ABC-42?", false, true},
		{"control: a take-back with the question", "Wispr, what about ABC-42? never mind", false, false},
		{"Wispr, never mind", "Wispr, what about ABC-42?", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRigOpening(t, []string{tc.opening}, true, answers(2)...)
			if tc.neverMind {
				r.hearNow("Wispr, never mind", true)
			}
			r.hearNow("Wispr, what is the status of ABC-12?", true)
			if tc.neverMind {
				select {
				case r.sess = <-r.driver.opened:
				case <-time.After(r7Deadline):
					t.Fatal("control: no new exchange opened")
				}
			}
			if out := r.answer(t, "next", "what is the status of ABC-12?"); out.IssueKey != "ABC-12" || out.Say == "" {
				t.Fatalf("control: the next question took %q, said %q", out.IssueKey, out.Say)
			}
			if got := strings.Contains(lastPrompt(r.rig), "ABC-42"); got != tc.kept {
				t.Errorf("the next model's prompt holds the cancelled question: %v, want %v:\n%s", got, tc.kept, lastPrompt(r.rig))
			}
		})
	}
}
