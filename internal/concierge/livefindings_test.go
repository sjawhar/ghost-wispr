package concierge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAKeyWithADecimalPointIsOneKey (R5; live at 30ce413): Deepgram wrote
// "INFRA forty-five ninety-two" as "INFRA 45.92", and the Concierge read
// INFRA-45 and answered about it, the wrong issue.
func TestAKeyWithADecimalPointIsOneKey(t *testing.T) {
	for words, want := range map[string][]string{
		"What is the latest on INFRA 45.92?": {"INFRA-4592"},
		"is INFRA 4.592 done":                {"INFRA-4592"},
		"version 2.5 of infra 12 shipped":    {"INFRA-12"},
	} {
		if got := issueKeysIn(words); !slices.Equal(got, want) {
			t.Errorf("issueKeysIn(%q) = %q, want %q", words, got, want)
		}
	}
}

// TestAHolderThatIsNoEnvoySessionIsNoOutage (R6, R10; live at 30ce413): a
// claim by an actor whose id is no Envoy session id, such as a Legion
// daemon's "legion-daemon:INFRA", cannot be looked up, and Envoy says so; the
// voice said "I cannot reach INFRA-45's agent yet." with the error sound.
// The holder is no agent the Concierge can ask, which is no outage.
func TestAHolderThatIsNoEnvoySessionIsNoOutage(t *testing.T) {
	for _, c := range []struct {
		name, id  string
		lookupErr error
	}{
		{"an id Envoy cannot hold", "legion-daemon:INFRA", nil},
		{"a lookup Envoy refused", "agent-8", &envoy.ListenerError{Path: "/v1/registry/agent-8", Status: 400, Message: "invalid key"}},
	} {
		r := newRig(t)
		r.records.issues["ABC-12"] = claimed("ABC-12", "session", c.id, "")
		r.agents.lookupErr = c.lookupErr
		holder := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"})
		if holder["holder"] != "unresponsive" || holder["note"] != nil {
			t.Errorf("%s: issue_holder = %v, want unresponsive with no out-of-reach note", c.name, holder)
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
		var spoken []said
		err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with ABC-12", recorder(&spoken))
		if errors.Is(err, voice.ErrUnreachable) {
			t.Errorf("%s: Answer = %v, want no error sound", c.name, err)
		}
		if !slices.Contains(spoken, said{"ABC-12's agent is not answering.", voice.Fact}) {
			t.Errorf("%s: spoken = %+v, want the not-answering line", c.name, spoken)
		}
	}
}

// TestNoWordsFromTheModelSaysWhatIsKnown (R6; live at 30ce413): when the
// model says nothing and nothing was said, the voice says what the record
// holds, the issue's status, or that no issue was found, rather than sorry.
// A turn that read nothing keeps the apology.
func TestNoWordsFromTheModelSaysWhatIsKnown(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(thought("Nothing to say.")), {Candidates: nil}}, {chunk(thought("Still nothing."))}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with ABC-12", recorder(&spoken))
	if err != nil || len(spoken) == 0 || spoken[0] != (said{"ABC-12 is in progress.", voice.Fact}) {
		t.Fatalf("Answer = %v, spoken %+v; want the status, no error", err, spoken)
	}

	r = newRig(t)
	r.records.hits = []dispatch.SearchHit{}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(thought("Nothing to say.")), {Candidates: nil}}}
	spoken = nil
	if err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken)); err == nil || len(spoken) != 1 || spoken[0].text != sorry {
		t.Fatalf("a turn that read nothing: Answer = %v, spoken %+v; want the apology", err, spoken)
	}
}
