package concierge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAFailedSearchLogsNoRoomWords: a search Dispatch never answers is
// logged, and the log holds none of the search's words, which come from the
// room. A session delete cannot reach the log.
func TestAFailedSearchLogsNoRoomWords(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	client, err := dispatch.NewClient("http://127.0.0.1:1", "test-token", &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.concierge.records = client
	result := r.run(t, "search_issues", map[string]string{"query": "private room words"})
	if result["error"] != cannotReachDispatch {
		t.Fatalf("control: a search of a closed port = %v", result)
	}
	if !strings.Contains(logs.String(), "concierge tool failed") {
		t.Fatalf("control: the failure was not logged:\n%s", logs)
	}
	if strings.Contains(logs.String(), "private") {
		t.Fatalf("the log holds the room's words:\n%s", logs)
	}
}

// TestAModelRefusalLogsNoRoomWords: a model error that quotes the prompt
// back reaches neither the log nor the error the Supervisor gets.
func TestAModelRefusalLogsNoRoomWords(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	r.model.errs = []error{genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "the prompt holds private room words"}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is it", recorder(&spoken))
	if err == nil {
		t.Fatal("control: the model's refusal was hidden")
	}
	if strings.Contains(err.Error(), "private") {
		t.Errorf("the error holds the room's words: %v", err)
	}
	if !strings.Contains(logs.String(), "concierge could not answer") {
		t.Fatalf("control: the failure was not logged:\n%s", logs)
	}
	if strings.Contains(logs.String(), "private") {
		t.Fatalf("the log holds the room's words:\n%s", logs)
	}
}

// TestAReplyThatRacesItsQuestionComesAgain: a reply that lands while its
// question is still being sent names an event the ledger does not hold yet.
// It is answered with an error, so Envoy delivers it again, and the second
// delivery lands on the task.
func TestAReplyThatRacesItsQuestionComesAgain(t *testing.T) {
	r := newRig(t)
	reply := &envoy.Message{EventID: "reply-1", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "It merged."}
	var early error
	r.agents.onSend = func(_ context.Context, eventID string) error {
		if eventID == "evt-1" {
			early = r.concierge.Deliver(context.Background(), reply)
		}
		return nil
	}
	if result := r.ask(t, "agent-7", "is it merged", ""); result["task_id"] != "evt-1" {
		t.Fatalf("ask_agent = %v", result)
	}
	if early == nil {
		t.Fatal("a reply that raced its question was taken as done, and would never come again")
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It merged."))}}
	if err := r.concierge.Deliver(context.Background(), reply); err != nil {
		t.Fatalf("the reply's second delivery: %v", err)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskDone {
		t.Fatalf("task = %+v, %v; want done", task, err)
	}
}

// failingRekey is the ledger with a rekey that fails.
type failingRekey struct{ *storage.SQLiteStore }

func (failingRekey) RekeyVoiceTask(string, string) error { return errors.New("database is locked") }

// TestAReplyFindsATaskWhoseRekeyFailed: when the task keeps its local id,
// the reply that names the question's event still finds it, and so does
// task_status.
func TestAReplyFindsATaskWhoseRekeyFailed(t *testing.T) {
	r := newRig(t)
	r.concierge.store = failingRekey{r.store}
	if result := r.ask(t, "agent-7", "is it merged", ""); result["task_id"] != "evt-1" {
		t.Fatalf("ask_agent = %v, want the event id", result)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("It merged."))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "It merged."}); err != nil {
		t.Fatal(err)
	}
	if status := r.run(t, "task_status", map[string]string{"id": "evt-1"}); status["state"] != "done" || status["answer"] != "It merged." {
		t.Fatalf("task_status = %v, want done with the answer", status)
	}
}

// TestAStopDuringTheSendKeepsTheQuestion: the Supervisor cancels the turn
// while the question is on its way. The send has its own deadline, so the
// question goes and its task stays, for the answer that comes later.
func TestAStopDuringTheSendKeepsTheQuestion(t *testing.T) {
	r := newRig(t)
	r.name(t, "agent-7")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var deadline time.Duration
	r.agents.onSend = func(sendCtx context.Context, _ string) error {
		stop()
		if until, ok := sendCtx.Deadline(); ok {
			deadline = time.Until(until)
		}
		return sendCtx.Err()
	}
	raw, _ := json.Marshal(map[string]string{"session": "agent-7", "question": "q"})
	if _, err := r.concierge.Tools(&r.exchange).Run(ctx, voice.ToolCall{Name: "ask_agent", Args: raw}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after the stop = %v, want context.Canceled", err)
	}
	if deadline <= 0 || deadline > sendTimeout {
		t.Errorf("the send's deadline is %v away, want at most %v", deadline, sendTimeout)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskWorking {
		t.Fatalf("task = %+v, %v; want the sent question kept, working", task, err)
	}
}

// TestATurnThatSaysNothingSpeaksTheFallback: a model turn with no words
// (blocked, or only thoughts) says sorry and reports a failure.
func TestATurnThatSaysNothingSpeaksTheFallback(t *testing.T) {
	r := newRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(thought("Nothing to say.")), {Candidates: nil}}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken))
	if err == nil {
		t.Error("a turn with no words reported success")
	}
	if len(spoken) != 1 || spoken[0].text != sorry || spoken[0].mode != voice.Ack {
		t.Fatalf("spoken = %+v, want the fallback line", spoken)
	}
}

