package concierge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// TestAReadBeforeTheSessionExistsIsUsed fails if the record read while the
// asker speaks, before the recording session exists (the wake's exchange has
// no session id yet), is not the one Answer uses once the turn has the
// session's id: Answer then reads it all again, inside its 8 s (end-to-end
// run 8's step D).
func TestAReadBeforeTheSessionExistsIsUsed(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	early := r.exchange
	early.SessionID = "" // Deepgram's first final had not come yet
	r.concierge.Heard(context.Background(), &early, "what about ABC-12")
	eventually(t, "the read of ABC-12", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12 today", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	if reads := len(slices.DeleteFunc(r.records.calls(), func(key string) bool { return key != "ABC-12" })); reads > 2 {
		t.Errorf("ABC-12 was read %d times; the Answer read again what the asker's words had read", reads)
	}
}

// TestAnAskTurnAsksOnceTheHolderIsKnown fails if a person's "ask the agent
// on …" waits for the whole read of the record, which a slow spec read can
// hold past the answer's bound, before the question goes to the holder: the
// holder is known from issue_holder alone (R7).
func TestAnAskTurnAsksOnceTheHolderIsKnown(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	issue := r.records.issues["ABC-12"]
	issue.PrimaryArtifactID = "spec-1"
	issue.Artifacts = append(issue.Artifacts, struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}{ID: "spec-1", Slug: "spec"})
	r.records.docs["ABC-12/spec"] = "## Summary\n\nBuild a voice for meetings."
	r.records.docBlock = make(chan struct{}) // the spec read never ends in time
	t.Cleanup(func() { close(r.records.docBlock) })
	r.concierge.fastBound = 2 * time.Second
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var spoken []said
	start := time.Now()
	_ = r.concierge.Answer(context.Background(), &r.exchange, "ask the agent on ABC-12 whether the tap is merged", recorder(&spoken))
	r.agents.mu.Lock()
	sent := len(r.agents.sent)
	r.agents.mu.Unlock()
	if sent != 1 {
		t.Fatalf("%d questions went to the holder in %v, want 1 though the record read was slow", sent, time.Since(start).Round(time.Millisecond))
	}
	asked := false
	for _, s := range spoken {
		if strings.HasPrefix(s.text, "I asked the agent on ABC-12.") {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("the voice did not say the question went out: %+v", spoken)
	}
}

// TestTheHolderGetsACleanQuestion fails if the question sent to the holder,
// once "please ask the agent on KEY" is taken off the turn, starts with the
// punctuation Deepgram put after the key (end-to-end run 8f sent ". When
// slice one will be ready to try.").
func TestTheHolderGetsACleanQuestion(t *testing.T) {
	for _, turn := range []string{
		"please ask the agent on INFRA 4592. When slice one will be ready to try.",
		"please ask the agent on INFRA-4592. When slice one will be ready to try.",
	} {
		if got, want := fewestWords(turn), "When will slice one be ready to try?"; got != want {
			t.Errorf("fewestWords(%q) = %q, want %q", turn, got, want)
		}
	}
}

// TestAHistoryQuestionIsAboutTheRecord fails if a question about an issue's
// history and recent events, which the record holds, counts as one that is
// not about status, so R7 asks the holder (end-to-end run 8f asked).
func TestAHistoryQuestionIsAboutTheRecord(t *testing.T) {
	if turn := "what is the full history of the issue about real time spoken conversation with its recent events?"; !aboutStatus(turn) {
		t.Fatalf("aboutStatus(%q) = false, want the record to answer it", turn)
	}
}
