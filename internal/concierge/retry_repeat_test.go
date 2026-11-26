package concierge

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// retryRig is a rig whose ABC-12 record holds every sentence of facts, so
// R8's check passes each, and whose model says first, then a sentence R8
// refuses (a name no record holds), then, asked again, retry.
func retryRig(t *testing.T, facts []string, first, retry string) *rig {
	t.Helper()
	r := newRig(t)
	heldIssue(r, "none", false)
	body, err := json.Marshal(map[string]string{"body": strings.Join(facts, " ")})
	if err != nil {
		t.Fatal(err)
	}
	events := r.records.events["ABC-12"]
	events[0].Payload = body
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text(first + " It went to Morgan."))},
		{chunk(text(retry))},
	}
	return r
}

// texts is what spoken says, in order.
func texts(spoken []said) []string {
	var out []string
	for _, s := range spoken {
		out = append(out, s.text)
	}
	return out
}

// TestARetryDropsASentenceSaidWordForWord: a sentence failed R8's check, the
// model wrote again, and the retry began with the sentence the first try had
// said. It is dropped, in another case or spacing too, and the retry's new
// sentence is said.
func TestARetryDropsASentenceSaidWordForWord(t *testing.T) {
	const heard = "ABC-12 is in progress, and PR 80 is in review."
	for _, again := range []string{heard, "abc-12 is in progress,  and PR 80 is in review."} {
		r := retryRig(t, []string{heard, "Its title is Realtime voice."}, heard, again+" Its title is Realtime voice.")
		spoken := texts(answerTurn(t, r, "what is happening with ABC-12"))
		if n := len(slices.DeleteFunc(slices.Clone(spoken), func(s string) bool { return !strings.EqualFold(s, heard) })); n != 1 || !slices.Contains(spoken, "Its title is Realtime voice.") {
			t.Errorf("after %q, a retry of %q spoke %q; want it once and the new sentence said", heard, again, spoken)
		}
	}
}

// TestARetryKeepsWhatChangesMeaning (Sol's reviews of #197 at 0b9cb31 and
// 7791198): a retry's sentence that is not a sentence the room heard, word
// for word with its punctuation, is said, though its words are a heard
// sentence's in another order, a part of one, or the same words with other
// punctuation: each can say something new. 0b9cb31 read a sentence as its set
// of words, and 7791198 as a clause of a heard sentence with its punctuation
// gone; each dropped some of these. It fails unless each is said.
func TestARetryKeepsWhatChangesMeaning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		heard []string
		next  string
	}{
		// Sol's first review: the words in another order, added, gone or
		// changed.
		{[]string{"ABC-12 blocks XYZ-7."}, "XYZ-7 blocks ABC-12."},
		{[]string{"It is not done."}, "It is done."},
		{[]string{"It is done."}, "It is not done."},
		{[]string{"PR 80 was in review."}, "PR 80 is in review."},
		{[]string{"ABC-12 blocks XYZ-7."}, "ABC-7 blocks XYZ-12."},
		{[]string{"Alex asked Sam."}, "Sam asked Alex."},
		{[]string{"PR 80 blocks PR 81."}, "PR 81 blocks PR 80."},
		{[]string{"PR 80 is merged and PR 81 is in review."}, "PR 80 is in review."},
		{[]string{"PR 80 is merged.", "ABC-12 is done."}, "PR 80 is merged and ABC-12 is done."},
		{[]string{"PR 80 is in review."}, "It is in review."},
		{[]string{"ABC-12 is in progress, and PR 80 is in review."}, "PR 80 is in review, and ABC-12 is in progress."},
		// Sol's second review: a clause under a report, a condition, a
		// checklist or a negation is no fact the room heard asserted.
		{[]string{"Sam said ABC-12 is open and XYZ-7 is done."}, "XYZ-7 is done."},
		{[]string{"If PR 80 merges then ABC-12 is open and XYZ-7 is done."}, "XYZ-7 is done."},
		{[]string{"Checks to make: ABC-12 is open and XYZ-7 is done."}, "XYZ-7 is done."},
		{[]string{"Nobody said ABC-12 is open and XYZ-7 is done."}, "XYZ-7 is done."},
		{[]string{"ABC-12 is open and XYZ-7 is done if PR 80 merges."}, "ABC-12 is open."},
		// Sol's second review: punctuation that changes the assertion.
		{[]string{"No tests failed."}, "No, tests failed."},
		{[]string{"XYZ-7 is done?"}, "XYZ-7 is done."},
		// A clause of a heard sentence, or the same words with a comma gone.
		{[]string{"ABC-12 is in progress, and PR 80 is in review."}, "PR 80 is in review."},
		{[]string{"ABC-12 is in progress, and PR 80 is in review."}, "ABC-12 is in progress and PR 80 is in review."},
	} {
		r := retryRig(t, append(slices.Clone(tc.heard), tc.next), strings.Join(tc.heard, " "), tc.next)
		if spoken := texts(answerTurn(t, r, "what is happening with ABC-12")); !slices.Contains(spoken, tc.next) {
			t.Errorf("after %q, a retry of %q spoke %q; want it said", tc.heard, tc.next, spoken)
		}
	}
}

