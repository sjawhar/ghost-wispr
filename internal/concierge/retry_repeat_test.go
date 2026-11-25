package concierge

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// TestARetryRepeatsNoSentenceInSubstance (live on 2026-11-25): a sentence of
// more than 20 words failed R8's check, the model wrote again, and the room
// heard its first sentence twice in other words: "INFRA-7 is in todo status
// with priority P1, and was updated two days ago." then "INFRA-7 is in todo
// status with priority P1, updated two days ago.". Only a repeat word for
// word was dropped. It fails unless a retry's sentence that says nothing the
// room has not heard is dropped, while its new sentence is said.
func TestARetryRepeatsNoSentenceInSubstance(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("ABC-12 is in progress, and PR 80 is in review. It went to Alex."))},
		{chunk(text("ABC-12 is in progress and PR 80 is in review. Its title is Realtime voice."))},
	}
	spoken := answerTurn(t, r, "what is happening with ABC-12")
	repeats := 0
	for _, s := range spoken {
		if strings.Contains(s.text, "PR 80") {
			repeats++
		}
	}
	if repeats != 1 || !saidText(spoken, "Its title is Realtime voice.") {
		t.Fatalf("spoken %+v; want PR 80's sentence once, and the retry's new sentence", spoken)
	}
}

// TestASentenceThatSaysNothingNewIsDropped: a sentence is a repeat when each
// of its words that carries meaning is in one sentence the room already
// heard, whatever its case, punctuation and small words. One that adds a
// word that carries meaning, a "not", another number or another subject
// ("it") among them, is said.
func TestASentenceThatSaysNothingNewIsDropped(t *testing.T) {
	for _, tc := range []struct {
		said, sentence string
		repeat         bool
	}{
		{"INFRA-7 is in todo status with priority P1, and was updated two days ago.", "INFRA-7 is in todo status with priority P1, updated two days ago.", true},
		{"It is in review.", "it is in review", true},
		{"Yes, PR 80 merged the tap.", "Yes.", true},
		{"INFRA-7's agent is not answering.", "The agent of INFRA-7 is not answering.", true},
		{"INFRA-7 is todo, updated two days ago.", "INFRA-7 is todo with priority P1.", false},
		{"INFRA-7 is todo.", "INFRA-14 is todo.", false},
		{"It is done.", "It is not done.", false},
		{"I took INFRA-7, Realtime voice.", "INFRA-7, Realtime voice, is todo.", false},
		{"PR 80 is in review.", "PR 81 is in review.", false},
		{"PR 80 is in review.", "It is in review.", false},
	} {
		if got := saysNothingNew(tc.sentence, []string{"Reading the record.", tc.said}); got != tc.repeat {
			t.Errorf("after %q, saysNothingNew(%q) = %v, want %v", tc.said, tc.sentence, got, tc.repeat)
		}
	}
}

// saidText reports whether spoken holds text.
func saidText(spoken []said, text string) bool {
	for _, s := range spoken {
		if s.text == text {
			return true
		}
	}
	return false
}
