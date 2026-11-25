package concierge

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// These tests hold issue_holder to the voice research of 2026-11-25 ("Why
// Envoy's registry answers 400"): Dispatch judges a claim's session itself, a
// Legion daemon is never a holder, an id Envoy refuses is an unknown holder
// and never one that is not answering, and with no claim the holder is the
// issue's worker rather than the newest session that passed by. Envoy's
// liveness never overrides Dispatch's.

// statusSet is an issue.updated event of a session that set the issue's
// status.
func statusSet(at time.Time, id, title, status string) dispatch.Event {
	return dispatch.Event{Type: "issue.updated", CreatedAt: at, Actor: actor("session", id, title), Payload: json.RawMessage(`{"changed":["status"],"status":"` + status + `"}`)}
}

// wrote is an event of kind by session id that changes no status.
func wrote(kind string, at time.Time, id, title string) dispatch.Event {
	return dispatch.Event{Type: kind, CreatedAt: at, Actor: actor("session", id, title), Payload: json.RawMessage(`{}`)}
}

func live(id string) envoy.SessionInfo {
	return envoy.SessionInfo{SessionID: id, State: envoy.StateLive}
}

// TestTheClaimsLivenessIsDispatchs fails if issue_holder asks Envoy about a
// claim's session that Dispatch judged, or answers otherwise than Dispatch's
// holder_state: live is {session, live}; unresponsive and gone are
// {unresponsive} (R5), with when Dispatch last saw it. Live on 2026-11-25,
// Dispatch read BET-1366's architect live while Envoy's registry answered 404
// gone for it, as a Legion session does while it restarts. Only when Dispatch
// could not judge (unknown, or a Dispatch that names no holder_state) is Envoy
// asked.
func TestTheClaimsLivenessIsDispatchs(t *testing.T) {
	seen := testNow.Add(-20 * time.Minute)
	for _, c := range []struct {
		name, state string
		lastSeen    *time.Time
		envoy       *envoy.SessionInfo
		want        map[string]any
		lookups     int
	}{
		{"live while Envoy says gone, a restarting Legion worker", "live", nil, nil,
			map[string]any{"holder": "session", "live": true, "session": "agent-7", "title": "Legion architect"}, 0},
		{"unresponsive while Envoy says live", "unresponsive", &seen, &envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive},
			map[string]any{"holder": "unresponsive", "state": "unresponsive", "session": "agent-7", "title": "Legion architect", "last_seen": "20 minutes ago"}, 0},
		{"gone while Envoy says live", "gone", nil, &envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive},
			map[string]any{"holder": "unresponsive", "state": "gone", "session": "agent-7", "title": "Legion architect"}, 0},
		{"unknown: Envoy is asked", "unknown", nil, &envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateLive},
			map[string]any{"holder": "session", "live": true, "session": "agent-7", "title": "Legion architect"}, 1},
		{"no holder_state: Envoy is asked", "", nil, &envoy.SessionInfo{SessionID: "agent-7", State: envoy.StateUnresponsive, LastSeen: seen},
			map[string]any{"holder": "unresponsive", "state": "unresponsive", "session": "agent-7", "title": "Legion architect", "last_seen": "20 minutes ago"}, 1},
	} {
		r := newRig(t)
		issue := claimed("ABC-12", "session", "agent-7", "Legion architect")
		issue.HolderState, issue.LastSeen = c.state, c.lastSeen
		r.records.issues["ABC-12"] = issue
		if c.envoy != nil {
			r.agents.states["agent-7"] = *c.envoy
		}
		got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"})
		if !maps.Equal(got, c.want) {
			t.Errorf("%s: issue_holder = %v, want %v", c.name, got, c.want)
		}
		if n := r.agents.lookupCount(); n != c.lookups {
			t.Errorf("%s: Envoy was asked %d times, want %d", c.name, n, c.lookups)
		}
	}
}

