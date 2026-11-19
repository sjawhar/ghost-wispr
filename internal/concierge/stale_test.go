package concierge

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAStaleRecordSpeaksItsAgeFirst (R6; run 8j): on a status question whose
// record has no event from the last hour, the answer comes first from the
// record: its status with its age, then the routing line that says the
// Concierge asks the holder, and the asked line last. The record's other
// facts are as old, and the holder answers what is new, so no model writes
// more: live, its facts repeated the status line and the answer ran past
// 20 s. The status line begins with the key, so on a searched turn it names
// the issue itself (R5), with no "I took KEY." before it.
func TestAStaleRecordSpeaksItsAgeFirst(t *testing.T) {
	for _, searched := range []bool{false, true} {
		r := newRig(t)
		heldIssue(r, "session", true)
		turn := "what is happening with ABC-12"
		if searched {
			r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word"}
			r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.012}, {Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.006}}
			turn = "what is happening with the realtime voice"
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		spoken := answerTurn(t, r, turn)
		want := []said{
			{"ABC-12 was in progress as of 2 hours ago.", voice.Fact},
			{"Asking the agent on ABC-12.", voice.Ack},
			{"I asked the agent on ABC-12. " + usualWait, voice.Ack},
		}
		if !slices.Equal(spoken, want) {
			t.Fatalf("searched %v: spoken = %+v, want %+v", searched, spoken, want)
		}
		if n := len(r.model.requests); n != 0 {
			t.Fatalf("searched %v: %d model calls, want none", searched, n)
		}
		r.agents.mu.Lock()
		sent := len(r.agents.sent)
		r.agents.mu.Unlock()
		if sent != 1 {
			t.Fatalf("searched %v: %d questions went to the holder, want 1", searched, sent)
		}
	}

	// Control: a fresh record gets neither line, and no question.
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "what is happening with ABC-12")
	if slices.ContainsFunc(spoken, func(s said) bool { return strings.Contains(s.text, " as of ") || strings.HasPrefix(s.text, "Asking ") }) {
		t.Fatalf("a fresh record: spoken = %+v", spoken)
	}
}

// TestTheAskingNoteKeepsTheAskToTheRecorder (R6; live): told the recorder
// asks the holder, the model said "The agent is being asked now.", which the
// routing line and the asked line already say.
func TestTheAskingNoteKeepsTheAskToTheRecorder(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in review."))}}
	answerTurn(t, r, "ask the agent on ABC-12 when slice one will be ready")
	if len(r.model.requests) == 0 || !strings.Contains(r.model.requests[0][0].Parts[0].Text, "Do not say that the agent is asked: the recorder says it.") {
		t.Fatal("the asking note does not keep the ask to the recorder")
	}
}

// TestASpokenKeyIsReadInTheFormsDeepgramWrites (R5; live at 08d3b8b): Deepgram
// writes a project that is a keyterm in capitals, and its number after a
// pause, with a thousands comma, or in words; each is the key. A lowercase
// word before a number in words is no key.
func TestASpokenKeyIsReadInTheFormsDeepgramWrites(t *testing.T) {
	for words, want := range map[string][]string{
		"When is happening with INFRA? 4592.":                                    {"INFRA-4592"},
		"Is INFRA 4,592 in review?":                                              {"INFRA-4592"},
		"What changed on INFRA four thousand five hundred and ninety two today?": {"INFRA-4592"},
		"what is happening with INFRA forty-five ninety-two":                     {"INFRA-4592"},
		"what about INFRA four five nine two":                                    {"INFRA-4592"},
		"What is the progress on INFRA 4592?":                                    {"INFRA-4592"},
		"what about ABC-12 and xyz 34":                                           {"ABC-12", "XYZ-34"},
		"I counted forty two people":                                             nil,
		"is INFRA one ready":                                                     nil,
	} {
		if got := issueKeysIn(words); !slices.Equal(got, want) {
			t.Errorf("issueKeysIn(%q) = %q, want %q", words, got, want)
		}
	}
	// The key so written is no search word.
	if got := searchTerms("what changed on INFRA four thousand five hundred and ninety two today"); got != "" {
		t.Errorf("searchTerms = %q, want none", got)
	}
}
