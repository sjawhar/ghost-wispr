package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// eventually waits up to 2 s for done to hold.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 2 s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAReplyWithoutInReplyToIsAskedForOnce: a message from the asked agent
// that names no question it answers is never taken as the answer. The agent
// is asked once to reply again, and that reply, which names the request,
// lands on the task.
func TestAReplyWithoutInReplyToIsAskedForOnce(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "is it merged", "ABC-12")
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m1", SourceSession: "agent-7", Text: "pong"}); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskWorking || task.AnswerWritten != "" {
		t.Fatalf("a message with no in_reply_to was taken as the answer: %+v", task)
	}
	if len(r.agents.sent) != 2 {
		t.Fatalf("sent %d messages, want the question and one request to reply again", len(r.agents.sent))
	}
	request := r.agents.sent[1]
	if request.Target != "agent-7" || request.InReplyTo != "m1" || !strings.Contains(request.Message, "in_reply_to") || !strings.Contains(request.Message, "ABC-12") {
		t.Fatalf("request to reply again = %+v", request)
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m2", SourceSession: "agent-7", Text: "pong again"}); err != nil {
		t.Fatal(err)
	}
	if len(r.agents.sent) != 2 {
		t.Fatalf("asked again a second time: %d messages", len(r.agents.sent))
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Pong."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m3", SourceSession: "agent-7", InReplyTo: "evt-2", Text: "pong"}); err != nil {
		t.Fatal(err)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskDone || task.AnswerWritten != "pong" {
		t.Fatalf("the reply to the request = %+v, %v; want the task done", task, err)
	}
}

// TestAMessageWithoutInReplyToFromAnUnaskedSessionIsDropped: a session the
// unit asked nothing gets no request and changes nothing.
func TestAMessageWithoutInReplyToFromAnUnaskedSessionIsDropped(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "m1", SourceSession: "agent-9", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(r.agents.sent) != 1 {
		t.Fatalf("sent %d messages, want only the question", len(r.agents.sent))
	}
}

// TestAFullAnswersQueueKeepsEveryAnnouncement: more answers land than the
// Supervisor has read; every one is announced once it reads, none twice.
func TestAFullAnswersQueueKeepsEveryAnnouncement(t *testing.T) {
	r := newRig(t)
	const answers = 40
	for i := range answers {
		r.ask(t, fmt.Sprintf("agent-%d", i), "q", "")
		r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(text(fmt.Sprintf("Answer %d.", i)))})
	}
	for i := range answers {
		reply := &envoy.Message{EventID: fmt.Sprintf("r%d", i), SourceSession: fmt.Sprintf("agent-%d", i), InReplyTo: fmt.Sprintf("evt-%d", i+1), Text: "x"}
		if err := r.concierge.Deliver(context.Background(), reply); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for len(got) < answers {
		select {
		case answer := <-r.concierge.Answers():
			got = append(got, answer.TaskID)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d announcements of %d: %q", len(got), answers, got)
		}
	}
	slices.Sort(got)
	if got = slices.Compact(got); len(got) != answers {
		t.Fatalf("announcements repeat: %d distinct of %d", len(got), answers)
	}
}

// TestTheAnswerToolIsNotTheRunners: the Supervisor answers voice.AnswerTool
// itself through Answer, so the runner runs only the six tools.
func TestTheAnswerToolIsNotTheRunners(t *testing.T) {
	r := newRig(t)
	args, _ := json.Marshal(voice.AnswerArgs{Question: "q"})
	if _, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{Name: voice.AnswerTool, Args: args}); err == nil {
		t.Fatal("the runner ran the answer tool")
	}
}

// TestEndingTheSessionCancelsItsOpenQuestion: a meeting that ends with a
// question still out cancels its task; the late answer is kept for the
// session's page and not announced.
func TestEndingTheSessionCancelsItsOpenQuestion(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	if err := r.concierge.SessionEnded("meeting-1"); err != nil {
		t.Fatal(err)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskCancelled {
		t.Fatalf("task after the session ended = %+v, %v; want cancelled", task, err)
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "late"}); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.AnswerWritten != "late" || task.State != storage.VoiceTaskCancelled {
		t.Fatalf("late answer = %+v; want it kept and the task cancelled", task)
	}
	select {
	case answer := <-r.concierge.Answers():
		t.Fatalf("announced an answer of an ended meeting: %+v", answer)
	case <-time.After(300 * time.Millisecond):
	}
}

