package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

type said struct {
	text string
	mode voice.SayMode
}

func recorder(into *[]said) voice.SayFunc {
	return func(_ context.Context, text string, mode voice.SayMode) error {
		*into = append(*into, said{text, mode})
		return nil
	}
}

func TestAnswerRunsToolsThenSpeaksTheFactSentenceBySentence(t *testing.T) {
	r := newRig(t)
	r.records.hits = []dispatch.SearchHit{{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Status: "in_progress"}}
	r.model.turns = [][]*genai.GenerateContentResponse{
		{chunk(thought("The room asks about voice work; search first.")), chunk(call("c1", "search_issues", map[string]any{"query": "voice work"}, "sig-1"))},
		{chunk(text("ABC-12 is in progress. It was updat")), chunk(text("ed two hours ago."))},
	}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with the voice work", recorder(&spoken)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(spoken) != 2 || spoken[0].text != "ABC-12 is in progress." || spoken[1].text != "It was updated two hours ago." {
		t.Fatalf("spoken = %+v", spoken)
	}
	for _, s := range spoken {
		if s.mode != voice.Fact {
			t.Errorf("%q spoken as %v, want fact", s.text, s.mode)
		}
	}
	if len(r.model.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(r.model.requests))
	}
	second := r.model.requests[1]
	modelTurn := second[len(second)-2]
	var echoed *genai.Part
	for _, part := range modelTurn.Parts {
		if part.FunctionCall != nil {
			echoed = part
		}
	}
	if modelTurn.Role != genai.RoleModel || echoed == nil || string(echoed.ThoughtSignature) != "sig-1" {
		t.Fatalf("the second call did not send back the model's call with its signature: %+v", modelTurn)
	}
	responseTurn := second[len(second)-1]
	if len(responseTurn.Parts) != 1 || responseTurn.Parts[0].FunctionResponse == nil || responseTurn.Parts[0].FunctionResponse.ID != "c1" || responseTurn.Parts[0].FunctionResponse.Name != "search_issues" {
		t.Fatalf("function response turn = %+v", responseTurn)
	}
	if _, ok := responseTurn.Parts[0].FunctionResponse.Response["issues"]; !ok {
		t.Errorf("the search result did not reach the model: %v", responseTurn.Parts[0].FunctionResponse.Response)
	}
	config := r.model.configs[0]
	if config.SystemInstruction == nil || len(config.Tools) != 1 || len(config.Tools[0].FunctionDeclarations) != len(ToolSpecs()) {
		t.Errorf("config = %+v", config)
	}
	if r.model.models[0] != "test-model" {
		t.Errorf("model = %q", r.model.models[0])
	}
}

func TestAnswerGivesTheModelTheTurnTheRecentTranscriptAndTheTime(t *testing.T) {
	r := newRig(t)
	r.exchange.Recent = []voice.TurnText{
		{Speaker: 0, Text: "we shipped the tap", At: testNow.Add(-time.Minute)},
		{Speaker: 1, Text: "Wispr, what about ABC-12", At: testNow.Add(-10 * time.Second)},
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Nothing new."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "what about ABC-12", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	first := r.model.requests[0]
	prompt := first[len(first)-1].Parts[0].Text
	for _, want := range []string{"Speaker 0: we shipped the tap", "Speaker 1: Wispr, what about ABC-12", "what about ABC-12", "2026-11-19 10:00 UTC"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestAnswerNeverSpeaksThoughts(t *testing.T) {
	r := newRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(thought("Secret plan. Do not say this.")), chunk(text("Here it is."))}}
	var spoken []said
	if err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken)); err != nil {
		t.Fatal(err)
	}
	if len(spoken) != 1 || spoken[0].text != "Here it is." {
		t.Fatalf("spoken = %+v", spoken)
	}
}

func TestTheSystemPromptHoldsTheRoomsRules(t *testing.T) {
	prompt := systemPrompt
	for _, want := range []string{
		"cannot identify", "read-only", "Never change anything", "ask_agent", "only when the record cannot answer",
		"one to three short sentences", "exactly as written", "I cannot reach Dispatch yet",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the system prompt lacks %q", want)
		}
	}
}

func TestAModelFailureSaysSoAndReturnsTheError(t *testing.T) {
	r := newRig(t)
	r.model.errs = []error{errors.New("vertex: 503")}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "x", recorder(&spoken))
	if err == nil {
		t.Fatal("Answer hid the model's failure")
	}
	if len(spoken) != 1 || spoken[0].mode != voice.Ack {
		t.Fatalf("spoken = %+v, want one apology", spoken)
	}
}

