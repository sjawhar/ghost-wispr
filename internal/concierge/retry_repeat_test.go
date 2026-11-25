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

// TestARetryDropsAClauseTheRoomHeard (live on 2026-11-25): a sentence failed
// R8's check, the model wrote again, and the retry said again what the
// first try had said. A retry's sentence is dropped when its words, in
// order and every one, are a clause of a sentence the room heard: the whole
// sentence again, punctuation aside, or a clause of it. The retry's new
// sentence is said.
func TestARetryDropsAClauseTheRoomHeard(t *testing.T) {
	const heard = "ABC-12 is in progress, and PR 80 is in review."
	for _, again := range []string{
		"ABC-12 is in progress and PR 80 is in review.",
		"PR 80 is in review.",
		"ABC-12 is in progress.",
	} {
		r := retryRig(t, []string{heard, "Its title is Realtime voice."}, heard, again+" Its title is Realtime voice.")
		spoken := texts(answerTurn(t, r, "what is happening with ABC-12"))
		if slices.Contains(spoken, again) || !slices.Contains(spoken, heard) || !slices.Contains(spoken, "Its title is Realtime voice.") {
			t.Errorf("after %q, a retry of %q spoke %q; want it dropped and the new sentence said", heard, again, spoken)
		}
	}
}

// TestARetryKeepsWhatChangesMeaning (Sol's review of #197 at 0b9cb31): a
// retry's sentence whose words a heard sentence holds, in another order or
// with a word added, removed or changed, can say something new: the other
// way round, a negation gone, another tense, another key. 0b9cb31 dropped
// it, reading a sentence as its set of words. It fails unless each is said.
func TestARetryKeepsWhatChangesMeaning(t *testing.T) {
	for _, tc := range []struct {
		heard []string
		next  string
	}{
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
	} {
		r := retryRig(t, append(slices.Clone(tc.heard), tc.next), strings.Join(tc.heard, " "), tc.next)
		if spoken := texts(answerTurn(t, r, "what is happening with ABC-12")); !slices.Contains(spoken, tc.next) {
			t.Errorf("after %q, a retry of %q spoke %q; want it said", tc.heard, tc.next, spoken)
		}
	}
}

// TestAFirstAttemptIsNotFiltered (Sol's review of #197): a first try's
// sentences are all said, whatever the room heard before them in other
// words. Only a retry can say again what its first try said. 0b9cb31
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
// says no repeat in other words, which no check of words can tell from a
// new fact. It fails at 0b9cb31, whose prompt ran the sentences together
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

// TestAHeardClauseIsOnlyARunOfItsWords: heardAgain's table. A sentence is
// heard again only when its words, lower case, punctuation aside and every
// word kept, in order, are a run of a text the room heard that stands as a
// sentence there: it starts a sentence of that text or follows "and", "but"
// or "so", and ends that sentence or comes before one of them. Sol's cases
// of a changed meaning are each new; so is a run under other words ("Nobody
// said ABC-12 is done.", "If PR 80 merges, ABC-12 is done."), and a repeat
// in other words, which the retry's prompt asks the model not to say.
func TestAHeardClauseIsOnlyARunOfItsWords(t *testing.T) {
	for _, tc := range []struct {
		heard  []string
		next   string
		repeat bool
	}{
		{[]string{"PR 80 is in review."}, "PR 80 is in review.", true},
		{[]string{"ABC-12 is in progress, and PR 80 is in review."}, "ABC-12 is in progress and PR 80 is in review.", true},
		{[]string{"ABC-12 is in progress, and PR 80 is in review."}, "PR 80 is in review.", true},
		{[]string{"ABC-12 is in progress and PR 80 is in review."}, "ABC-12 is in progress.", true},
		{[]string{"PR 80 merged, but ABC-12 is open."}, "ABC-12 is open.", true},
		{[]string{"I asked the agent on ABC-12. Its answer usually comes within a minute, with a chime."}, "Its answer usually comes within a minute, with a chime.", true},
		{[]string{"Reading the record.", "It is in review."}, "it is in review", true},
		{[]string{"PR 80 is in review."}, "PR 80 is merged.", false},
		{[]string{"It is done."}, "It is not done.", false},
		{[]string{"It is not done."}, "It is done.", false},
		{[]string{"PR 80 is in review."}, "PR 81 is in review.", false},
		{[]string{"Alex owns ABC-12."}, "Sam owns ABC-12.", false},
		{[]string{"ABC-12 blocks XYZ-7."}, "XYZ-7 blocks ABC-12.", false},
		{[]string{"PR 80 is merged.", "ABC-12 is done."}, "PR 80 is merged and ABC-12 is done.", false},
		{[]string{"PR 80 was in review."}, "PR 80 is in review.", false},
		{[]string{"PR 80 is merged and PR 81 is in review."}, "PR 80 is in review.", false},
		{[]string{"PR 80 blocks PR 81."}, "PR 81 blocks PR 80.", false},
		{[]string{"ABC-12 blocks XYZ-7."}, "ABC-7 blocks XYZ-12.", false},
		{[]string{"Alex asked Sam."}, "Sam asked Alex.", false},
		{[]string{"PR 80 is in review."}, "It is in review.", false},
		{[]string{"PR 80 is in review."}, "This is in review.", false},
		{[]string{"Nobody said ABC-12 is done."}, "ABC-12 is done.", false},
		{[]string{"ABC-12 is done if PR 80 merges."}, "ABC-12 is done.", false},
		{[]string{"If PR 80 merges, ABC-12 is done."}, "ABC-12 is done.", false},
		{[]string{"ABC-12 is done, if PR 80 merges."}, "ABC-12 is done.", false},
		{[]string{"Yes, PR 80 merged the tap."}, "Yes.", false},
		{[]string{"ABC-12 is in progress, and PR 80 is in review."}, "PR 80 is in review, and ABC-12 is in progress.", false},
		{[]string{"INFRA-7 is in todo status with priority P1, and was updated two days ago."}, "INFRA-7 is in todo status with priority P1, updated two days ago.", false},
	} {
		if got := heardAgain(tc.next, tc.heard); got != tc.repeat {
			t.Errorf("after %q, heardAgain(%q) = %v, want %v", tc.heard, tc.next, got, tc.repeat)
		}
	}
}
