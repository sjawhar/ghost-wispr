package concierge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAOneDigitKeyIsReadWhenItsProjectIsHeld (live on 2026-11-25): "Who is
// working on INFRA-7?" was no key, since a key's number needed two digits;
// the turn searched "infra" instead. It fails on main (c57adc3) unless a
// key of one digit whose project the turn's view holds is the turn's key in
// every form the transcript writes ("INFRA-7", "infra 7", "INFRA seven",
// "INFRA? 7.", "A B C 7"), its project is no search word, the routing line
// names it, and a reply that says it picks it, or declines it, when it was
// offered.
func TestAOneDigitKeyIsReadWhenItsProjectIsHeld(t *testing.T) {
	r := newRig(t)
	// newRig's projects are ABC, XYZ, DEF and INFRA.
	view := r.concierge.viewProjects()
	for turn, want := range map[string]string{
		"Who is working on INFRA-7?":     "INFRA-7",
		"What is happening with INFRA 7": "INFRA-7",
		"what about infra 7":             "INFRA-7",
		"what about INFRA seven":         "INFRA-7",
		"Who is working on INFRA? 7.":    "INFRA-7",
		"the status of ABC-9 please":     "ABC-9",
		"what about ABC-9 and XYZ-12":    "XYZ-12,ABC-9",
	} {
		if got := keysOf(r.concierge.readTarget(view, turn)); got != want {
			t.Errorf("readTarget(%q) names %q, want %q", turn, got, want)
		}
	}
	for said, want := range map[string]string{
		"what about A B C 7":     "what about ABC-7",
		"what about A B C seven": "what about ABC-7",
		"I O U 5 dollars":        "I O U 5 dollars",
		"the A B C one":          "the A B C one",
		"A B C one oh five":      "A B C one oh five",
	} {
		if got, unread := view.readSpelledKeys(said); got != want || unread {
			t.Errorf("readSpelledKeys(%q) = %q, %v; want %q", said, got, unread, want)
		}
	}
	if got, want := r.concierge.readTarget(view, "Who is working on INFRA-7?"), "keys:INFRA-7||"; got != want {
		t.Errorf("readTarget = %q, want %q: the key's project is no search word", got, want)
	}
	if got, want := routingLine(view, "Who is working on INFRA-7?"), "Reading INFRA-7."; got != want {
		t.Errorf("routing line = %q, want %q", got, want)
	}
	offered := []candidate{{"INFRA-7", "Realtime voice"}, {"INFRA-12", "Wake word"}}
	for reply, want := range map[string]struct {
		key string
		ok  bool
	}{
		"INFRA-7":         {"INFRA-7", true},
		"infra 7, please": {"INFRA-7", true},
		"no, not INFRA-7": {"", true},
	} {
		r.concierge.offerPick(&r.exchange, "who is working on the voice", offered)
		if _, key, ok := r.concierge.takePick(&r.exchange, reply); key != want.key || ok != want.ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, %v", reply, key, ok, want.key, want.ok)
		}
	}
	// A no names any word and number of one digit; an offered key said
	// letter by letter picks whatever the project list says.
	unknown := newRig(t)
	unknown.concierge.mu.Lock()
	unknown.concierge.projects = nil
	unknown.concierge.mu.Unlock()
	for _, tc := range []struct {
		offered []candidate
		reply   string
		key     string
	}{
		{[]candidate{{"ABC-12", "Realtime voice"}, {"ABC-13", "Wake word"}}, "no, not INFRA-7", ""},
		{[]candidate{{"ABC-7", "Realtime voice"}, {"ABC-13", "Wake word"}}, "A B C seven", "ABC-7"},
	} {
		unknown.concierge.offerPick(&unknown.exchange, "who is working on the voice", tc.offered)
		if _, key, ok := unknown.concierge.takePick(&unknown.exchange, tc.reply); key != tc.key || !ok {
			t.Errorf("takePick(%q) = %q, %v; want %q, true", tc.reply, key, ok, tc.key)
		}
	}
}

