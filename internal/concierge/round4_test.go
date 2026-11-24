package concierge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAReplyToTheRequestLandsAfterTheMeetingEnded: the agent was asked to
// reply again with in_reply_to; the meeting ends; the reply to that request
// still lands in the ledger, for the session's page.
func TestAReplyToTheRequestLandsAfterTheMeetingEnded(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m1", SourceSession: "agent-7", Text: "pong"}); err != nil {
		t.Fatal(err)
	}
	if len(r.agents.sent) != 2 {
		t.Fatalf("control: sent %d messages, want the question and the request", len(r.agents.sent))
	}
	if err := r.concierge.SessionEnded("meeting-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m2", SourceSession: "agent-7", InReplyTo: "evt-2", Text: "late pong"}); err != nil {
		t.Fatal(err)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.AnswerWritten != "late pong" || task.State != storage.VoiceTaskCancelled {
		t.Fatalf("task = %+v, %v; want the late answer kept and the task cancelled", task, err)
	}
}

// TestAReplyLandsAfterTheMeetingEndedWhenItsRekeyFailed: the task kept its
// local id; the meeting ends while the question is sent; the reply that names
// the question's event still lands.
func TestAReplyLandsAfterTheMeetingEndedWhenItsRekeyFailed(t *testing.T) {
	r := newRig(t)
	r.concierge.store = failingRekey{r.store}
	r.name(t, "agent-7")
	r.agents.onSend = func(context.Context, string) error { return r.concierge.SessionEnded("meeting-1") }
	r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "q"})
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "late"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := r.store.VoiceTasks("meeting-1")
	if err != nil || len(tasks) != 1 || tasks[0].AnswerWritten != "late" {
		t.Fatalf("tasks = %+v, %v; want the late answer kept", tasks, err)
	}
}

// TestAQuestionSentAsTheMeetingEndsIsCancelled: the meeting ends while the
// question is on its way; its task is cancelled, like the meeting's others.
func TestAQuestionSentAsTheMeetingEndsIsCancelled(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	r.agents.onSend = func(context.Context, string) error { return r.concierge.SessionEnded("meeting-1") }
	r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "q"})
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskCancelled {
		t.Fatalf("task = %+v, %v; want cancelled", task, err)
	}
}

// TestAnEndThatCancelledNoRowStillCancelsTheSentQuestion: a meeting's end
// that lands after ask_agent found the meeting open, and whose own cancel of
// working rows missed this question's row, leaves the question working. The
// cancel after the send still ends it. The test cannot place the end between
// the closed check and the row's write; it marks the meeting ended during
// the send with no row cancel, which is the state such an end leaves.
func TestAnEndThatCancelledNoRowStillCancelsTheSentQuestion(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	r.agents.onSend = func(context.Context, string) error {
		r.concierge.mu.Lock()
		r.concierge.closeLocked("meeting-1", false)
		r.concierge.mu.Unlock()
		return nil
	}
	r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "q"})
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskCancelled {
		t.Fatalf("task = %+v, %v; want cancelled", task, err)
	}
}

// TestAFreshReadEndsWithTheAnswersBound: a read made at Answer, of a record
// Dispatch does not answer, ends with the answer's bound, and the voice says
// it cannot answer.
func TestAFreshReadEndsWithTheAnswersBound(t *testing.T) {
	r := newRig(t)
	r.concierge.fastBound = 300 * time.Millisecond
	heldIssue(r, "none", false)
	r.records.block, r.records.blockKey = make(chan struct{}), "ABC-12"
	defer close(r.records.block)
	var spoken []said
	start := time.Now()
	err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12", recorder(&spoken))
	if took := time.Since(start); took > time.Second || err == nil {
		t.Fatalf("Answer of a record that never answers = %v after %v, want an error at the 300 ms bound", err, took)
	}
	if len(spoken) != 1 || spoken[0].text != sorry {
		t.Fatalf("spoken = %+v, want the apology", spoken)
	}
}