// TestTheModelAsksNoAgent: the model of a fast answer cannot ask an agent:
// ask_agent is not among its tools, a call of it is refused, and the round
// after its last allowed tool call may call none. The Concierge asks the
// holder itself (R7).
func TestTheModelAsksNoAgent(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = claimed("ABC-12", "session", "agent-7", "Worker")
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(call("c", "ask_agent", map[string]any{"session": "agent-7", "question": "q"}, ""))})
	for range maxToolCalls + 1 {
		r.model.turns = append(r.model.turns, []*genai.GenerateContentResponse{chunk(call("c", "issue_holder", map[string]any{"key": "ABC-12"}, ""))})
	}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken)); err == nil {
		t.Error("a turn that ran out of tool calls with no words reported success")
	}
	if len(r.agents.sent) != 0 {
		t.Fatalf("the model sent %d questions", len(r.agents.sent))
	}
	for _, declaration := range r.model.configs[0].Tools[0].FunctionDeclarations {
		if declaration.Name == "ask_agent" {
			t.Fatal("the fast answer's model is given ask_agent")
		}
	}
	last := r.model.configs[len(r.model.configs)-1]
	if last.ToolConfig == nil || last.ToolConfig.FunctionCallingConfig == nil || last.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone {
		t.Errorf("the round after the last tool call lets the model call tools: %+v", last.ToolConfig)
	}
	if len(spoken) != 1 || spoken[0].text != sorry {
		t.Fatalf("spoken = %+v", spoken)
	}
}

// TestNoAnnouncementForASessionDeletedDuringTheRewrite: the meeting is
// deleted while its answer is rewritten for the ear; the answer is not
// announced.
func TestNoAnnouncementForASessionDeletedDuringTheRewrite(t *testing.T) {
	r := newRig(t)
	r.ask(t, "agent-7", "q", "")
	deleted := make(chan error, 1)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Done."))}}
	r.model.onCall = func(int) {
		if err := r.store.EndSession("meeting-1", testNow, "", ""); err != nil {
			deleted <- err
			return
		}
		_, err := r.store.DeleteSession("meeting-1", t.TempDir())
		deleted <- err
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Done."}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("delete during the rewrite: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rewrite never ran")
	}
	if _, err := r.store.VoiceTask("evt-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("control: the task outlived its session: %v", err)
	}
	select {
	case answer := <-r.concierge.Answers():
		t.Fatalf("announced an answer of a deleted meeting: %+v", answer)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAskAgentAsksOnlyASessionThisExchangeWasTold: the model may ask only a
// session that issue_holder named in this exchange, so room speech, an
// issue's text or an agent's answer cannot steer a question to any session.
func TestAskAgentAsksOnlyASessionThisExchangeWasTold(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	ask := map[string]string{"session": "agent-7", "question": "q"}
	if result := r.run(t, "ask_agent", ask); result["error"] == nil || len(r.agents.sent) != 0 {
		t.Fatalf("asked a session nobody named: %v", result)
	}
	r.name(t, "agent-7")
	if result := r.run(t, "ask_agent", ask); result["error"] != nil || len(r.agents.sent) != 1 {
		t.Fatalf("ask_agent of a named session = %v", result)
	}
	other := r.exchange
	other.WakeAt = other.WakeAt.Add(time.Minute)
	raw, _ := json.Marshal(ask)
	result, err := r.concierge.Tools(&other).Run(context.Background(), voice.ToolCall{Name: "ask_agent", Args: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "error") || len(r.agents.sent) != 1 {
		t.Fatalf("another exchange asked a session it was never told: %s", result)
	}
}

// TestALocalFailureIsNotBlamedOnDispatch: a failure on the recorder itself
// does not make the room hear that Dispatch is out of reach.
func TestALocalFailureIsNotBlamedOnDispatch(t *testing.T) {
	r := newRig(t)
	r.exchange.SessionID = "no-such-meeting"
	r.name(t, "agent-7")
	result := r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "q"})
	if result["error"] == nil || result["error"] == cannotReachDispatch {
		t.Fatalf("a failure to record the question = %v", result)
	}
}

// TestEmptyStreamedPartsDoNotGoBackToTheModel: Vertex streams an empty part
// at the end of a turn and refuses it when it is sent back ("required oneof
// field 'data' must have one initialized field", seen live 2026-11-19). A
// signature-only part keeps its signature on the part before it.
func TestEmptyStreamedPartsDoNotGoBackToTheModel(t *testing.T) {
	r := newRig(t)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12"}}
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(call("c1", "search_issues", map[string]any{"query": "voice"}, "")), chunk(&genai.Part{ThoughtSignature: []byte("sig-1")}), chunk(&genai.Part{})},
		{chunk(text("ABC-12 is open."))},
	}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	second := r.model.requests[1]
	modelTurn := second[len(second)-2]
	if len(modelTurn.Parts) != 1 {
		t.Fatalf("the model turn sent back has %d parts, want the call alone: %+v", len(modelTurn.Parts), modelTurn.Parts)
	}
	if part := modelTurn.Parts[0]; part.FunctionCall == nil || string(part.ThoughtSignature) != "sig-1" {
		t.Fatalf("the call went back as %+v, want it with the signature", part)
	}
}
