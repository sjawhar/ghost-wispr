package concierge

import (
	"context"
	"encoding/json"
	"errors"
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

// timedSaid is one sentence the voice was given, and when.
type timedSaid struct {
	said
	at time.Time
}

func timedRecorder(into *[]timedSaid, mu *sync.Mutex) voice.SayFunc {
	return func(_ context.Context, text string, mode voice.SayMode) error {
		mu.Lock()
		defer mu.Unlock()
		*into = append(*into, timedSaid{said{text, mode}, time.Now()})
		return nil
	}
}

// heldIssue is ABC-12, in progress, held by agent session agent-7, whose
// newest event is fresh unless stale is set.
func heldIssue(r *rig, holderKind string, stale bool) {
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "in_progress", UpdatedAt: testNow.Add(-2 * time.Hour)}
	switch holderKind {
	case "session":
		r.records.issues["ABC-12"].Claim = &dispatch.Claim{Actor: actor("session", "agent-7", "Worker")}
		r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	case "person":
		r.records.issues["ABC-12"].Claim = &dispatch.Claim{Actor: actor("user", "lead@example.com", "")}
	}
	newest := testNow.Add(-5 * time.Minute)
	if stale {
		newest = testNow.Add(-2 * time.Hour)
	}
	r.records.events["ABC-12"] = []dispatch.Event{{Type: "comment.created", CreatedAt: newest, Actor: actor("user", "lead@example.com", ""), Payload: json.RawMessage(`{"body":"PR 80 is in review."}`)}}
}

func answerTurn(t *testing.T, r *rig, turn string) []said {
	t.Helper()
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, turn, recorder(&spoken)); err != nil {
		t.Fatalf("Answer(%q): %v", turn, err)
	}
	return spoken
}

// TestOneClearMatch (c-find, R5): a search whose best issue scores at least
// 1.5 times the second is one clear match. Below that, the first sentence
// names the issue the Concierge took, by key and title; at 1.6 times it does
// not.
func TestOneClearMatch(t *testing.T) {
	for _, c := range []struct {
		ratio float64
		named bool
	}{{1.4, true}, {1.6, false}} {
		r := newRig(t)
		heldIssue(r, "none", false)
		r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Title: "Wake word", Status: "done"}
		r.records.hits = []dispatch.SearchHit{
			{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Status: "in_progress", Score: 0.006 * c.ratio},
			{Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Status: "done", Score: 0.006},
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		spoken := answerTurn(t, r, "what is happening with the realtime voice")
		if len(spoken) == 0 {
			t.Fatalf("ratio %v: nothing said", c.ratio)
		}
		first := spoken[0].text
		named := strings.Contains(first, "ABC-12") && strings.Contains(first, "Realtime voice")
		if named != c.named {
			t.Errorf("ratio %v: first sentence %q names the key and title: %v, want %v", c.ratio, first, named, c.named)
		}
	}
}

// TestNoSearchResult (c-find, R5): a search with no result makes the voice
// say it found no issue and ask for the key, with no model call.
func TestNoSearchResult(t *testing.T) {
	r := newRig(t)
	r.records.hits = nil
	spoken := answerTurn(t, r, "what is happening with the purple submarine")
	if len(spoken) != 1 || spoken[0].text != noIssueFound || !strings.Contains(spoken[0].text, "key") {
		t.Fatalf("spoken = %+v, want the no-issue sentence that asks for the key", spoken)
	}
	if len(r.model.requests) != 0 {
		t.Fatalf("the model was called %d times", len(r.model.requests))
	}
}