// offeringNow waits until the pump offers an announcement, and returns it.
func offeringNow(t *testing.T, r *rig) voice.PendingAnswer {
	t.Helper()
	var offered voice.PendingAnswer
	eventually(t, "an announcement was offered", func() bool {
		r.concierge.mu.Lock()
		defer r.concierge.mu.Unlock()
		if r.concierge.offering == nil {
			return false
		}
		offered = *r.concierge.offering
		return true
	})
	return offered
}

// noAnnouncement fails if Answers hands anything over within wait.
func noAnnouncement(t *testing.T, r *rig, wait time.Duration, why string) {
	t.Helper()
	select {
	case answer := <-r.concierge.Answers():
		t.Fatalf("announced %s: %+v", why, answer)
	case <-time.After(wait):
	}
}

// TestDeletingTheSessionDropsItsAnnouncements: the announcement being offered
// and the one queued behind it are both dropped by SessionDeleted alone; the
// tasks' rows still exist here, so no recheck of the store drops them.
func TestDeletingTheSessionDropsItsAnnouncements(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "ABC-12")
	r.ask(t, "agent-8", "q", "")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("First."))}, {chunk(text("Second."))}}
	for i, agent := range []string{"agent-7", "agent-8"} {
		reply := &envoy.Message{EventID: fmt.Sprintf("r%d", i), SourceSession: agent, InReplyTo: fmt.Sprintf("evt-%d", i+1), Text: "Done."}
		if err := r.concierge.Deliver(context.Background(), reply); err != nil {
			t.Fatal(err)
		}
	}
	offeringNow(t, r)
	eventually(t, "the second answer was queued", func() bool {
		r.concierge.mu.Lock()
		defer r.concierge.mu.Unlock()
		return len(r.concierge.queue) == 1
	})
	r.concierge.SessionDeleted("meeting-1")
	noAnnouncement(t, r, 400*time.Millisecond, "an answer of a deleted meeting")
}

// TestEndingTheSessionDropsItsAnnouncements: an answer that landed but was
// not yet read is not announced once its meeting ended; it stays in the
// ledger for the session's page.
func TestEndingTheSessionDropsItsAnnouncements(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	offeringNow(t, r)
	if err := r.concierge.SessionEnded("meeting-1"); err != nil {
		t.Fatal(err)
	}
	noAnnouncement(t, r, 400*time.Millisecond, "an answer of an ended meeting")
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.AnswerWritten != "Done." {
		t.Fatalf("the answer left the ledger: %+v, %v", task, err)
	}
}

// TestDeletingTheSessionDropsItsStateForGood: what the Concierge holds of a
// deleted meeting goes, and a read or a send still running for it adds none
// back.
func TestDeletingTheSessionDropsItsStateForGood(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.records.issues["ABC-12"].Claim = &dispatch.Claim{Actor: actor("session", "agent-9", "Worker")}
	r.ask(t, "agent-7", "q", "")
	// A read of the record still running when the delete comes.
	released := make(chan struct{})
	r.records.block, r.records.blockKey = released, "ABC-12"
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12")
	eventually(t, "the read started", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	// A question still being sent when the delete comes.
	r.name(t, "agent-8")
	r.agents.onSend = func(context.Context, string) error {
		r.concierge.SessionDeleted("meeting-1")
		return nil
	}
	r.run(t, "ask_agent", map[string]string{"session": "agent-8", "question": "q"})
	r.agents.onSend = nil
	close(released)
	time.Sleep(200 * time.Millisecond)

	r.concierge.mu.Lock()
	defer r.concierge.mu.Unlock()
	for key := range r.concierge.lastAsk {
		if key[0] == "meeting-1" {
			t.Errorf("lastAsk keeps %v", key)
		}
	}
	for key := range r.concierge.named {
		if strings.HasPrefix(key, "meeting-1|") {
			t.Errorf("named keeps %q", key)
		}
	}
	for key := range r.concierge.prefetches {
		if strings.HasPrefix(key, "meeting-1|") {
			t.Errorf("prefetches keep %q", key)
		}
	}
	if len(r.concierge.working) != 0 || len(r.concierge.sentAs) != 0 || len(r.concierge.queue) != 0 {
		t.Errorf("working = %v, sentAs = %v, queue = %v; want nothing of the deleted meeting", r.concierge.working, r.concierge.sentAs, r.concierge.queue)
	}
}

// flakyTasks is the ledger with a read of tasks that fails while failing is
// set, as a busy database does.
type flakyTasks struct {
	*storage.SQLiteStore
	failing *atomic.Bool
}

func (f flakyTasks) VoiceTask(id string) (storage.VoiceTask, error) {
	if f.failing.Load() {
		return storage.VoiceTask{}, errors.New("database is locked")
	}
	return f.SQLiteStore.VoiceTask(id)
}

// TestABusyStoreDoesNotDropAnAnnouncement: a read of the ledger that fails
// for a moment keeps the announcement; only a task that is gone drops it.
func TestABusyStoreDoesNotDropAnAnnouncement(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	var failing atomic.Bool
	r.concierge.store = flakyTasks{r.store, &failing}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	offeringNow(t, r)
	failing.Store(true)
	time.Sleep(3 * offerRecheck)
	failing.Store(false)
	select {
	case answer := <-r.concierge.Answers():
		if answer.TaskID != "evt-1" {
			t.Fatalf("announced %+v", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a busy store dropped the announcement")
	}
}

func TestIssueKeysAreFoundAsTheyAreSpoken(t *testing.T) {
	for words, want := range map[string][]string{
		"what about infra 4592, and ABC-12?": {"INFRA-4592", "ABC-12"},
		"how is INFRA-4592 going":            {"INFRA-4592"},
		"what is happening with the voice":   nil,
		"room 2 is free":                     nil,
	} {
		if got := issueKeysIn(words); !slices.Equal(got, want) {
			t.Errorf("issueKeysIn(%q) = %q, want %q", words, got, want)
		}
	}
}

// TestTheLastKeyThatNamesAnIssueIsRead: "abc 12 in the last 30 days" also
// reads as LAST-30; the read tries the keys from the last and keeps the first
// that names an issue.
func TestTheLastKeyThatNamesAnIssueIsRead(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about abc 12 in the last 30 days", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "Realtime voice") || strings.Contains(prompt, "LAST-30") {
		t.Fatalf("the record read for the turn:\n%s", prompt)
	}
}

// voiceIssue is ABC-12 as the fakes hold it.
func voiceIssue(r *rig) {
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow.Add(-2 * time.Hour)}
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "done", UpdatedAt: testNow.Add(-time.Hour)}
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Status: "in_progress"}}
}

