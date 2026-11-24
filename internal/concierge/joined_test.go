package concierge

import (
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// joinAgent has agent session id join the meeting on key, live.
func joinAgent(r *rig, id, key string) {
	r.agents.states[id] = envoy.SessionInfo{SessionID: id, State: envoy.StateLive}
	r.concierge.joined = func() []JoinedAgent { return []JoinedAgent{{SessionID: id, Title: "Joined work", IssueKey: key}} }
}

// TestAJoinedAgentIsAskedFirst (R17) fails if issue_holder names the issue's
// claim when an agent joined the meeting on that issue: "Wispr, ask the
// ABC-12 agent …" goes to the joined agent first.
func TestAJoinedAgentIsAskedFirst(t *testing.T) {
	r := newRig(t)
	heldIssue(r, "session", false) // agent-7 claims ABC-12
	joinAgent(r, "joined-1", "ABC-12")
	if holder := r.run(t, "issue_holder", map[string]string{"key": "abc-12"}); holder["session"] != "joined-1" || holder["holder"] != "session" {
		t.Fatalf("issue_holder = %v, want the joined agent, live", holder)
	}
	if holder := r.run(t, "issue_holder", map[string]string{"key": "XYZ-1"}); holder["session"] == "joined-1" {
		t.Fatalf("issue_holder named the joined agent for an issue it did not join on: %v", holder)
	}

	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
	answerTurn(t, r, "ask the ABC-12 agent whether the tap is merged")
	if len(r.agents.sent) != 1 || r.agents.sent[0].Target != "joined-1" {
		t.Fatalf("sent %+v, want one question to the joined agent", r.agents.sent)
	}
}

// TestABareNumberNamesAJoinedAgentsIssue (R17) fails if "ask the 1027 agent
// …", a number with no project, does not name the issue of the agent that
// joined on it, or names one with no agent joined on it.
func TestABareNumberNamesAJoinedAgentsIssue(t *testing.T) {
	r := newRig(t)
	if target := r.concierge.readTarget(r.concierge.viewProjects(), "ask the 1027 agent what is left"); strings.HasPrefix(target, "keys:") {
		t.Fatalf("readTarget = %q with no agent joined, want no key", target)
	}
	joinAgent(r, "joined-1", "INFRA-1027")
	if target := r.concierge.readTarget(r.concierge.viewProjects(), "ask the 1027 agent what is left"); !strings.HasPrefix(target, "keys:INFRA-1027|") {
		t.Fatalf("readTarget = %q, want the joined agent's INFRA-1027", target)
	}

	r.records.issues["INFRA-1027"] = &dispatch.Issue{Key: "INFRA-1027", Title: "Meeting recorder", Status: "in_progress", UpdatedAt: testNow}
	r.records.events["INFRA-1027"] = nil
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-1027 is in progress."))}}
	spoken := answerTurn(t, r, "ask the 1027 agent what is left")
	if len(r.agents.sent) != 1 || r.agents.sent[0].Target != "joined-1" {
		t.Fatalf("sent %+v (spoken %+v), want one question to the joined agent", r.agents.sent, spoken)
	}
	if strings.Contains(strings.Join(textsOf(spoken), " "), "Did you mean") {
		t.Fatalf("spoken %+v, want no confirmation of a number a joined agent's issue holds", spoken)
	}
}