func TestAStoppedAnswerSaysNothingMore(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("First sentence. ")), chunk(text("Second sentence."))}}
	var spoken []said
	say := func(_ context.Context, text string, mode voice.SayMode) error {
		spoken = append(spoken, said{text, mode})
		cancel()
		return nil
	}
	err := r.concierge.Answer(ctx, &r.exchange, "x", say)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Answer after a stop = %v, want context.Canceled", err)
	}
	if len(spoken) != 1 {
		t.Fatalf("spoken after the stop = %+v", spoken)
	}
}

func TestTheAnswerToolReturnsWhatTheConciergeWouldSay(t *testing.T) {
	r := newRig(t)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is done. "), text("It closed today."))}}
	args, _ := json.Marshal(voice.AnswerArgs{Question: "is ABC-12 done", Transcript: "Speaker 0: is ABC-12 done"})
	raw, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{ID: "d1", Name: voice.AnswerTool, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	var result voice.AnswerResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Say != "ABC-12 is done. It closed today." {
		t.Fatalf("answer result = %+v", result)
	}
	prompt := r.model.requests[0][0].Parts[0].Text
	if !strings.Contains(prompt, "is ABC-12 done") || !strings.Contains(prompt, "Speaker 0: is ABC-12 done") {
		t.Errorf("the prompt lacks the question or the transcript:\n%s", prompt)
	}
}

func TestAReplyLandsOnItsTaskAndIsAnnouncedForTheEar(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "is the tap merged", "ABC-12")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("Yes. PR 80 merged the tap this morning."))}}

	err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "reply-1", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "Yes: `PR #80` merged the tap at 08:10 (see `internal/voice/tap.go`)."})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	task, err := r.store.VoiceTask("evt-1")
	if err != nil || task.State != storage.VoiceTaskDone || !strings.Contains(task.AnswerWritten, "internal/voice/tap.go") {
		t.Fatalf("task = %+v, %v; want done with the written answer", task, err)
	}
	select {
	case answer := <-r.concierge.Answers():
		if answer.TaskID != "evt-1" || answer.SessionID != "meeting-1" || answer.IssueKey != "ABC-12" || answer.Text != "Yes. PR 80 merged the tap this morning." {
			t.Fatalf("pending answer = %+v", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pending answer")
	}
	rewrite := r.model.requests[len(r.model.requests)-1]
	if !strings.Contains(rewrite[len(rewrite)-1].Parts[0].Text, "internal/voice/tap.go") {
		t.Errorf("the rewrite did not get the written answer")
	}

	if err := r.concierge.Spoken(context.Background(), "evt-1"); err != nil {
		t.Fatal(err)
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.SpokenAt == nil {
		t.Error("Spoken did not record spoken_at")
	}
}

func TestWhenTheRewriteFailsTheWrittenAnswerIsSpokenPlain(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "q", "")
	r.model.errs = []error{errors.New("vertex: 503")}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "**Done.** See `PR #80`."}); err != nil {
		t.Fatal(err)
	}
	select {
	case answer := <-r.concierge.Answers():
		if answer.Text != "Done. See PR #80." {
			t.Fatalf("plain answer = %q", answer.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pending answer")
	}
}

func TestAReplyWithoutInReplyToFindsTheOnlyWorkingTaskOfItsSender(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "q", "")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("pong"))}}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", Text: "pong"}); err != nil {
		t.Fatal(err)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskDone || task.AnswerWritten != "pong" {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

func TestAMessageThatMatchesNoTaskChangesNothing(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "q", "")
	messages := []envoy.Message{
		{EventID: "a", SourceSession: "agent-9", Text: "unrelated"},
		{EventID: "b", SourceSession: "agent-9", InReplyTo: "evt-1", Text: "a reply from a session that was not asked"},
		{EventID: "c", SourceSession: "agent-7", InReplyTo: "someone-elses-event", Text: "x"},
	}
	for i := range messages {
		if err := r.concierge.Deliver(context.Background(), &messages[i]); err != nil {
			t.Fatalf("Deliver(%s): %v", messages[i].EventID, err)
		}
	}
	if task, _ := r.store.VoiceTask("evt-1"); task.State != storage.VoiceTaskWorking || task.AnswerWritten != "" {
		t.Fatalf("task = %+v, want still working", task)
	}
	select {
	case answer := <-r.concierge.Answers():
		t.Fatalf("announced %+v", answer)
	default:
	}
}

func TestADeletedMeetingsReplyIsDropped(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "q", "")
	if err := r.store.EndSession("meeting-1", testNow, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.DeleteSession("meeting-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := r.concierge.Deliver(context.Background(), &envoy.Message{EventID: "r", SourceSession: "agent-7", InReplyTo: "evt-1", Text: "late"}); err != nil {
		t.Fatalf("Deliver after the delete: %v", err)
	}
	select {
	case answer := <-r.concierge.Answers():
		t.Fatalf("announced an answer of a deleted meeting: %+v", answer)
	case <-time.After(100 * time.Millisecond):
	}
}