// TestHeardReadsTheRecordBeforeTheTurnEnds: the issue named in the turn so far
// is read while the asker still speaks, and Answer gives the model what was
// read instead of reading it again.
func TestHeardReadsTheRecordBeforeTheTurnEnds(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12")
	eventually(t, "the read of ABC-12", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12 today", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "read_issue") || !strings.Contains(prompt, "Realtime voice") {
		t.Fatalf("the prompt lacks what was read:\n%s", prompt)
	}
	if reads := len(slices.DeleteFunc(r.records.calls(), func(key string) bool { return key != "ABC-12" })); reads > 2 {
		t.Errorf("ABC-12 was read %d times; the Answer read it again", reads)
	}
}

// TestHeardSearchesWhenNoKeyIsSaid: a turn that describes the work is
// searched, and the best hit is read, before the turn ends; Answer uses that
// read and searches no more.
func TestHeardSearchesWhenNoKeyIsSaid(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	const turn = "what is happening with the realtime voice"
	r.concierge.Heard(context.Background(), &r.exchange, turn)
	eventually(t, "the read of the best hit", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "search_issues") || !strings.Contains(prompt, "Realtime voice") {
		t.Fatalf("the prompt lacks the search and the read:\n%s", prompt)
	}
	if searches := r.records.searches(); len(searches) != 1 {
		t.Fatalf("searched %d times, want once, before the turn ended: %q", len(searches), searches)
	}
}

// TestAStopEndsTheReadAhead: the exchange's context ends with a stop; the
// read Heard started ends with it, and its result is never used.
func TestAStopEndsTheReadAhead(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.records.block = make(chan struct{})
	ctx, stop := context.WithCancel(context.Background())
	r.concierge.Heard(ctx, &r.exchange, "what about ABC-12")
	eventually(t, "the read started", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	stop()
	eventually(t, "the read ended with the stop", func() bool {
		r.concierge.mu.Lock()
		defer r.concierge.mu.Unlock()
		p := r.concierge.prefetches[exchangeKey(&r.exchange)]
		if p == nil {
			return true
		}
		for _, read := range p.reads {
			select {
			case <-read.done:
			default:
				return false
			}
		}
		return true
	})
}

// TestAStopBeforeTheSendSendsNothing: a stop that comes after the holder was
// looked up and before the question goes keeps the room's question in the
// unit: nothing is sent and no task is kept.
func TestAStopBeforeTheSendSendsNothing(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	r.agents.onLookup = func(string) { stop() }
	raw, _ := json.Marshal(map[string]string{"session": "agent-7", "question": "q"})
	if _, err := r.concierge.Tools(&r.exchange).Run(ctx, voice.ToolCall{Name: "ask_agent", Args: raw}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after the stop = %v, want context.Canceled", err)
	}
	if len(r.agents.sent) != 0 {
		t.Fatalf("sent %d messages after the stop", len(r.agents.sent))
	}
	if tasks, err := r.store.VoiceTasks("meeting-1"); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks after the stop = %+v, %v; want none", tasks, err)
	}
}

