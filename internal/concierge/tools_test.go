package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

func TestToolSpecsDeclareTheSixTools(t *testing.T) {
	want := map[string][]string{
		"search_issues":      {"query"},
		"read_issue":         {"key"},
		"issue_holder":       {"key"},
		"ask_agent":          {"session", "question"},
		"task_status":        {"id"},
		"list_joined_agents": nil,
	}
	specs := ToolSpecs()
	if len(specs) != len(want) {
		t.Fatalf("ToolSpecs has %d tools, want %d", len(specs), len(want))
	}
	for _, spec := range specs {
		required, ok := want[spec.Name]
		if !ok {
			t.Errorf("unexpected tool %q", spec.Name)
			continue
		}
		if spec.Description == "" || spec.Parameters["type"] != "object" {
			t.Errorf("%s: no description or no object schema: %+v", spec.Name, spec)
		}
		got, _ := spec.Parameters["required"].([]string)
		if !slices.Equal(got, required) {
			t.Errorf("%s requires %q, want %q", spec.Name, got, required)
		}
		properties, _ := spec.Parameters["properties"].(map[string]any)
		for _, name := range required {
			if _, ok := properties[name]; !ok {
				t.Errorf("%s: required %q has no property", spec.Name, name)
			}
		}
	}
	if _, err := json.Marshal(specs); err != nil {
		t.Fatalf("the specs do not encode: %v", err)
	}
}

func TestAnUnknownToolIsAnError(t *testing.T) {
	r := newRig(t)
	if _, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{Name: "delete_issue", Args: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("an unknown tool ran")
	}
}

func TestSearchIssuesAnswersEachIssueOnce(t *testing.T) {
	r := newRig(t)
	r.records.hits = []dispatch.SearchHit{
		{Kind: "issue", IssueKey: "ABC-12", Title: "Realtime voice", Status: "backlog", Snippet: "realtime voice"},
		{Kind: "document", IssueKey: "ABC-12", Title: "Realtime voice", Status: "backlog", Document: "spec", Snippet: "the spec"},
		{Kind: "comment", Snippet: "no issue"},
		{Kind: "issue", IssueKey: "ABC-9", Title: "Wake word", Status: "done"},
	}
	result := r.run(t, "search_issues", map[string]string{"query": "realtime voice"})
	issues, _ := result["issues"].([]any)
	if len(issues) != 2 {
		t.Fatalf("issues = %v, want ABC-12 and ABC-9", result)
	}
	first, _ := issues[0].(map[string]any)
	if first["key"] != "ABC-12" || first["title"] != "Realtime voice" || first["status"] != "backlog" {
		t.Errorf("first issue = %v", first)
	}
	if len(r.records.queries) != 1 || r.records.queries[0] != "realtime voice" {
		t.Errorf("queries = %q", r.records.queries)
	}
	if result := r.run(t, "search_issues", map[string]string{"query": "  "}); result["error"] == nil {
		t.Errorf("an empty query ran: %v", result)
	}
}

func TestReadIssueGivesStatusSpecSummaryAndRecentEvents(t *testing.T) {
	r := newRig(t)
	updated := testNow.Add(-2 * time.Hour)
	issue := &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "in_progress", Assignee: "lead@example.com", UpdatedAt: updated, OpenAsks: 1, PrimaryArtifactID: "doc-1"}
	issue.Artifacts = append(issue.Artifacts, struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}{ID: "doc-1", Slug: "spec"})
	r.records.issues["ABC-12"] = issue
	r.records.docs["ABC-12/spec"] = "## Summary\n\nBuild a voice for meetings.\n\n" + strings.Repeat("More detail. ", 400)
	r.records.events["ABC-12"] = []dispatch.Event{
		{Type: "comment.created", CreatedAt: testNow.Add(-30 * time.Minute), Actor: actor("session", "s-2", "Worker on ABC-12"), Payload: json.RawMessage(`{"body":"PR 80 merged; the tap is next."}`)},
		{Type: "ask.answered", CreatedAt: testNow.Add(-3 * time.Hour), Actor: actor("user", "lead@example.com", ""), Payload: json.RawMessage(`{"question":"Two levels?","answer":{"selected":["Two levels"],"text":null}}`)},
	}

	result := r.run(t, "read_issue", map[string]string{"key": "abc-12"})
	if result["key"] != "ABC-12" || result["status"] != "in_progress" || result["updated_ago"] != "2 hours ago" || result["open_asks"] != float64(1) {
		t.Errorf("issue = %v", result)
	}
	spec, _ := result["spec_summary"].(string)
	if !strings.HasPrefix(spec, "## Summary\n\nBuild a voice for meetings.") || len(spec) > specSummaryChars+3 {
		t.Errorf("spec summary = %d chars, starting %.60q", len(spec), spec)
	}
	events, _ := result["recent_events"].([]any)
	if len(events) != 2 {
		t.Fatalf("recent events = %v", result["recent_events"])
	}
	first, _ := events[0].(map[string]any)
	if first["type"] != "comment.created" || first["when"] != "30 minutes ago" || first["by"] != "Worker on ABC-12" || first["text"] != "PR 80 merged; the tap is next." {
		t.Errorf("first event = %v", first)
	}
	second, _ := events[1].(map[string]any)
	if second["by"] != "lead@example.com" || !strings.Contains(second["text"].(string), "Two levels") {
		t.Errorf("second event = %v", second)
	}
}