// TestFastAnswerFactsHaveSourcesAndAges (c-fast-answer, R6): read_issue
// takes 10 events, each with its age, and a fact the record does not hold is
// never spoken: the model writes once more, then the voice says it can only
// say what the record holds.
func TestFastAnswerFactsHaveSourcesAndAges(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(text("PR 81 merged an hour ago."))},
		{chunk(text("PR 81 is merged."))},
	}
	spoken := answerTurn(t, r, "what is happening with ABC-12")
	for _, s := range spoken {
		if strings.Contains(s.text, "81") {
			t.Fatalf("a pull request the record does not hold was spoken: %+v", spoken)
		}
	}
	if last := spoken[len(spoken)-1]; last.text != notInRecord {
		t.Fatalf("spoken = %+v, want the record-only sentence last", spoken)
	}
	if limits := r.records.eventLimits(); len(limits) == 0 || limits[0] != 10 {
		t.Errorf("read_issue asked for %v events, want 10", limits)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, `"updated_ago":"2 hours ago"`) || !strings.Contains(prompt, `"when":"5 minutes ago"`) {
		t.Errorf("the record lacks the ages:\n%s", prompt)
	}

	r = newRig(t)
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review, as of five minutes ago."))}}
	spoken = answerTurn(t, r, "what is happening with ABC-12")
	if last := spoken[len(spoken)-1]; last.text != "PR 80 is in review, as of five minutes ago." {
		t.Fatalf("a fact the record holds was not spoken: %+v", spoken)
	}
}

// TestFastAnswerOrder (c-fast-answer, R6): the answer gives the issue search
// chose, then the holder's case, then the facts.
func TestFastAnswerOrder(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.007}, {Kind: "issue", IssueKey: "ABC-13", Title: "Wake word", Score: 0.006}}
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13"}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	spoken := answerTurn(t, r, "what is happening with the realtime voice")
	if len(spoken) != 3 {
		t.Fatalf("spoken = %+v, want the issue, the holder and a fact", spoken)
	}
	if !strings.Contains(spoken[0].text, "Realtime voice") || !strings.Contains(spoken[1].text, "live agent session") || spoken[2].text != "PR 80 is in review." {
		t.Fatalf("spoken out of order: %+v", spoken)
	}
	if len(r.agents.sent) != 0 {
		t.Errorf("a status question to a fresh record asked the holder")
	}
}

// TestRoutingLineAt1500ms (c-fast-answer, R6): with no fact to say 1.5 s
// after the turn, the voice says one routing line.
func TestRoutingLineAt1500ms(t *testing.T) {
	if routingAfter != 1500*time.Millisecond {
		t.Fatalf("routingAfter = %v, want 1.5 s", routingAfter)
	}
	r := newRig(t)
	heldIssue(r, "none", false)
	r.records.block, r.records.blockKey = make(chan struct{}), "ABC-12"
	go func() {
		time.Sleep(2200 * time.Millisecond)
		close(r.records.block)
	}()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var mu sync.Mutex
	var spoken []timedSaid
	start := time.Now()
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12", timedRecorder(&spoken, &mu)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spoken) < 2 || spoken[0].text != "Reading ABC-12." || spoken[0].mode != voice.Ack {
		t.Fatalf("spoken = %+v, want the routing line first", spoken)
	}
	if at := spoken[0].at.Sub(start); at < 1450*time.Millisecond || at > 1900*time.Millisecond {
		t.Errorf("the routing line came %v after the turn, want 1.5 s", at)
	}
	for _, s := range spoken[1:] {
		if s.mode == voice.Ack {
			t.Errorf("a second routing line: %+v", spoken)
		}
	}
}

// TestConciergeBounds (c-fast-answer, R6): one fast answer makes at most 6
// tool calls of the model's and takes at most 8 s.
func TestConciergeBounds(t *testing.T) {
	if maxToolCalls != 6 || fastAnswerBound != 8*time.Second {
		t.Fatalf("bounds = %d calls, %v; want 6 and 8 s", maxToolCalls, fastAnswerBound)
	}
	r := newRig(t)
	heldIssue(r, "none", false)
	for range 10 {
		r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(
			call("a", "read_issue", map[string]any{"key": "ABC-12"}, ""),
			call("b", "read_issue", map[string]any{"key": "ABC-12"}, ""),
		)})
	}
	var spoken []said
	_ = r.concierge.Answer(context.Background(), &r.exchange, "what is it", recorder(&spoken))
	responses := 0
	for _, request := range r.model.requests {
		for _, part := range request[len(request)-1].Parts {
			if part.FunctionResponse != nil && part.FunctionResponse.Response["error"] == nil {
				responses++
			}
		}
	}
	if responses > 6 {
		t.Fatalf("the model's tools ran %d times, want at most 6", responses)
	}

	r = newRig(t)
	r.concierge.fastBound = 300 * time.Millisecond
	r.model.block = make(chan struct{})
	defer close(r.model.block)
	start := time.Now()
	_ = r.concierge.Answer(context.Background(), &r.exchange, "what is it", recorder(&spoken))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("an answer whose model never answers took %v, want the bound", took)
	}
}

