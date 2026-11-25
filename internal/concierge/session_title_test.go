package concierge

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// legionIssue is INFRA-17, claimed by the live session agent-7 whose title is
// as Legion names its sessions, "Legion architect · INFRA-17", with an event
// of INFRA-12's reviewer and one of its own architect.
func legionIssue(r *rig) {
	issue := claimed("INFRA-17", "session", "agent-7", "Legion architect · INFRA-17")
	issue.Title, issue.Status, issue.HolderState, issue.UpdatedAt = "Realtime voice", "todo", "live", testNow.Add(-time.Hour)
	r.records.issues["INFRA-17"] = issue
	r.records.events["INFRA-17"] = []dispatch.Event{
		wrote("comment.created", testNow.Add(-time.Minute), "rev-12", "Legion reviewer · INFRA-12"),
		wrote("artifact.version", testNow.Add(-time.Hour), "agent-7", "Legion architect · INFRA-17"),
	}
	r.records.events["INFRA-17"][0].Payload = json.RawMessage(`{"body":"INFRA-12 waits on this."}`)
}

// TestASessionTitleIsSaidWithoutItsSeparator (live on 2026-11-25): the room
// heard "INFRA-7 is held by Legion architect · INFRA-7": issue_holder gave the
// model the session's title as Legion writes it, a separator and the issue's
// key in it. It fails unless what the tools give the model of a session's
// title holds no separator, and a Legion session of the issue is its role
// ("its architect"), so the issue's key is said once; another issue's
// session is that issue's role ("INFRA-12's reviewer").
func TestASessionTitleIsSaidWithoutItsSeparator(t *testing.T) {
	r := newRig(t)
	legionIssue(r)
	if holder := r.run(t, "issue_holder", map[string]string{"key": "INFRA-17"}); holder["title"] != "its architect" {
		t.Errorf("issue_holder title = %q, want %q", holder["title"], "its architect")
	}
	read := r.run(t, "read_issue", map[string]string{"key": "INFRA-17"})
	events, _ := read["recent_events"].([]any)
	var by []string
	for _, event := range events {
		entry, _ := event.(map[string]any)
		name, _ := entry["by"].(string)
		by = append(by, name)
	}
	if !slices.Equal(by, []string{"INFRA-12's reviewer", "its architect"}) {
		t.Errorf("read_issue's events are by %q, want %q", by, []string{"INFRA-12's reviewer", "its architect"})
	}
	r.concierge.joined = func() []JoinedAgent {
		return []JoinedAgent{{SessionID: "agent-7", Title: "Legion implementer · INFRA-17", IssueKey: "INFRA-17"}}
	}
	joined := r.run(t, "list_joined_agents", map[string]string{})
	if agents, _ := joined["agents"].([]any); len(agents) != 1 || agents[0].(map[string]any)["title"] != "its implementer" {
		t.Errorf("list_joined_agents = %v, want the agent on INFRA-17 titled %q", joined, "its implementer")
	}
}

// TestTheHolderIsSpokenByItsRole: through Answer, the model reads INFRA-17's
// record and holder. It fails unless the record holds no separator, and the
// sentence the model writes from it, "INFRA-17 is held by its architect.",
// is said, with no separator and the key once. At c1a03f3 the record held
// "Legion architect · INFRA-17".
func TestTheHolderIsSpokenByItsRole(t *testing.T) {
	const fact = "INFRA-17 is held by its architect."
	r := newRig(t)
	legionIssue(r)
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text(fact))}}
	spoken := answerTurn(t, r, "who is working on INFRA-17")
	if len(r.model.requests) == 0 {
		t.Fatalf("the model was not asked; spoken %+v", spoken)
	}
	var record strings.Builder
	for _, content := range r.model.requests[0] {
		for _, part := range content.Parts {
			record.WriteString(part.Text)
		}
	}
	if strings.ContainsAny(record.String(), "·•|") {
		t.Errorf("the model's record holds a title's separator:\n%s", record.String())
	}
	heard := slices.ContainsFunc(spoken, func(s said) bool { return s.text == fact })
	if !heard || slices.ContainsFunc(spoken, func(s said) bool {
		return strings.ContainsAny(s.text, "·•|") || strings.Count(s.text, "INFRA-17") > 1
	}) {
		t.Errorf("spoken %+v; want %q, no separator and the key once a sentence", spoken, fact)
	}
}

// TestASessionNameIsItsWords: sessionName's table. A title's parts apart from
// its separators ("·", "•", "|", a dash), the issue's own key left out, a
// Legion role named as the issue's ("its architect") or another issue's
// ("INFRA-12's reviewer"); an emoji goes, as spokenName has it.
func TestASessionNameIsItsWords(t *testing.T) {
	for _, tc := range []struct{ title, key, want string }{
		{"Legion architect · INFRA-7", "INFRA-7", "its architect"},
		{"Legion reviewer · INFRA-12", "INFRA-7", "INFRA-12's reviewer"},
		{"Legion implementer · infra-7", "INFRA-7", "its implementer"},
		{"Legion architect • INFRA-7", "INFRA-7", "its architect"},
		{"Legion architect · INFRA-7", "", "INFRA-7's architect"},
		{"🍓 PI: Deploy the unit", "INFRA-7", "PI: Deploy the unit"},
		{"Worker | INFRA-7", "INFRA-7", "Worker"},
		{"Worker — realtime voice", "INFRA-7", "Worker, realtime voice"},
		{"Release - INFRA-7 - notes", "INFRA-7", "Release, notes"},
		{"Legion architect", "INFRA-7", "Legion architect"},
		{"INFRA-7", "INFRA-7", "its agent"},
		{"", "INFRA-7", ""},
		{"Parser-evals", "INFRA-7", "Parser-evals"},
	} {
		if got := sessionName(tc.title, tc.key); got != tc.want {
			t.Errorf("sessionName(%q, %q) = %q, want %q", tc.title, tc.key, got, tc.want)
		}
	}
}