// TestAnEndedMeetingAsksNoAgent: the meeting ended after the holder was
// looked up; nothing is sent and no task is kept.
func TestAnEndedMeetingAsksNoAgent(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	r.agents.onLookup = func(string) {
		if err := r.concierge.SessionEnded("meeting-1"); err != nil {
			t.Error(err)
		}
	}
	result := r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "q"})
	if result["error"] == nil || len(r.agents.sent) != 0 {
		t.Fatalf("ask_agent of an ended meeting = %v, %d sent", result, len(r.agents.sent))
	}
	if tasks, err := r.store.VoiceTasks("meeting-1"); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v; want none", tasks, err)
	}
}

// TestARequestSentAsTheMeetingIsDeletedAddsNothingBack: a request to reply
// again still being sent when the meeting is deleted leaves no lead behind.
func TestARequestSentAsTheMeetingIsDeletedAddsNothingBack(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	r.agents.onSend = func(context.Context, string) error {
		r.concierge.SessionDeleted("meeting-1")
		return nil
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m1", SourceSession: "agent-7", Text: "pong"}); err != nil {
		t.Fatal(err)
	}
	r.concierge.mu.Lock()
	defer r.concierge.mu.Unlock()
	if len(r.concierge.askedAgain) != 0 || len(r.concierge.againFor) != 0 {
		t.Fatalf("askedAgain = %v, againFor = %v; want nothing of the deleted meeting", r.concierge.askedAgain, r.concierge.againFor)
	}
}

// TestASecondWithdrawWaitsForTheHandOver: an end and a delete of the same
// meeting both return only once the hand-over of its offered answer settled.
func TestASecondWithdrawWaitsForTheHandOver(t *testing.T) {
	r := newRig(t)
	r.concierge.mu.Lock()
	r.concierge.offering = &voice.PendingAnswer{TaskID: "t", SessionID: "meeting-1"}
	r.concierge.offerDrop, r.concierge.offerDone = make(chan struct{}), make(chan struct{})
	first := r.concierge.withdrawLocked("meeting-1")
	second := r.concierge.withdrawLocked("meeting-1")
	r.concierge.mu.Unlock()
	if first == nil || second != first {
		t.Fatalf("withdraws = %v, %v; want both to wait on the same hand-over", first, second)
	}
}

// TestANotFoundOutsideAnIssueReadIsLogged: a key heard in the room that
// names no issue is not logged, but a 404 from the search, which a wrong
// Dispatch URL gives, is.
func TestANotFoundOutsideAnIssueReadIsLogged(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.run(t, "read_issue", map[string]string{"key": "LAST-24"})
	if strings.Contains(logs.String(), "concierge tool failed") {
		t.Fatalf("a key that names no issue was logged:\n%s", logs)
	}
	r.records.err = &dispatch.APIError{Status: 404, Code: "NOT_FOUND", Route: "search"}
	r.run(t, "search_issues", map[string]string{"query": "voice"})
	if !strings.Contains(logs.String(), "concierge tool failed") {
		t.Fatalf("a search Dispatch answered 404 was not logged:\n%s", logs)
	}
}

// TestAnUnreachableDispatchEndsTheKeyReads: when Dispatch does not answer,
// the read tries no more keys.
func TestAnUnreachableDispatchEndsTheKeyReads(t *testing.T) {
	r := newRig(t)
	r.records.err = fmt.Errorf("%w: dial tcp: no route to host", dispatch.ErrUnreachable)
	r.concierge.readRecord(context.Background(), &r.exchange, r.concierge.readTarget("what about abc 12 and xyz 34 and def 56"), nil, nil)
	// The first key's issue and holder are read side by side: one key, read
	// once by each.
	if calls := slices.Compact(r.records.calls()); len(calls) != 1 {
		t.Fatalf("read %q, want one key", calls)
	}
}

// TestANumberThatNamesNoIssueGivesWayToASearch: "at 10 am" reads as AT-10,
// a key of a project Dispatch has (AT here) that names no issue; the turn's
// words are searched instead.
func TestANumberThatNamesNoIssueGivesWayToASearch(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.concierge.mu.Lock()
	r.concierge.projects["AT"] = true
	r.concierge.mu.Unlock()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what did we decide at 10 am about the realtime voice", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "the issue a search of the question's words chose") || !strings.Contains(prompt, "Realtime voice") {
		t.Fatalf("the prompt for a turn whose only key names no issue:\n%s", prompt)
	}
	if !slices.Contains(r.records.calls(), "AT-10") || len(r.records.searches()) != 1 {
		t.Fatalf("issue reads %q, searches %q", r.records.calls(), r.records.searches())
	}
}