// TestACancelledSpecReadLogsNoFailure: a read that a newer target or a stop
// cancelled is no failure of Dispatch's, so it logs no warning; run 7's log
// sent its readers after one.
func TestACancelledSpecReadLogsNoFailure(t *testing.T) {
	r := newRig(t)
	logs := r.captureLogs()
	issue := &dispatch.Issue{Key: "ABC-12", Title: "Realtime voice", Status: "in_progress", PrimaryArtifactID: "doc-1"}
	issue.Artifacts = append(issue.Artifacts, struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}{ID: "doc-1", Slug: "spec"})
	r.records.issues["ABC-12"] = issue
	r.records.docs["ABC-12/spec"] = "## Summary"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = (&runner{c: r.concierge, x: &r.exchange}).readIssue(ctx, "ABC-12")
	if strings.Contains(logs.String(), "could not read an issue's spec") {
		t.Fatalf("a cancelled read logged a failure: %s", logs)
	}
}

func actor(kind, id, title string) dispatch.Actor {
	var a dispatch.Actor
	a.Kind, a.ID = kind, id
	a.Origin.SessionTitle = title
	return a
}

func claimed(key, kind, id, title string) *dispatch.Issue {
	return &dispatch.Issue{Key: key, Status: "in_progress", Claim: &dispatch.Claim{Actor: actor(kind, id, title), At: testNow.Add(-time.Hour)}}
}

// TestIssueHolderCases (c-find): issue_holder's four answers. A live session
// gives {session, live}; an unresponsive or gone one {unresponsive}; a
// person's claim {person}, whom ask_agent never asks; no claim and no session
// in the events {none}.
func TestIssueHolderCases(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = claimed("ABC-12", "session", "agent-7", "Legion architect")
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive, Title: "Legion architect", LastSeen: testNow.Add(-time.Minute)}
	result := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"})
	if result["holder"] != "session" || result["session"] != "agent-7" || result["live"] != true || result["title"] != "Legion architect" {
		t.Fatalf("live holder = %v", result)
	}

	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateUnresponsive, LastSeen: testNow.Add(-20 * time.Minute)}
	result = r.run(t, "issue_holder", map[string]string{"key": "ABC-12"})
	if result["holder"] != "unresponsive" || result["session"] != "agent-7" || result["last_seen"] != "20 minutes ago" {
		t.Fatalf("unresponsive holder = %v", result)
	}
	delete(r.agents.states, "agent-7")
	result = r.run(t, "issue_holder", map[string]string{"key": "ABC-12"})
	if result["holder"] != "unresponsive" || result["state"] != "gone" {
		t.Fatalf("gone holder = %v", result)
	}

	r.records.issues["ABC-13"] = claimed("ABC-13", "user", "lead@example.com", "")
	result = r.run(t, "issue_holder", map[string]string{"key": "ABC-13"})
	if result["holder"] != "person" || result["person"] != "lead@example.com" {
		t.Fatalf("person holder = %v", result)
	}
	if asked := r.run(t, "ask_agent", map[string]string{"session": "lead@example.com", "question": "q"}); asked["error"] == nil || len(r.agents.sent) != 0 {
		t.Fatalf("ask_agent asked a person: %v, %d sent", asked, len(r.agents.sent))
	}

	r.records.issues["ABC-14"] = &dispatch.Issue{Key: "ABC-14", Status: "backlog", Assignee: "lead@example.com"}
	r.records.events["ABC-14"] = []dispatch.Event{{Type: "issue.created", Actor: actor("user", "lead@example.com", "")}}
	result = r.run(t, "issue_holder", map[string]string{"key": "ABC-14"})
	if result["holder"] != "none" || result["assignee"] != "lead@example.com" {
		t.Fatalf("no holder = %v", result)
	}
}