// TestWhenToAskTheHolder (c-ask-holder, R7): the Concierge asks a live holder
// when the person says "ask", when the question is not about status, and when
// the record has no event in the last 60 minutes; it does not for a status
// question on a fresh record, nor for one the record answers (who holds it,
// its priority, its assignee), nor for an issue a search took without a
// clear match, which it says. "Asks" as a noun is no "ask".
func TestWhenToAskTheHolder(t *testing.T) {
	for _, c := range []struct {
		name, turn string
		stale      bool
		search     bool
		asked      string
	}{
		{"status on a fresh record", "what is happening with ABC-12", false, false, ""},
		{"ask", "ask the agent on ABC-12 whether the tap is merged", false, false, "whether the tap is merged"},
		{"not about status", "is the tap on ABC-12 merged", false, false, "is the tap on ABC-12 merged"},
		{"a stale record", "what is happening with ABC-12", true, false, "what is happening with ABC-12"},
		{"a search with no clear match", "ask the agent on the realtime voice whether the tap is merged", false, true, ""},
		{"asks as a noun", "what is the status of the open asks on ABC-12", false, false, ""},
		{"who holds it", "who holds ABC-12", false, false, ""},
		{"its priority", "what is the priority of ABC-12", false, false, ""},
		{"its assignee", "is ABC-12 assigned to anyone", false, false, ""},
		{"where, not about status", "where does ABC-12 keep the session tokens", false, false, "where does ABC-12 keep the session tokens"},
		{"state, not about status", "what state does the parser of ABC-12 keep", false, false, "what state does the parser of ABC-12 keep"},
		{"new, not about status", "is the new retry on ABC-12 safe to run twice", false, false, "is the new retry on ABC-12 safe to run twice"},
	} {
		r := newRig(t)
		heldIssue(r, "session", c.stale)
		if c.search {
			r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Score: 0.007}, {Kind: "issue", IssueKey: "ABC-13", Score: 0.006}}
			r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13"}
		}
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		spoken := answerTurn(t, r, c.turn)
		if c.asked == "" {
			if len(r.agents.sent) != 0 {
				t.Errorf("%s: asked %+v", c.name, r.agents.sent)
			}
			if withheld := spoken[len(spoken)-1].text == askWithheld; withheld != c.search {
				t.Errorf("%s: spoken = %+v; want the withheld ask said %v", c.name, spoken, c.search)
			}
			continue
		}
		if len(r.agents.sent) != 1 || r.agents.sent[0].Target != "agent-7" || !strings.Contains(r.agents.sent[0].Message, ": "+c.asked+"\n") {
			t.Errorf("%s: sent %+v, want %q to agent-7", c.name, r.agents.sent, c.asked)
			continue
		}
		if last := spoken[len(spoken)-1].text; !strings.Contains(last, "asked the agent on ABC-12") {
			t.Errorf("%s: spoken = %+v, want the asked line last", c.name, spoken)
		}
	}
}