// TestALegionDaemonIsNeverTheHolder (INFRA-1508 on 2026-11-25): with no
// claim, the newest session event was the Legion daemon's child.status, whose
// actor legion-daemon:INFRA Envoy refuses as a key (400), and the room heard
// "INFRA-1508's agent is not answering." The daemon is skipped: the holder is
// the session that worked the issue. When that session is gone and Dispatch
// names no worker, nobody is on the issue: no agent is assigned to it, so the
// room hears that, not that an agent is not answering.
func TestALegionDaemonIsNeverTheHolder(t *testing.T) {
	events := []dispatch.Event{
		wrote("child.status", testNow.Add(-31*time.Hour), "legion-daemon:INFRA", ""),
		wrote("child.status", testNow.Add(-32*time.Hour), "legion-daemon:INFRA", ""),
		wrote("comment.created", testNow.Add(-6*24*time.Hour), "agent-9", "Prioritizer"),
	}
	setup := func() *rig {
		r := newRig(t)
		r.records.issues["INFRA-1508"] = &dispatch.Issue{Key: "INFRA-1508", Status: "testing", Assignee: "lead@example.com"}
		r.records.events["INFRA-1508"] = events
		return r
	}
	r := setup()
	r.agents.states["agent-9"] = live("agent-9")
	if got, want := r.run(t, "issue_holder", map[string]string{"key": "INFRA-1508"}), map[string]any{"holder": "session", "live": true, "session": "agent-9", "title": "Prioritizer"}; !maps.Equal(got, want) {
		t.Errorf("issue_holder = %v, want %v, the session past the daemon", got, want)
	}

	// The session that worked it is gone: no agent is on the issue.
	r = setup()
	if got, want := r.run(t, "issue_holder", map[string]string{"key": "INFRA-1508"}), map[string]any{"holder": "none", "assigned_to": "lead@example.com"}; !maps.Equal(got, want) {
		t.Errorf("issue_holder = %v, want %v", got, want)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("INFRA-1508 is in testing."))}}
	var spoken []said
	err := r.concierge.Answer(context.Background(), &r.exchange, "what is happening with INFRA-1508", recorder(&spoken))
	if err != nil {
		t.Fatalf("Answer = %v, want no error and no error sound", err)
	}
	if slices.ContainsFunc(spoken, func(s said) bool { return strings.Contains(s.text, "not answering") }) ||
		!slices.Contains(spoken, said{"No agent is on INFRA-1508 right now.", voice.Fact}) {
		t.Errorf("spoken = %+v, want that no agent is on INFRA-1508 right now, and no agent that is not answering", spoken)
	}
}

// TestNoClaimAndNoSessionIsNobody fails if an issue nobody claims, whose
// events name no agent session but the Legion daemon's writer and people, is
// held by anyone.
func TestNoClaimAndNoSessionIsNobody(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress", Assignee: "lead@example.com"}
	r.records.events["ABC-12"] = []dispatch.Event{
		wrote("child.status", testNow.Add(-time.Hour), "legion-daemon:ABC", ""),
		{Type: "comment.created", CreatedAt: testNow.Add(-2 * time.Hour), Actor: actor("user", "lead@example.com", "")},
	}
	if got, want := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}), map[string]any{"holder": "none", "assigned_to": "lead@example.com"}; !maps.Equal(got, want) {
		t.Errorf("issue_holder = %v, want %v", got, want)
	}
}

// TestARestartedLegionWorkerIsItsRoleHolder fails if, with no claim, the
// holder is the session that last wrote to the issue rather than the one the
// Legion daemon publishes for its role: a Legion session restarts under a new
// id, Envoy calls the old one gone, and the room heard that the issue's agent
// was not answering while its new session worked.
func TestARestartedLegionWorkerIsItsRoleHolder(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
	r.records.events["ABC-12"] = []dispatch.Event{
		wrote("artifact.version", testNow.Add(-10*time.Minute), "impl-old", "Legion implementer · ABC-12"),
		statusSet(testNow.Add(-3*time.Hour), "impl-old", "Legion implementer · ABC-12", "in_progress"),
	}
	r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "implementer", Session: "impl-new"}}
	r.agents.states["impl-new"] = envoy.SessionInfo{SessionID: "impl-new", State: envoy.StateLive, Title: "Legion implementer · ABC-12"}
	if got, want := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}), map[string]any{"holder": "session", "live": true, "session": "impl-new", "title": "Legion implementer · ABC-12"}; !maps.Equal(got, want) {
		t.Errorf("issue_holder = %v, want %v, the implementer the daemon publishes", got, want)
	}
}