// TestAFinalTurnThatNamesAnotherIssueReadsThatOne: the final turn corrects
// the issue; Answer reads the one it names.
func TestAFinalTurnThatNamesAnotherIssueReadsThatOne(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12")
	eventually(t, "the read of ABC-12", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-13 is done."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12, no, ABC-13", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "Wake word") {
		t.Fatalf("the prompt lacks ABC-13, which the final turn named:\n%s", prompt)
	}
}

// TestAnAnswerOfAnEndedSessionIsNotAnnounced: the store says the meeting
// ended (the unit restarted, say, and SessionEnded never ran); an answer that
// lands after is kept and not announced.
func TestAnAnswerOfAnEndedSessionIsNotAnnounced(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	if err := r.store.EndSession("meeting-1", testNow, ""); err != nil {
		t.Fatal(err)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	noAnnouncement(t, r, 600*time.Millisecond, "an answer of an ended meeting")
	if task, _ := r.store.VoiceTask("evt-1"); task.AnswerWritten != "Done." {
		t.Fatalf("the answer was not kept: %+v", task)
	}
}

// TestADescribedTurnGetsTheSearchNotAHolder: a turn with no key is searched;
// the model is told the hits may not be what the question is about, and no
// holder is looked up, so no session becomes askable from a search.
func TestADescribedTurnGetsTheSearchNotAHolder(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.records.issues["ABC-12"].Claim = &dispatch.Claim{Actor: actor("session", "agent-9", "Worker")}
	r.agents.states["agent-9"] = envoy.SessionInfo{SessionID: "agent-9", State: envoy.StateLive}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "who joined this meeting so far", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "may not be what the question is about") || strings.Contains(prompt, "issue_holder") {
		t.Fatalf("the prompt for a described turn:\n%s", prompt)
	}
	if r.concierge.wasNamed(&r.exchange, "agent-9") {
		t.Fatal("a search made a holder askable")
	}
}

// TestAReadThatFoundNothingIsNotReadAgain: the read Heard made of the final
// turn finished and found nothing (LAST-24, of a project LAST that has no such
// issue); Answer does not read it again.
func TestAReadThatFoundNothingIsNotReadAgain(t *testing.T) {
	r := newRig(t)
	r.concierge.mu.Lock()
	r.concierge.projects["LAST"] = true
	r.concierge.mu.Unlock()
	const turn = "what changed in the last 24 hours"
	r.concierge.Heard(context.Background(), &r.exchange, turn)
	eventually(t, "the read of LAST-24", func() bool { return slices.Contains(r.records.calls(), "LAST-24") })
	time.Sleep(100 * time.Millisecond)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Nothing."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	if reads := len(slices.DeleteFunc(r.records.calls(), func(key string) bool { return key != "LAST-24" })); reads != 1 {
		t.Fatalf("LAST-24 was read %d times, want once", reads)
	}
}

// TestWithoutDispatchHeardReadsNothing: with no Dispatch, Heard starts no
// read, so every interim word does not log a failure.
func TestWithoutDispatchHeardReadsNothing(t *testing.T) {
	r := newRig(t)
	r.concierge.records = nil
	r.concierge.Heard(context.Background(), &r.exchange, "what about ABC-12")
	r.concierge.mu.Lock()
	defer r.concierge.mu.Unlock()
	if len(r.concierge.prefetches) != 0 {
		t.Fatalf("Heard started a read with no Dispatch: %v", r.concierge.prefetches)
	}
}

// TestARequestToReplyAgainKeepsItsKey: a request to reply again that may
// have reached Envoy before its send failed is sent again under the same
// idempotency key, so the agent gets it once.
func TestARequestToReplyAgainKeepsItsKey(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	fail := true
	r.agents.onSend = func(context.Context, string) error {
		if fail {
			fail = false
			return context.DeadlineExceeded
		}
		return nil
	}
	for _, id := range []string{"m1", "m2"} {
		if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: id, SourceSession: "agent-7", Text: "pong"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.agents.sent) != 3 {
		t.Fatalf("sent %d messages, want the question and the request twice", len(r.agents.sent))
	}
	if first, second := r.agents.sent[1].IdempotencyKey, r.agents.sent[2].IdempotencyKey; first == "" || first != second {
		t.Fatalf("the request's keys = %q and %q, want one key", first, second)
	}
}