// TestTheAskRunsBesideTheFacts (c-fast-answer, R6): the ask runs beside the
// facts, so a slow Envoy holds no fact of the record back.
func TestTheAskRunsBesideTheFacts(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	factSaid := make(chan struct{})
	var once sync.Once
	r.agents.onSend = func(context.Context, string) error {
		select {
		case <-factSaid:
			return nil
		case <-time.After(3 * time.Second):
			return errors.New("envoy: no answer in 3 s")
		}
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	var mu sync.Mutex
	var spoken []said
	say := func(_ context.Context, text string, mode voice.SayMode) error {
		mu.Lock()
		spoken = append(spoken, said{text, mode})
		mu.Unlock()
		if text == "PR 80 is in review." {
			once.Do(func() { close(factSaid) })
		}
		return nil
	}
	if err := r.concierge.Answer(context.Background(), &r.exchange, "is the tap on ABC-12 merged", say); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := spoken[len(spoken)-1].text; !strings.Contains(last, "asked the agent on ABC-12") {
		t.Fatalf("spoken = %+v, want the facts while the ask was sent, then the asked line", spoken)
	}
}

// TestTheRoutingLineOnlyWhenNothingWasSaid (c-fast-answer, R6): a holder's
// case said before 1.5 s keeps the routing line silent, however long the
// facts then take.
func TestTheRoutingLineOnlyWhenNothingWasSaid(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "person", false)
	r.model.block = make(chan struct{})
	go func() {
		time.Sleep(2200 * time.Millisecond)
		close(r.model.block)
	}()
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
	for _, s := range answerTurn(t, r, "what is happening with ABC-12") {
		if s.mode == voice.Ack {
			t.Fatalf("a routing line after the holder's case was said: %q", s.text)
		}
	}
}