// TestAPassingReviewerIsNotTheHolder fails if, with no claim, the newest
// session to write, a passing reviewer, is the holder over the Legion role
// holder of the issue's current phase: the implementer while the issue is in
// progress, the tester while it is in testing.
func TestAPassingReviewerIsNotTheHolder(t *testing.T) {
	for _, c := range []struct{ status, want string }{{"in_progress", "impl-1"}, {"testing", "test-1"}} {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: c.status}
		r.records.events["ABC-12"] = []dispatch.Event{
			wrote("handoff.recorded", testNow.Add(-5*time.Minute), "rev-1", "Legion reviewer · ABC-12"),
			wrote("result.recorded", testNow.Add(-time.Hour), "test-1", "Legion tester · ABC-12"),
		}
		r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "architect", Session: "arch-1"}, {Role: "implementer", Session: "impl-1"}, {Role: "tester", Session: "test-1"}, {Role: "reviewer", Session: "rev-1"}}
		for _, id := range []string{"arch-1", "impl-1", "test-1", "rev-1"} {
			r.agents.states[id] = live(id)
		}
		if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != c.want || got["live"] != true {
			t.Errorf("%s: issue_holder = %v, want %s, the role holder of the current phase", c.status, got, c.want)
		}
	}
}

// TestAnEarlierWorkerDoesNotHideTheCurrentOne (Sol's review of #193 at
// 4788285): with no claim, a session that worked an earlier phase, while it
// still runs, hid the session working now. An issue in testing named its
// previous implementer, live or unresponsive, over its live tester, who
// wrote the newest event; an old controller that set the status hid the
// worker who wrote since. The current phase's role holder decides, then the
// newest writer; an earlier role holder or status setter never beats a newer
// running writer.
func TestAnEarlierWorkerDoesNotHideTheCurrentOne(t *testing.T) {
	for _, old := range []envoy.SessionState{envoy.StateLive, envoy.StateUnresponsive, envoy.StateGone} {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "testing"}
		r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "implementer", Session: "previous-impl"}, {Role: "tester", Session: "current-tester"}}
		r.records.events["ABC-12"] = []dispatch.Event{
			wrote("comment.created", testNow.Add(-time.Minute), "current-tester", "Current tester"),
			wrote("comment.created", testNow.Add(-time.Hour), "previous-impl", "Previous implementer"),
		}
		r.agents.states["previous-impl"] = envoy.SessionInfo{SessionID: "previous-impl", State: old}
		r.agents.states["current-tester"] = envoy.SessionInfo{SessionID: "current-tester", State: envoy.StateLive}
		if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != "current-tester" || got["live"] != true {
			t.Errorf("previous implementer %s: issue_holder = %v, want current-tester", old, got)
		}

		r = newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
		r.records.events["ABC-12"] = []dispatch.Event{
			wrote("comment.created", testNow.Add(-time.Minute), "current-worker", "Current worker"),
			statusSet(testNow.Add(-24*time.Hour), "old-controller", "Project controller", "in_progress"),
		}
		r.agents.states["current-worker"] = envoy.SessionInfo{SessionID: "current-worker", State: envoy.StateLive}
		r.agents.states["old-controller"] = envoy.SessionInfo{SessionID: "old-controller", State: old}
		if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != "current-worker" || got["live"] != true {
			t.Errorf("old controller %s: issue_holder = %v, want current-worker", old, got)
		}
	}
}

