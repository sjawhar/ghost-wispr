package concierge

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Sol's twelfth review of #191 at b4d340c
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6067235008),
// from Sol's probes, through the real Supervisor and Concierge.

// hearReply has the room reply to Wispr's question in finals, an endpoint
// after each when endpoints, else after the last alone.
func (r *cutRig) hearReply(finals []string, endpoints bool) {
	p0 := time.Now().Add(20 * time.Millisecond)
	for i, words := range finals {
		at := make([]time.Time, len(strings.Fields(words)))
		for j := range at {
			at[j] = p0.Add(time.Duration(i)*600*time.Millisecond + time.Duration(j)*30*time.Millisecond)
		}
		r.hearEnding(words, true, endpoints || i == len(finals)-1, at)
	}
}

// wantAsked fails t unless the model was asked want and a holder was sent it.
func wantAsked(t *testing.T, r *cutRig, want string) {
	t.Helper()
	if got := modelQuestion(r); got != want {
		t.Errorf("the model was asked %q, want %q", got, want)
	}
	if !slices.ContainsFunc(holderMessages(r), func(m string) bool { return strings.Contains(strings.ToLower(m), strings.ToLower(want)) }) {
		t.Errorf("the holder was sent %q, want %q", holderMessages(r), want)
	}
}

// endpointNames names a row with endpoints between its finals or none.
func endpointNames(name string, endpoints bool) string {
	if endpoints {
		return name + ", endpoints between"
	}
	return name + ", no endpoint"
}

// TestAnUnclosedQuoteTakesNothingBackFromATakeBack (Sol's twelfth review of
// #191, P2 1): a quotation never closed is no quotation. At b4d340c the
// opener of `does ABC-42 show "Retry?` quoted the rest of the reply, and
// "never mind" then "what about ABC-12?" took nothing back: with no endpoint
// the model was asked the whole reply, and with endpoints its recent words
// kept ABC-42. Controls: no quote, and a closed one.
func TestAnUnclosedQuoteTakesNothingBackFromATakeBack(t *testing.T) {
	for _, first := range []string{
		`does ABC-42 show "Retry?`,
		"does ABC-42 show “Retry?",
		"does ABC-42 show «Retry?",
		"control: does ABC-42 show Retry?",
		`control: does ABC-42 show "Retry"?`,
	} {
		for _, endpoints := range []bool{false, true} {
			t.Run(endpointNames(first, endpoints), func(t *testing.T) {
				r := askedBackRig(t)
				r.hearReply([]string{strings.TrimPrefix(first, "control: "), "never mind", "what about ABC-12?"}, endpoints)
				if out := r.answer(t, "reply", "what about ABC-12?"); out.Say == "" {
					t.Fatal("the question after the take-back was not answered")
				}
				wantAsked(t, r, "what about ABC-12?")
				if prompt := lastPrompt(r.rig); strings.Contains(prompt, "ABC-42") {
					t.Errorf("the model was given the cancelled words:\n%s", prompt)
				}
			})
		}
	}
}

// TestAQuotationHoldsItsInnerQuotesAndApostrophes (Sol's twelfth review of
// #191, P2 2): a mark closes only a quotation of its own kind, quotations
// nest, and a single mark after a word is an apostrophe. At b4d340c the
// inner opener of `"Press 'Retry' now, cancel, then retry"`, and the
// possessive of `"the users' option, cancel, then retry"`, closed the outer
// quotation, and its "cancel" took back the question: the model was asked
// `then retry" in the tooltip?`. The same passages split across finals after
// Wispr's question, with or without endpoints, are asked whole, also when
// the last final asks on its own (Sol's diagnostic: at b4d340c the answer
// read that final alone). Controls: a mixed pair of double marks, and a
// possessive before a real take-back.
func TestAQuotationHoldsItsInnerQuotesAndApostrophes(t *testing.T) {
	for _, tc := range []struct{ name, words, want string }{
		{"single inside double", `does ABC-12 show "Press 'Retry' now, cancel, then retry" in the tooltip?`, ""},
		{"curly single inside curly double", "does ABC-12 show “Press ‘Retry’ now, cancel, then retry” in the tooltip?", ""},
		{"curly single inside guillemets", "does ABC-12 show «Press ‘Retry’ now, cancel, then retry» in the tooltip?", ""},
		{"a possessive inside double", `does ABC-12 show "the users' option, cancel, then retry" in the tooltip?`, ""},
		{"a curly possessive inside curly double", "does ABC-12 show “the users’ option, cancel, then retry” in the tooltip?", ""},
		{"a possessive inside guillemets", "does ABC-12 show «the users' option, cancel, then retry» in the tooltip?", ""},
		{"control: a mixed pair of double marks", "does ABC-12 show “Press Retry now, cancel, then retry\" in the tooltip?", ""},
		{"control: a possessive, then a take-back", "did ABC-42 show the users' cancel option? never mind. what about ABC-12?", "what about ABC-12?"},
		{"a possessive before a comma, a take-back, then a possessive", "what about the users', never mind. what about the bosses' plan for ABC-12?", "what about the bosses' plan for ABC-12?"},
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
	for _, tc := range []struct {
		name   string
		finals []string
	}{
		{"a quotation", []string{`does ABC-12 show "Please`, "cancel", `then retry" in the tooltip?`}},
		{"single inside double", []string{`does ABC-12 show "Press 'Retry' now`, "cancel", `then retry" in the tooltip?`}},
		{"curly single inside curly double", []string{"does ABC-12 show “Press ‘Retry’ now", "cancel", "then retry” in the tooltip?"}},
		{"a possessive inside guillemets", []string{"does ABC-12 show «the users' option", "cancel", "then retry» in the tooltip?"}},
	} {
		for _, endpoints := range []bool{false, true} {
			t.Run(endpointNames(tc.name+" across finals", endpoints), func(t *testing.T) {
				r := askedBackRig(t)
				r.hearReply(tc.finals, endpoints)
				if out := r.answer(t, "reply", tc.finals[len(tc.finals)-1]); out.Say == "" {
					t.Fatal("the reply was not answered")
				}
				wantAsked(t, r, strings.Join(tc.finals, " "))
			})
		}
	}
}