// TestASearchSendsDispatchTheFewestWords: a search carries the issue words
// of what was said, never the whole sentence.
func TestASearchSendsDispatchTheFewestWords(t *testing.T) {
	r := newRig(t)
	r.run(t, "search_issues", map[string]string{"query": "Wispr, what is happening with the realtime spoken conversation work?"})
	if searches := r.records.searches(); len(searches) != 1 || searches[0] != "realtime spoken conversation" {
		t.Fatalf("searches = %q, want [realtime spoken conversation]", searches)
	}
	result := r.run(t, "search_issues", map[string]string{"query": "what is it, then?"})
	if result["error"] == nil || len(r.records.searches()) != 1 {
		t.Fatalf("a sentence with no issue words was searched: %v, %q", result, r.records.searches())
	}
}

// TestAKeyOfNoProjectIsNotRead: with Dispatch's projects known, a number said
// after a word that is no project (the last 30 days) is no key, and goes to
// Dispatch neither as a read nor in a path.
func TestAKeyOfNoProjectIsNotRead(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.records.projects = []string{"ABC", "INFRA"}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about abc 12 in the last 30 days", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	if calls := r.records.calls(); slices.Contains(calls, "LAST-30") || !slices.Contains(calls, "ABC-12") {
		t.Fatalf("issue reads = %q, want ABC-12 and no LAST-30", calls)
	}
}

// TestASessionTitleIsSpokenWithoutEmoji: an agent session's title reaches
// the model, which speaks it, without emoji or control characters.
func TestASessionTitleIsSpokenWithoutEmoji(t *testing.T) {
	r := newRig(t)
	const title = "🍓 PI:\u200d Deploy the unit \u0007🚀"
	r.records.issues["ABC-12"] = claimed("ABC-12", "session", "agent-7", title)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive, Title: title}
	if holder := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); holder["title"] != "PI: Deploy the unit" {
		t.Errorf("holder title = %q", holder["title"])
	}
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Status: "backlog"}
	r.records.events["ABC-13"] = []dispatch.Event{{Type: "comment.created", CreatedAt: testNow, Actor: actor("session", "agent-7", title)}}
	issue := r.run(t, "read_issue", map[string]string{"key": "ABC-13"})
	events, _ := issue["recent_events"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["by"] != "PI: Deploy the unit" {
		t.Errorf("event by = %v", issue["recent_events"])
	}
	r.concierge.joined = func() []JoinedAgent { return []JoinedAgent{{SessionID: "agent-7", Title: title}} }
	joined := r.run(t, "list_joined_agents", map[string]string{})
	if agents := joined["agents"].([]any); len(agents) != 1 || agents[0].(map[string]any)["title"] != "PI: Deploy the unit" {
		t.Errorf("joined = %v", joined)
	}
}

// TestEveryLedgerChangeIsTold: each write that changes a meeting's
// questions, as the session page lists them, calls Changed with the meeting,
// so the page reads them again.
func TestEveryLedgerChangeIsTold(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var told []string
	r.concierge.changed = func(sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, sessionID)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(told)
	}
	r.ask(t, "agent-7", "q", "")
	afterAsk := count()
	if afterAsk == 0 {
		t.Fatal("asking an agent told no change")
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	afterAnswer := count()
	if afterAnswer == afterAsk {
		t.Fatal("an answer told no change")
	}
	if err := r.concierge.Spoken(context.Background(), "evt-1"); err != nil {
		t.Fatal(err)
	}
	afterSpoken := count()
	if afterSpoken == afterAnswer {
		t.Fatal("a spoken answer told no change")
	}
	r.ask(t, "agent-8", "q", "")
	beforeEnd := count()
	if err := r.concierge.SessionEnded("meeting-1"); err != nil {
		t.Fatal(err)
	}
	if count() == beforeEnd {
		t.Fatal("cancelling the open question told no change")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range told {
		if id != "meeting-1" {
			t.Fatalf("told a change of %q", id)
		}
	}
}