// TestTheCurrentRoleThatWroteLastHolds (Sol's second review of #193, at
// cdd3a93): one status is two Legion phases. needs_review is reviewing (the
// reviewer) and the architect's acceptance; in_progress is planning and
// implementing. Both workers stay live after their phase, so a fixed order
// named the finished reviewer during acceptance. Among the running holders
// of the status's roles, the one who wrote to the issue most recently holds
// it.
func TestTheCurrentRoleThatWroteLastHolds(t *testing.T) {
	for _, c := range []struct {
		name, status, newest, older string
		roles                       []dispatch.RoleHolder
	}{
		{"acceptance after review", "needs_review", "arch-1", "rev-1", []dispatch.RoleHolder{{Role: "architect", Session: "arch-1"}, {Role: "reviewer", Session: "rev-1"}}},
		{"review in progress", "needs_review", "rev-1", "arch-1", []dispatch.RoleHolder{{Role: "architect", Session: "arch-1"}, {Role: "reviewer", Session: "rev-1"}}},
		{"planning in progress", "in_progress", "plan-1", "impl-1", []dispatch.RoleHolder{{Role: "planner", Session: "plan-1"}, {Role: "implementer", Session: "impl-1"}}},
		{"implementing after planning", "in_progress", "impl-1", "plan-1", []dispatch.RoleHolder{{Role: "planner", Session: "plan-1"}, {Role: "implementer", Session: "impl-1"}}},
	} {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: c.status}
		r.records.roleHolders["ABC-12"] = c.roles
		r.records.events["ABC-12"] = []dispatch.Event{
			wrote("comment.created", testNow.Add(-time.Minute), c.newest, "Newest"),
			wrote("comment.created", testNow.Add(-time.Hour), c.older, "Older"),
		}
		for _, h := range c.roles {
			r.agents.states[h.Session] = live(h.Session)
		}
		if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != c.newest || got["live"] != true {
			t.Errorf("%s: issue_holder = %v, want %s, the current role's holder who wrote last", c.name, got, c.newest)
		}
	}
}

// TestAnAskDuringAcceptanceGoesToTheArchitect (Sol's probe): "ask the agent
// on ABC-12 …" on an unclaimed issue in needs_review, whose architect is
// accepting it after the review, goes to the architect, not the reviewer.
func TestAnAskDuringAcceptanceGoesToTheArchitect(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Title: "Offline mode", Status: "needs_review"}
	r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "architect", Session: "architect"}, {Role: "reviewer", Session: "reviewer"}}
	r.records.events["ABC-12"] = []dispatch.Event{
		{Type: "comment.created", CreatedAt: testNow.Add(-time.Minute), Actor: actor("session", "architect", "Legion architect"), Payload: json.RawMessage(`{"body":"Review passed. I am accepting this head now."}`)},
		{Type: "comment.created", CreatedAt: testNow.Add(-time.Hour), Actor: actor("session", "reviewer", "Legion reviewer"), Payload: json.RawMessage(`{"body":"My phase is complete."}`)},
	}
	r.agents.states["architect"], r.agents.states["reviewer"] = live("architect"), live("reviewer")
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: the review passed."))}}
	answerTurn(t, r, "ask the agent on ABC-12 which scope decision the architect still needs from me")
	r.agents.mu.Lock()
	sent := slices.Clone(r.agents.sent)
	r.agents.mu.Unlock()
	if len(sent) != 1 || sent[0].Target != "architect" {
		t.Errorf("sent %+v, want one question to the architect", sent)
	}
}