// TestSpeechDoesNotSpendTheAnswersBound (c-fast-answer, R6): a voice's Say
// returns once a sentence's audio is queued, at playback speed. The answer's
// 8 s bound the Concierge's work, not the room's listening: the model starts
// while the holder's case still plays, and a slow voice cuts no fact. Run 7
// of the end-to-end test lost every fast answer this way.
func TestSpeechDoesNotSpendTheAnswersBound(t *testing.T) {
	r := newRig(t)
	r.concierge.fastBound = time.Second
	heldIssue(r, "none", false)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review. It is in review."))}}
	var mu sync.Mutex
	var modelAt, leadPlayed time.Time
	r.model.onCall = func(int) {
		mu.Lock()
		defer mu.Unlock()
		if modelAt.IsZero() {
			modelAt = time.Now()
		}
	}
	var spoken []string
	say := func(ctx context.Context, text string, _ voice.SayMode) error {
		select {
		case <-time.After(600 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		mu.Lock()
		defer mu.Unlock()
		spoken = append(spoken, text)
		if len(spoken) == 1 {
			leadPlayed = time.Now()
		}
		return nil
	}
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with ABC-12", say); err != nil {
		t.Fatalf("Answer with a voice that plays 600 ms a sentence and a 1 s bound: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spoken) != 3 {
		t.Fatalf("spoken = %q, want the holder's case and both facts", spoken)
	}
	if !modelAt.Before(leadPlayed) {
		t.Fatalf("the model started %v after the holder's case played, want before", modelAt.Sub(leadPlayed))
	}
}

// TestAskAgentMessage (c-ask-holder, R7): the message holds the question's
// words, the pointer and the read-only line, and nothing else; a retry of the
// same question reuses its idempotency key.
func TestAskAgentMessage(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	r.agents.sendErr = errors.New("envoy: 503")
	args := map[string]string{"session": "agent-7", "question": "is the tap merged yet", "issue_key": "ABC-12"}
	if result := r.run(t, "ask_agent", args); result["error"] == nil {
		t.Fatalf("control: a failed send answered %v", result)
	}
	r.agents.sendErr = nil
	r.run(t, "ask_agent", args)
	if len(r.agents.attempts) != 2 || len(r.agents.sent) != 1 {
		t.Fatalf("attempts %d, sent %d; want 2 and 1", len(r.agents.attempts), len(r.agents.sent))
	}
	if first, retry := r.agents.attempts[0].IdempotencyKey, r.agents.attempts[1].IdempotencyKey; first == "" || first != retry {
		t.Fatalf("idempotency keys %q then %q, want one key reused", first, retry)
	}
	sent := r.agents.sent[0]
	if sent.Target != "agent-7" || sent.NoReply {
		t.Fatalf("send = %+v, want a reply asked of agent-7", sent)
	}
	want := "From the office meeting recorder (Ghost Wispr), on behalf of someone in the room it cannot identify.\n" +
		"This is a read-only question. Make no change because of it unless a person confirms the change in Dispatch.\n\n" +
		"Question about ABC-12: is the tap merged yet\n\n" +
		"Asked in Ghost Wispr session meeting-1, 09:59:52 to 10:00:00 UTC (19:52 to 20:00 into the session's recording). Its transcript, which honours deletions: https://unit.example/api/sessions/meeting-1\n\n" +
		"Reply to this message (envoy_send with in_reply_to) in three sentences or fewer. Your reply is read aloud in the room."
	if sent.Message != want {
		t.Fatalf("message =\n%s\nwant\n%s", sent.Message, want)
	}
}

// TestARepeatedQuestionKeepsItsAnswer (c-ask-holder, R7): Envoy stores
// nothing for a key it already holds, and answers with an event id that
// names no message.
//   - The same question again in one exchange, while the first waits, sends
//     nothing and replaces nothing.
//   - A retry after a send that Envoy stored but did not confirm reuses its
//     key; its task keeps its local id, and the holder's reply to the event
//     the unit never learned gets the request to reply again, whose reply
//     lands on the task.
//   - A new question after a replace gets a key of its own, so it reaches
//     the holder.
func TestARepeatedQuestionKeepsItsAnswer(t *testing.T) {
	ask := map[string]string{"session": "agent-7", "question": "is the tap merged"}
	r := newRig(t)
	r.name(t, "agent-7")
	first := r.run(t, "ask_agent", ask)
	again := r.run(t, "ask_agent", ask)
	if len(r.agents.attempts) != 1 || again["task_id"] != first["task_id"] {
		t.Fatalf("the same question twice: attempts %d, tasks %v then %v; want one send and one task", len(r.agents.attempts), first["task_id"], again["task_id"])
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskWorking {
		t.Fatalf("the first task = %+v, %v; want working", task, err)
	}
	r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "who reviews it"})
	r.run(t, "ask_agent", ask)
	asked := 0
	for _, m := range r.agents.sent {
		if !m.NoReply && strings.Contains(m.Message, ": is the tap merged\n") {
			asked++
		}
	}
	if asked != 2 {
		t.Fatalf("the question reached the holder %d times; want twice, the second after its replace", asked)
	}

	r = newRig(t)
	r.name(t, "agent-7")
	r.agents.landedErr = errors.New("envoy: context deadline exceeded")
	if result := r.run(t, "ask_agent", ask); result["error"] != questionNotSent {
		t.Fatalf("control: a send that failed answered %v", result)
	}
	r.agents.landedErr = nil
	result := r.run(t, "ask_agent", ask)
	if keys := r.agents.attempts; len(keys) != 2 || keys[0].IdempotencyKey != keys[1].IdempotencyKey {
		t.Fatalf("attempts %+v, want one key reused", keys)
	}
	taskID, _ := result["task_id"].(string)
	if strings.HasPrefix(taskID, "unstored-") || taskID == "" {
		t.Fatalf("the retry's task is %q, an event Envoy never stored", taskID)
	}
	if task, err := r.store.VoiceTask(taskID); err != nil || task.State != storage.VoiceTaskWorking {
		t.Fatalf("the retry's task = %+v, %v; want working", task, err)
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "reply-1", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Yes."}); err != nil {
		t.Fatal(err)
	}
	if len(r.agents.sent) != 2 || r.agents.sent[1].InReplyTo != "reply-1" {
		t.Fatalf("sent %+v; want the request to reply again, in reply to reply-1", r.agents.sent)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Yes."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "reply-2", SourceSession: "agent-7", InReplyTo: "evt-2", Text: "Yes."}); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask(taskID); task.State != storage.VoiceTaskDone || task.AnswerWritten != "Yes." {
		t.Fatalf("the task after the reply = %+v, want done with the answer", task)
	}
}

// TestNothingSentToAPersonOrNone (c-ask-holder, R7): with {person} or
// {none}, the Concierge sends nothing, even on "ask", and answers from the
// record.
func TestNothingSentToAPersonOrNone(t *testing.T) {
	for _, holder := range []string{"person", "none"} {
		r := newRig(t)
		heldIssue(r, holder, false)
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("PR 80 is in review."))}}
		spoken := answerTurn(t, r, "ask the agent on ABC-12 whether the tap is merged")
		if len(r.agents.sent) != 0 {
			t.Errorf("%s: sent %+v", holder, r.agents.sent)
		}
		if spoken[len(spoken)-1].text != "PR 80 is in review." {
			t.Errorf("%s: spoken = %+v, want the record's fact", holder, spoken)
		}
	}
}