// TestAOneDigitAfterAWordThatIsNoProjectIsSpeech: a number of one digit
// after a word that is no project of Dispatch's is speech ("the last 7
// days", "room 2 is free"), and so is one with a decimal or thousands part
// ("infra 6.8", "infra 7,500"); a version beside a key stays out ("version
// 2.5 of infra 12 shipped" is INFRA-12 alone). After a project, "one" is
// speech ("the INFRA one"), and so is a number word before "oh" ("INFRA one
// oh five"), the start of a longer number. With the project list unable to
// tell, no one-digit word is a key, so none is an outage, while a written
// key of two digits still is (#190). This is a control: it passes on main,
// and fails if a one-digit number is read as a key with no view, with its
// decimal part, or from "one" or a word before "oh".
func TestAOneDigitAfterAWordThatIsNoProjectIsSpeech(t *testing.T) {
	r := newRig(t)
	view := r.concierge.viewProjects()
	for turn, want := range map[string]string{
		"what changed in the last 7 days":   "",
		"does it build on infra 6.8":        "",
		"is infra 7,500 dollars over":       "",
		"version 2.5 of infra 12 shipped":   "INFRA-12",
		"what about infra 45.92 and ABC-12": "ABC-12,INFRA-45",
		// "One" after a project is as often a pronoun, and a number word
		// before "oh" starts a longer number.
		"the INFRA one":                "",
		"is INFRA one of our projects": "",
		"what about INFRA one oh five": "",
		"is INFRA one ready":           "",
	} {
		if got := keysOf(r.concierge.readTarget(view, turn)); got != want {
			t.Errorf("readTarget(%q) names %q, want %q", turn, got, want)
		}
	}
	for _, turn := range []string{"room 2 is free", "version 2.5 of infra 12 shipped", "X V 6 evals", "is INFRA one ready"} {
		for _, key := range issueKeysIn(turn) {
			if key != "INFRA-12" {
				t.Errorf("issueKeysIn(%q) holds %q, a one-digit word read with no view", turn, key)
			}
		}
	}
	unknown := projectsView{}
	for _, turn := range []string{"what changed in the last 7 days", "what about INFRA 7"} {
		if unknown.keyUntold(turn) {
			t.Errorf("keyUntold(%q) with the list unable to tell = true, want false", turn)
		}
	}
	if !unknown.keyUntold("what about ABC-12") {
		t.Errorf("keyUntold(%q) with the list unable to tell = false, want true", "what about ABC-12")
	}
}

// oneDigitIssue is INFRA-7, claimed by live agent session agent-7, whose
// newest event is fresh; a search of "infra" would run past its bound.
func oneDigitIssue(r *rig) {
	r.records.issues["INFRA-7"] = &dispatch.Issue{
		Key: "INFRA-7", Title: "Realtime voice", Status: "in_progress",
		UpdatedAt: testNow.Add(-5 * time.Minute),
		Claim:     &dispatch.Claim{Actor: actor("session", "agent-7", "Worker on INFRA-7")},
	}
	r.records.events["INFRA-7"] = []dispatch.Event{{Type: "comment.created", CreatedAt: testNow.Add(-5 * time.Minute)}}
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.records.searchDelay, r.records.kwDelay = time.Hour, time.Hour
	r.concierge.searchBound = 100 * time.Millisecond
}

// TestAOneDigitKeyIsAnsweredFromItsRecord (live on 2026-11-25): "Who is
// working on INFRA-7?" must read INFRA-7, not search "infra" and, the
// search past its bound, say "I cannot reach Dispatch yet." with the error
// sound. The first fact names the key. "Ask the agent on INFRA-7 is it
// merged" sends the holder "Is it merged?", as for a key of two digits. It
// fails on main, which never reads INFRA-7.
func TestAOneDigitKeyIsAnsweredFromItsRecord(t *testing.T) {
	r := newRig(t)
	oneDigitIssue(r)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-7 is in progress, updated 5 minutes ago."))}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "Who is working on INFRA-7?", recorder(&spoken))
	if errors.Is(err, voice.ErrUnreachable) || !slices.ContainsFunc(spoken, func(s said) bool { return s.text == "INFRA-7 is in progress, updated 5 minutes ago." }) {
		t.Fatalf("Answer = %v, spoken %+v; want INFRA-7's fact, no outage", err, spoken)
	}
	if calls := r.records.calls(); !slices.Contains(calls, "INFRA-7") {
		t.Fatalf("issue reads %q, want INFRA-7", calls)
	}
	if n := len(r.records.searches()); n != 0 {
		t.Fatalf("searched %d times, want none: the turn names its key", n)
	}
	if prompt := promptOf(r.model.requests[0]); !strings.Contains(prompt, "Begin your first sentence with INFRA-7") {
		t.Errorf("the model was not told to name INFRA-7 first")
	}

	r = newRig(t)
	oneDigitIssue(r)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-7 is in progress."))}}
	answerTurn(t, r, "ask the agent on INFRA-7 is it merged")
	if len(r.agents.sent) != 1 || !strings.Contains(r.agents.sent[0].Message, "Question about INFRA-7: Is it merged?\n") {
		t.Fatalf("sent %+v, want the question %q about INFRA-7", r.agents.sent, "Is it merged?")
	}
}