// TestAPersonsClaimIsThePerson fails if a person's claim, whose holder_state
// Dispatch leaves null, is read as Dispatch giving no liveness: no Envoy
// lookup, no role holder, no session of the events is the holder.
func TestAPersonsClaimIsThePerson(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = claimed("ABC-12", "user", "lead@example.com", "")
	r.records.events["ABC-12"] = []dispatch.Event{statusSet(testNow.Add(-time.Minute), "agent-9", "Worker", "in_progress")}
	r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "implementer", Session: "agent-9"}}
	r.agents.states["agent-9"] = live("agent-9")
	if got, want := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}), map[string]any{"holder": "person", "person": "lead@example.com"}; !maps.Equal(got, want) {
		t.Errorf("issue_holder = %v, want %v", got, want)
	}
	if n := r.agents.lookupCount(); n != 0 {
		t.Errorf("Envoy was asked %d times about a person's claim", n)
	}
}

// TestAnIDEnvoyRefusesIsNoHolderThatIsNotAnswering fails if, with no claim, an
// event's session id Envoy's registry refuses as a key (400) is the holder
// rather than passed over for the next, or if an id that cannot be a session
// in Envoy's registry (a "/" the Envoy client refuses to send, as Dispatch's
// "ci-minutes/…" writer; a ":" no registry key holds) is looked up at all.
func TestAnIDEnvoyRefusesIsNoHolderThatIsNotAnswering(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
	r.records.events["ABC-12"] = []dispatch.Event{
		wrote("comment.created", testNow.Add(-time.Minute), "ci-minutes/abc123", ""),
		wrote("comment.created", testNow.Add(-2*time.Minute), "relay:ossv", "Relay"),
		wrote("comment.created", testNow.Add(-3*time.Minute), "refused-1", "Refused"),
		wrote("comment.created", testNow.Add(-4*time.Minute), "agent-9", "Worker"),
	}
	r.agents.refused = map[string]bool{"refused-1": true}
	r.agents.states["agent-9"] = live("agent-9")
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != "agent-9" || got["live"] != true {
		t.Errorf("issue_holder = %v, want agent-9, past the ids Envoy refuses", got)
	}
	if n := r.agents.lookupCount(); n != 2 {
		t.Errorf("Envoy was asked %d times, want 2: refused-1, then agent-9", n)
	}
}

// TestARoleHoldersReadThatFailsLeavesTheEvents fails if, with no claim, a
// read of the Legion role holders that fails fails issue_holder: the events
// still name the session that works the issue. A not-found there would tell
// the model that Dispatch has no such issue, and a server fault that Dispatch
// is out of reach, beside a read_issue that read the issue.
func TestARoleHoldersReadThatFailsLeavesTheEvents(t *testing.T) {
	for _, failure := range []error{
		&dispatch.APIError{Status: 404, Code: "NOT_FOUND", Route: "role_holders"},
		&dispatch.APIError{Status: 500, Code: "INTERNAL", Route: "role_holders"},
	} {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
		r.records.events["ABC-12"] = []dispatch.Event{statusSet(testNow.Add(-time.Hour), "agent-9", "Worker", "in_progress")}
		r.records.roleHoldersErr = failure
		r.agents.states["agent-9"] = live("agent-9")
		if got, want := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}), map[string]any{"holder": "session", "live": true, "session": "agent-9", "title": "Worker"}; !maps.Equal(got, want) {
			t.Errorf("role holders answered %v: issue_holder = %v, want %v, from the events", failure, got, want)
		}
	}
}

// TestNoEventsReadOutlivesTheHolder fails if issue_holder answers while its
// read of the events, which runs beside the role holders' read, still runs:
// a role holder found first stops that read, and the tool's run, which Stop
// waits for, ends only after it.
func TestNoEventsReadOutlivesTheHolder(t *testing.T) {
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
	r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "implementer", Session: "impl-1"}}
	r.records.holderEventsSlow = true
	r.agents.states["impl-1"] = live("impl-1")
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != "impl-1" || got["live"] != true {
		t.Fatalf("issue_holder = %v, want impl-1, the implementer", got)
	}
	if !slices.Contains(r.records.eventLimits(), holderEvents) || r.records.openHolderEvents() != 0 {
		t.Errorf("issue_holder answered before its read of the events ended (reads asked %v, %d not ended)", r.records.eventLimits(), r.records.openHolderEvents())
	}
}