// TestAFirstAttemptIsNotFiltered (Sol's review of #197): a first try's
// sentences are all said but one the room heard word for word. 0b9cb31
// dropped "XYZ-7 blocks ABC-12." after "ABC-12 blocks XYZ-7." in one try.
func TestAFirstAttemptIsNotFiltered(t *testing.T) {
	for _, answer := range [][]string{
		{"ABC-12 blocks XYZ-7.", "XYZ-7 blocks ABC-12."},
		{"ABC-12 is in progress, and PR 80 is in review.", "PR 80 is in review."},
	} {
		r := newRig(t)
		heldIssue(r, "none", false)
		body, err := json.Marshal(map[string]string{"body": strings.Join(answer, " ")})
		if err != nil {
			t.Fatal(err)
		}
		r.records.events["ABC-12"][0].Payload = body
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(strings.Join(answer, " ")))}}
		spoken := texts(answerTurn(t, r, "what is happening with ABC-12"))
		for _, sentence := range answer {
			if !slices.Contains(spoken, sentence) {
				t.Errorf("a first try of %q spoke %q; want each sentence said", answer, spoken)
			}
		}
	}
}

// TestARetryIsToldWhatTheRoomHeard: a retry's prompt names each sentence
// the room already heard and asks for none of them again, so the model
// says no repeat in other words, which only a sentence said word for word is
// dropped as. It fails at 0b9cb31, whose prompt ran the sentences together
// and asked nothing of them.
func TestARetryIsToldWhatTheRoomHeard(t *testing.T) {
	const heard = "ABC-12 is in progress, and PR 80 is in review."
	r := retryRig(t, []string{heard}, heard, "Its title is Realtime voice.")
	answerTurn(t, r, "what is happening with ABC-12")
	if len(r.model.requests) < 2 {
		t.Fatalf("the model was asked %d times, want a retry", len(r.model.requests))
	}
	if prompt := promptOf(r.model.requests[1]); !strings.Contains(prompt, "\n- "+heard+"\n") || !strings.Contains(prompt, "Say none of them again") {
		t.Errorf("the retry's prompt does not name %q and ask for it not to be said again:\n%s", heard, prompt)
	}
}

// TestASentenceSaidAgainIsWordForWord: saidBefore's table. A sentence is
// said again only when it is a sentence the room heard, word for word with
// its punctuation, in any case or spacing.
func TestASentenceSaidAgainIsWordForWord(t *testing.T) {
	heard := []string{"Reading the record.", "PR 80 is in review."}
	for sentence, want := range map[string]bool{
		"PR 80 is in review.":    true,
		"pr 80  is in review.":   true,
		" PR 80 is in review. ":  true,
		"PR 80 is in review?":    false,
		"PR 80 is in review":     false,
		"PR 80 is in review, it": false,
		"It is in review.":       false,
	} {
		if got := saidBefore(sentence, heard); got != want {
			t.Errorf("after %q, saidBefore(%q) = %v, want %v", heard, sentence, got, want)
		}
	}
}