// TestLedgerStates (c-ask-holder, R7): a task goes working to done on its
// answer, to cancelled on "Wispr, never mind", and to replaced on a new
// question to the same holder in the same exchange; a cancel and a replace
// each send the holder a notice. A question in another exchange replaces
// nothing.
func TestLedgerStates(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "first", "")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r1", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskDone {
		t.Fatalf("answered task = %s, want done", task.State)
	}

	r.ask(t, "agent-7", "second", "")
	r.ask(t, "agent-7", "third", "")
	if task, _ := r.store.VoiceTask("evt-2"); task.State != storage.VoiceTaskReplaced {
		t.Fatalf("the earlier question of the exchange = %s, want replaced", task.State)
	}
	notices := 0
	for _, sent := range r.agents.sent {
		if sent.NoReply {
			notices++
			if sent.InReplyTo != "evt-2" || !strings.Contains(sent.Message, "replaces it") {
				t.Errorf("replace notice = %+v", sent)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("%d notices after a replace, want 1", notices)
	}

	other := r.exchange
	other.WakeAt = other.WakeAt.Add(time.Minute)
	r.records.issues["OTHER-1"] = claimed("OTHER-1", "session", "agent-7", "Worker")
	tools := r.concierge.Tools(&other)
	raw, _ := json.Marshal(map[string]string{"key": "OTHER-1"})
	if _, err := tools.Run(context.Background(), voice.ToolCall{Name: "issue_holder", Args: raw}); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]string{"session": "agent-7", "question": "fourth"})
	if _, err := tools.Run(context.Background(), voice.ToolCall{Name: "ask_agent", Args: raw}); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-5"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("the question of another exchange replaced: evt-5 %s", task.State)
	}
	if task, _ := r.store.VoiceTask("evt-3"); task.State != storage.VoiceTaskWorking {
		t.Fatalf("a question of another exchange replaced the earlier exchange's: evt-3 %s", task.State)
	}

	if err := r.concierge.NeverMind(context.Background(), "meeting-1"); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-5"); task.State != storage.VoiceTaskCancelled {
		t.Fatalf("never mind left the newest task %s", task.State)
	}
}

// TestLedgerStoresNoRoomWords (c-ask-holder, R7): after a question and its
// answer, no column of voice_tasks holds the question's words, and only
// answer_written holds text.
func TestLedgerStoresNoRoomWords(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "zebra quartz pineapple", "ABC-12")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Mango."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Mango."}); err != nil {
		t.Fatal(err)
	}
	row := r.store.DB().QueryRow(`SELECT id, session_id, asked_at, issue_key, holder, state, answer_written, COALESCE(spoken_at, '') FROM voice_tasks`)
	var columns [8]string
	pointers := make([]any, len(columns))
	for i := range columns {
		pointers[i] = &columns[i]
	}
	if err := row.Scan(pointers...); err != nil {
		t.Fatal(err)
	}
	names := []string{"id", "session_id", "asked_at", "issue_key", "holder", "state", "answer_written", "spoken_at"}
	for i, value := range columns {
		if strings.Contains(value, "zebra") || strings.Contains(value, "quartz") {
			t.Errorf("%s holds the question's words: %q", names[i], value)
		}
		if names[i] != "answer_written" && strings.Contains(value, "Mango") {
			t.Errorf("%s holds the answer's text", names[i])
		}
	}
	if columns[6] != "Mango." {
		t.Errorf("answer_written = %q", columns[6])
	}
}

// TestIssueHolderIsTheToolsAnswer: IssueHolder, for the acceptance driver,
// answers exactly what the issue_holder tool answers.
func TestIssueHolderIsTheToolsAnswer(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false)
	got, err := r.concierge.IssueHolder(context.Background(), "ABC-12")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"key": "ABC-12"})
	want, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{Name: "issue_holder", Args: raw})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("IssueHolder = %s, want the tool's %s", got, want)
	}
}