// TestAnEnvoyGoneWorkerDoesNotEndTheSearch fails if, with no claim, one
// session Envoy's registry calls gone ends the search with nobody on the
// issue while another session Dispatch names as a worker runs: a Legion role
// holder, or a session that set the issue's status. Envoy calls a restarting
// Legion session gone for under a second (4 times in 52 sweeps of 355 live
// claims on 2026-11-25). Past the current phase's role holder, the newest
// writer decides when it runs; when it is gone, the other role holders and
// the status setters are tried; none running, nobody is on the issue.
func TestAnEnvoyGoneWorkerDoesNotEndTheSearch(t *testing.T) {
	for _, c := range []struct {
		name    string
		roles   []dispatch.RoleHolder
		events  []dispatch.Event
		live    []string
		want    map[string]any
		lookups int
	}{
		{"a gone implementer, a running tester", []dispatch.RoleHolder{{Role: "implementer", Session: "impl-1"}, {Role: "tester", Session: "test-1"}},
			[]dispatch.Event{wrote("artifact.version", testNow.Add(-time.Minute), "impl-1", "Legion implementer · ABC-12")},
			[]string{"test-1"}, map[string]any{"holder": "session", "live": true, "session": "test-1", "title": ""}, 2},
		{"a gone newest writer, a running status setter", nil,
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "rev-2", "Reviewer"),
				statusSet(testNow.Add(-2*time.Hour), "worker-2", "Worker", "in_progress"),
			},
			[]string{"worker-2"}, map[string]any{"holder": "session", "live": true, "session": "worker-2", "title": "Worker"}, 2},
		{"a gone phase worker: the newest writer, running", []dispatch.RoleHolder{{Role: "implementer", Session: "impl-1"}},
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "rev-2", "Reviewer"),
				statusSet(testNow.Add(-time.Hour), "worker-old", "Worker", "in_progress"),
			},
			[]string{"rev-2"}, map[string]any{"holder": "session", "live": true, "session": "rev-2", "title": "Reviewer"}, 2},
		{"nothing Dispatch names runs, the newest writer gone: nobody", []dispatch.RoleHolder{{Role: "architect", Session: "arch-1"}},
			[]dispatch.Event{
				wrote("artifact.version", testNow.Add(-time.Minute), "arch-1", "Legion architect · ABC-12"),
				wrote("comment.created", testNow.Add(-time.Hour), "passer-1", "Passer"),
			},
			[]string{"passer-1"}, map[string]any{"holder": "none", "assigned_to": "lead@example.com"}, 1},
	} {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress", Assignee: "lead@example.com"}
		r.records.roleHolders["ABC-12"] = c.roles
		r.records.events["ABC-12"] = c.events
		for _, id := range c.live {
			r.agents.states[id] = live(id)
		}
		if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); !maps.Equal(got, c.want) {
			t.Errorf("%s: issue_holder = %v, want %v", c.name, got, c.want)
		}
		if n := r.agents.lookupCount(); n != c.lookups {
			t.Errorf("%s: Envoy was asked %d times, want %d", c.name, n, c.lookups)
		}
	}
}

// TestAClaimByNoAgentSessionIsUnknownWhateverDispatchSays fails if a claim by
// an id that cannot be a session in Envoy's registry is read by Dispatch's
// holder_state, which calls every session claim its list lacks gone, or is
// sent to Envoy: the room would hear that the issue's agent is not answering.
func TestAClaimByNoAgentSessionIsUnknownWhateverDispatchSays(t *testing.T) {
	for _, id := range []string{"legion-daemon:INFRA", "relay:ossv", "ci-minutes/abc123"} {
		for _, state := range []string{"gone", "unknown", ""} {
			r := newRig(t)
			issue := claimed("ABC-12", "session", id, "")
			issue.HolderState = state
			r.records.issues["ABC-12"] = issue
			if got, want := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}), map[string]any{"holder": "unknown"}; !maps.Equal(got, want) {
				t.Errorf("%s, holder_state %q: issue_holder = %v, want %v", id, state, got, want)
			}
			if n := r.agents.lookupCount(); n != 0 {
				t.Errorf("%s, holder_state %q: Envoy was asked %d times", id, state, n)
			}
		}
	}
}