// TestHolderFromEventsWithNoClaim (c-find): with no claim, the holder is the
// newest session in the issue's events, other than this unit.
func TestHolderFromEventsWithNoClaim(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "backlog"}
	r.records.events["ABC-12"] = []dispatch.Event{
		{Type: "comment.created", CreatedAt: testNow.Add(-time.Minute), Actor: actor("user", "lead@example.com", "")},
		{Type: "comment.created", CreatedAt: testNow.Add(-2 * time.Minute), Actor: actor("session", "unit-session", "This unit")},
		{Type: "artifact.version", CreatedAt: testNow.Add(-5 * time.Minute), Actor: actor("session", "agent-9", "Planner")},
		{Type: "comment.created", CreatedAt: testNow.Add(-9 * time.Minute), Actor: actor("session", "agent-3", "Older")},
	}
	r.agents.states["agent-9"] = envoy.SessionInfo{SessionID: "agent-9", State: envoy.StateLive, Title: "Planner"}
	result := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"})
	if result["holder"] != "session" || result["session"] != "agent-9" || result["live"] != true {
		t.Fatalf("holder = %v, want agent-9, the newest session that is not this unit", result)
	}
}

func TestWithoutDispatchTheToolsSaySo(t *testing.T) {
	r := newRig(t)
	r.concierge.records = nil
	for _, name := range []string{"search_issues", "read_issue", "issue_holder"} {
		result := r.run(t, name, map[string]string{"query": "x", "key": "ABC-1"})
		if result["error"] != cannotReachDispatch {
			t.Errorf("%s without Dispatch = %v", name, result)
		}
	}
	r = newRig(t)
	r.records.err = fmt.Errorf("%w: dial tcp: no route to host", dispatch.ErrUnreachable)
	if result := r.run(t, "read_issue", map[string]string{"key": "ABC-1"}); result["error"] != cannotReachDispatch {
		t.Errorf("read_issue when Dispatch does not answer = %v", result)
	}
	if result := r.run(t, "read_issue", map[string]string{"key": "../x"}); !strings.Contains(result["error"].(string), "issue key") {
		t.Errorf("read_issue of a bad key = %v", result)
	}
}

// TestToolRunSignalsOutOfReach (R10, voices B and C): a tool whose answer
// says Dispatch or Envoy did not answer returns that answer with an error
// that wraps voice.ErrUnreachable, so the Supervisor plays the error sound.
// Another answer has no error, and IssueHolder's never has one.
func TestToolRunSignalsOutOfReach(t *testing.T) {
	run := func(r *rig, name string, args string) (string, error) {
		raw, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{ID: "c1", Name: name, Args: json.RawMessage(args)})
		return string(raw), err
	}
	r := newRig(t)
	r.records.err = fmt.Errorf("%w: dial tcp: no route to host", dispatch.ErrUnreachable)
	if raw, err := run(r, "read_issue", `{"key":"ABC-1"}`); !errors.Is(err, voice.ErrUnreachable) || !strings.Contains(raw, cannotReachDispatch) {
		t.Errorf("read_issue when Dispatch does not answer = %s, %v", raw, err)
	}

	r = newRig(t)
	heldIssue(r, "session", false)
	if raw, err := run(r, "issue_holder", `{"key":"ABC-12"}`); err != nil || !strings.Contains(raw, `"live":true`) {
		t.Fatalf("control: issue_holder with Envoy answering = %s, %v", raw, err)
	}
	r.agents.lookupErr = errors.New("envoy: dial tcp: no route to host")
	if raw, err := run(r, "issue_holder", `{"key":"ABC-12"}`); !errors.Is(err, voice.ErrUnreachable) || !strings.Contains(raw, cannotReachEnvoy) {
		t.Errorf("issue_holder when Envoy does not answer = %s, %v", raw, err)
	}
	if raw, err := r.concierge.IssueHolder(context.Background(), "ABC-12"); err != nil || !strings.Contains(string(raw), cannotReachEnvoy) {
		t.Errorf("IssueHolder when Envoy does not answer = %s, %v; want its answer and no error", raw, err)
	}
	if raw, err := run(r, "read_issue", `{"key":"../x"}`); err != nil || !strings.Contains(raw, "issue key") {
		t.Errorf("read_issue of a bad key = %s, %v; want no error", raw, err)
	}
}

