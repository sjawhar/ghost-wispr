package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
// Supervisor has read; every one is announced, in order, once it reads.
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

// TestDeletingTheSessionDropsItsAnnouncementAndState: an answer waiting to be
// read is dropped when its meeting is deleted, and the Concierge keeps nothing
// of that meeting.
func TestDeletingTheSessionDropsItsAnnouncementAndState(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "ABC-12")
	r.ask(t, "agent-8", "q", "")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer was queued", func() bool {
		r.concierge.mu.Lock()
		defer r.concierge.mu.Unlock()
		return len(r.concierge.queue) > 0 || r.concierge.offering != nil
	})
	if err := r.store.EndSession("meeting-1", testNow, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.DeleteSession("meeting-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	r.concierge.SessionDeleted("meeting-1")
	select {
	case answer := <-r.concierge.Answers():
		t.Fatalf("announced an answer of a deleted meeting: %+v", answer)
	case <-time.After(400 * time.Millisecond):
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r2", SourceSession: "agent-8", InReplyTo: "evt-2", Text: "late"}); err != nil {
		t.Fatalf("a late reply after the delete: %v", err)
	}
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
	if len(r.concierge.working) != 0 || len(r.concierge.queue) != 0 {
		t.Errorf("working = %v, queue = %v; want nothing of the deleted meeting", r.concierge.working, r.concierge.queue)
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
	r.concierge.Heard(&r.exchange, "what about ABC-12")
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
// searched, and the best hit is read, before the turn ends.
func TestHeardSearchesWhenNoKeyIsSaid(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.concierge.Heard(&r.exchange, "what is happening with the realtime voice")
	eventually(t, "the read of the best hit", func() bool { return slices.Contains(r.records.calls(), "ABC-12") })
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the realtime voice work", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "search_issues") || !strings.Contains(prompt, "Realtime voice") {
		t.Fatalf("the prompt lacks the search and the read:\n%s", prompt)
	}
}

// TestAFinalTurnThatNamesAnotherIssueReadsThatOne: the final turn corrects
// the issue; Answer reads the one it names.
func TestAFinalTurnThatNamesAnotherIssueReadsThatOne(t *testing.T) {
	r := newRig(t)
	voiceIssue(r)
	r.concierge.Heard(&r.exchange, "what about ABC-12")
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