// TestASessionDispatchReadsRunningIsAsked fails if ask_agent refuses a
// claim's session that Dispatch read running, live or unresponsive, because
// Envoy's registry calls it gone, as it does for under a second while a
// Legion session restarts: the room heard "I could not ask the agent on
// ABC-12." Envoy stores the question for an unresponsive session. A session
// Envoy alone judged gone is still not asked.
func TestASessionDispatchReadsRunningIsAsked(t *testing.T) {
	for _, c := range []struct{ state, said string }{
		{"live", "I asked the agent on ABC-12."},
		{"unresponsive", "ABC-12's agent isn't answering now; I left the question for it."},
	} {
		r := newRig(t)
		heldIssue(r, "session", false)
		r.records.issues["ABC-12"].HolderState = c.state
		delete(r.agents.states, "agent-7")
		r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12: PR 80 is in review."))}}
		spoken := answerTurn(t, r, "ask the agent on ABC-12 whether the tap is merged")
		if len(r.agents.sent) != 1 || r.agents.sent[0].Target != "agent-7" {
			t.Errorf("Dispatch read it %s: sent %+v, want the question to agent-7", c.state, r.agents.sent)
		}
		if !slices.ContainsFunc(spoken, func(s said) bool { return strings.HasPrefix(s.text, c.said) }) {
			t.Errorf("Dispatch read it %s: spoken = %+v, want %q", c.state, spoken, c.said)
		}
	}

	r := newRig(t)
	heldIssue(r, "session", false)
	delete(r.agents.states, "agent-7")
	if holder := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); holder["state"] != "gone" {
		t.Fatalf("issue_holder = %v, want agent-7 gone, by Envoy", holder)
	}
	if asked := r.run(t, "ask_agent", map[string]string{"session": "agent-7", "question": "Is the tap merged?", "issue_key": "ABC-12"}); asked["error"] == nil || len(r.agents.sent) != 0 {
		t.Errorf("ask_agent = %v, sent %d; want a session Envoy alone calls gone not asked", asked, len(r.agents.sent))
	}
}

// TestTheAssigneeIsNotTheHolder (a live run of the real Concierge on
// 2026-11-25): asked "Who holds INFRA-3173?", the voice named the person
// the issue is assigned to as its holder: the model took read_issue's
// assignee for the holder, while issue_holder named the issue's Legion
// architect, gone. read_issue and issue_holder name the person an issue is
// assigned to as such, never as a holder, and the model is told that the
// holder comes only from issue_holder.
func TestTheAssigneeIsNotTheHolder(t *testing.T) {
	r := newRig(t)
	issue := claimed("ABC-12", "session", "agent-7", "Legion architect")
	issue.Assignee, issue.HolderState = "lead@example.com", "gone"
	r.records.issues["ABC-12"] = issue
	r.records.issues["ABC-13"] = &dispatch.Issue{Key: "ABC-13", Status: "todo", Assignee: "lead@example.com"}
	for _, c := range []struct{ tool, key string }{{"read_issue", "ABC-12"}, {"issue_holder", "ABC-13"}} {
		got := r.run(t, c.tool, map[string]string{"key": c.key})
		if _, named := got["assignee"]; named || got["assigned_to"] != "lead@example.com" {
			t.Errorf("%s(%s) = %v, want the person it is assigned to as assigned_to, and no assignee", c.tool, c.key, got)
		}
	}
	if !strings.Contains(systemPrompt, "Who holds an issue comes only from issue_holder") {
		t.Error("the system prompt does not say that the holder comes only from issue_holder")
	}
}