func TestAskAgentSendsAReadOnlyPointerQuestionAndRecordsATask(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	result := r.ask(t, "agent-7", "is the tap merged yet", "ABC-12")
	if result["task_id"] != "evt-1" || result["error"] != nil {
		t.Fatalf("ask_agent = %v", result)
	}
	if len(r.agents.sent) != 1 {
		t.Fatalf("sent %d messages", len(r.agents.sent))
	}
	sent := r.agents.sent[0]
	if sent.Target != "agent-7" || sent.IdempotencyKey == "" || sent.Supersedes != "" {
		t.Errorf("send = %+v", sent)
	}
	for _, want := range []string{
		"office meeting recorder",
		"someone in the room it cannot identify",
		"read-only question",
		"Make no change because of it unless a person confirms the change in Dispatch",
		"is the tap merged yet",
		"ABC-12",
		"meeting-1",
		"09:59:52",
		"19:52",
		"https://unit.example/api/sessions/meeting-1",
		"three sentences or fewer",
	} {
		if !strings.Contains(sent.Message, want) {
			t.Errorf("the message lacks %q:\n%s", want, sent.Message)
		}
	}
	task, err := r.store.VoiceTask("evt-1")
	if err != nil {
		t.Fatalf("no task under the event id: %v", err)
	}
	if task.SessionID != "meeting-1" || task.IssueKey != "ABC-12" || task.Holder != "session:agent-7" || task.State != storage.VoiceTaskWorking || !task.AskedAt.Equal(testNow) {
		t.Errorf("task = %+v", task)
	}
}

func TestAskAgentRefusesAGoneSessionAndRecordsNothing(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-40"] = claimed("ABC-40", "session", "agent-gone", "Gone worker")
	r.run(t, "issue_holder", map[string]string{"key": "ABC-40"})
	result := r.run(t, "ask_agent", map[string]string{"session": "agent-gone", "question": "anything"})
	if result["error"] == nil || len(r.agents.sent) != 0 {
		t.Fatalf("ask_agent to a gone session = %v, %d sent", result, len(r.agents.sent))
	}
	if tasks, err := r.store.VoiceTasks("meeting-1"); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v; want none", tasks, err)
	}
}

func TestAFailedSendLeavesNoTask(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.agents.sendErr = errors.New("envoy: 503")
	result := r.ask(t, "agent-7", "anything", "")
	if result["error"] == nil {
		t.Fatalf("ask_agent with a failed send = %v", result)
	}
	if tasks, err := r.store.VoiceTasks("meeting-1"); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v; want none", tasks, err)
	}
}

func TestASecondAskToTheSameAgentReplacesTheFirst(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	first := r.ask(t, "agent-7", "first", "")
	second := r.ask(t, "agent-7", "second", "")
	if first["task_id"] != "evt-1" || second["task_id"] != "evt-2" {
		t.Fatalf("task ids = %v, %v", first, second)
	}
	if r.agents.sent[1].Supersedes != "evt-1" {
		t.Errorf("the second message supersedes %q, want evt-1", r.agents.sent[1].Supersedes)
	}
	old, err := r.store.VoiceTask("evt-1")
	if err != nil || old.State != storage.VoiceTaskReplaced {
		t.Fatalf("first task = %+v, %v; want replaced", old, err)
	}
}

func TestTaskStatusReadsOnlyThisMeetingsTasks(t *testing.T) {
	r := newRig(t)
	r.agents.states["agent-7"] = envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive}
	r.ask(t, "agent-7", "q", "ABC-12")
	result := r.run(t, "task_status", map[string]string{"id": "evt-1"})
	if result["state"] != "working" || result["issue_key"] != "ABC-12" || result["asked"] != "just now" {
		t.Fatalf("task_status = %v", result)
	}
	if _, err := r.store.AnswerVoiceTask("evt-1", "It merged."); err != nil {
		t.Fatal(err)
	}
	if result := r.run(t, "task_status", map[string]string{"id": "evt-1"}); result["state"] != "done" || result["answer"] != "It merged." {
		t.Fatalf("task_status after the answer = %v", result)
	}

	if err := r.store.CreateSession("other-meeting", meetingStart); err != nil {
		t.Fatal(err)
	}
	if err := r.store.CreateVoiceTask(&storage.VoiceTask{ID: "other-task", SessionID: "other-meeting", AskedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if result := r.run(t, "task_status", map[string]string{"id": "other-task"}); result["error"] == nil {
		t.Fatalf("task_status read another meeting's task: %v", result)
	}
}

func TestListJoinedAgentsListsWhatTheUnitHolds(t *testing.T) {
	r := newRig(t)
	if result := r.run(t, "list_joined_agents", map[string]string{}); len(result["agents"].([]any)) != 0 {
		t.Fatalf("no agents joined, got %v", result)
	}
	r.concierge.joined = func() []JoinedAgent {
		return []JoinedAgent{{SessionID: "agent-7", Title: "Worker", IssueKey: "ABC-12"}}
	}
	result := r.run(t, "list_joined_agents", map[string]string{})
	agents := result["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["session"] != "agent-7" {
		t.Fatalf("joined = %v", result)
	}
}
