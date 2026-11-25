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
// Envoy's registry answers 400") and Sol's three reviews of #193: Dispatch
// judges a claim's session itself, a Legion daemon is never a holder, and
// with no claim the holder is the session that works the issue, runs, and
// wrote to it last; a session that wrote in passing never holds it. Who holds
// it is unknown, never guessed, when its history cannot be read.

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

// personNotes are n events of a person, newest first, from at back.
func personNotes(n int, at time.Time) []dispatch.Event {
	notes := make([]dispatch.Event, n)
	for i := range notes {
		notes[i] = dispatch.Event{Type: "comment.created", CreatedAt: at.Add(-time.Duration(i) * time.Second), Actor: actor("user", "lead@example.com", "")}
	}
	return notes
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
// the session that wrote to the issue. When that session is gone and no other
// works it, nobody is on the issue: no agent is assigned to it, so the room
// hears that, not that an agent is not answering.
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

	// The session that wrote to it is gone: no agent is on the issue.
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

// TestAnIDEnvoyRefusesIsNoHolderThatIsNotAnswering fails if an id that
// cannot be a session in Envoy's registry (a "/" the Envoy client refuses to
// send, as Dispatch's "ci-minutes/…" writer; a ":" no registry key holds) is
// looked up at all, or if a writer Envoy's registry refuses as a key (400)
// is the holder rather than passed over for the next, as no session: the
// room heard that such an issue's agent was not answering.
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

// TestTheSessionsThatWorkTheIssueHoldIt fails if, with no claim, the holder
// is not the session that works the issue and runs and wrote to it most
// recently. The sessions that work it are its Legion role holders, every one,
// and each session that set its status or claimed it; a session that wrote
// in passing never holds it, and an issue none works falls back to the
// newest session that wrote and runs. Envoy calling one gone passes it over.
func TestTheSessionsThatWorkTheIssueHoldIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		roles  []dispatch.RoleHolder
		events []dispatch.Event
		live   []string
		want   string
	}{
		{"a role holder over a newer passer", []dispatch.RoleHolder{{Role: "reviewer", Session: "rev-1"}},
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Legion implementer · ABC-99"),
				wrote("handoff.recorded", testNow.Add(-time.Hour), "rev-1", "Legion reviewer · ABC-12"),
			},
			[]string{"passer-1", "rev-1"}, "rev-1"},
		{"the role holder that wrote last, whatever its role", []dispatch.RoleHolder{{Role: "architect", Session: "arch-1"}, {Role: "reviewer", Session: "rev-1"}},
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "arch-1", "Legion architect · ABC-12"),
				wrote("handoff.recorded", testNow.Add(-time.Hour), "rev-1", "Legion reviewer · ABC-12"),
			},
			[]string{"arch-1", "rev-1"}, "arch-1"},
		{"a status setter over a newer passer", nil,
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Coordinator"),
				statusSet(testNow.Add(-time.Hour), "worker-1", "Worker", "in_progress"),
			},
			[]string{"passer-1", "worker-1"}, "worker-1"},
		{"a session that claimed it", nil,
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Coordinator"),
				wrote("issue.released", testNow.Add(-10*time.Minute), "worker-1", "Worker"),
				wrote("issue.claimed", testNow.Add(-time.Hour), "worker-1", "Worker"),
			},
			[]string{"passer-1", "worker-1"}, "worker-1"},
		{"a gone role holder passed over for an older one that runs", []dispatch.RoleHolder{{Role: "implementer", Session: "impl-1"}, {Role: "tester", Session: "test-1"}},
			[]dispatch.Event{
				wrote("artifact.version", testNow.Add(-time.Minute), "impl-1", "Legion implementer · ABC-12"),
				wrote("result.recorded", testNow.Add(-time.Hour), "test-1", "Legion tester · ABC-12"),
			},
			[]string{"test-1"}, "test-1"},
		{"no session works it: the newest writer that runs", nil,
			[]dispatch.Event{
				wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Coordinator"),
				wrote("comment.created", testNow.Add(-time.Hour), "passer-2", "Coordinator"),
			},
			[]string{"passer-2"}, "passer-2"},
	} {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress", Assignee: "lead@example.com"}
		r.records.roleHolders["ABC-12"] = c.roles
		r.records.events["ABC-12"] = c.events
		for _, id := range c.live {
			r.agents.states[id] = live(id)
		}
		if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != c.want || got["live"] != true {
			t.Errorf("%s: issue_holder = %v, want %s", c.name, got, c.want)
		}
	}
}

// TestNobodyRunsIsNobody fails if an issue whose sessions that work it are
// all gone is held by anyone, a passer that runs included. A role holder
// that never wrote holds it only when it is the one that runs: two such
// cannot be ranked, and who holds the issue is unknown.
func TestNobodyRunsIsNobody(t *testing.T) {
	nobody := map[string]any{"holder": "none", "assigned_to": "lead@example.com"}
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress", Assignee: "lead@example.com"}
	r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "implementer", Session: "impl-1"}}
	r.records.events["ABC-12"] = []dispatch.Event{
		wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Coordinator"),
		wrote("artifact.version", testNow.Add(-time.Hour), "impl-1", "Legion implementer · ABC-12"),
	}
	r.agents.states["passer-1"] = live("passer-1")
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); !maps.Equal(got, nobody) {
		t.Errorf("the worker gone, a passer live: issue_holder = %v, want %v", got, nobody)
	}

	silent := func(roles ...string) *rig {
		r := newRig(t)
		r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "todo", Assignee: "lead@example.com"}
		r.records.events["ABC-12"] = []dispatch.Event{wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Coordinator")}
		for _, role := range roles {
			r.records.roleHolders["ABC-12"] = append(r.records.roleHolders["ABC-12"], dispatch.RoleHolder{Role: role, Session: role + "-1"})
			r.agents.states[role+"-1"] = live(role + "-1")
		}
		return r
	}
	if got := silent("architect").run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != "architect-1" || got["live"] != true {
		t.Errorf("one role holder runs, never wrote: issue_holder = %v, want architect-1", got)
	}
	if got, want := silent("architect", "planner").run(t, "issue_holder", map[string]string{"key": "ABC-12"}), map[string]any{"holder": "unknown"}; !maps.Equal(got, want) {
		t.Errorf("two role holders run, neither wrote: issue_holder = %v, want %v", got, want)
	}
}

// TestTheEventsArePagedUntilTheAnswerSettles fails if issue_holder decides
// on a page that cannot settle it: a session that set the issue's status in
// an older page outranks a passer that wrote since, so a running writer that
// works no role is placed only once the history says whether it set the
// status. It pages newest first, before the oldest event read.
func TestTheEventsArePagedUntilTheAnswerSettles(t *testing.T) {
	events := []dispatch.Event{wrote("comment.created", testNow.Add(-time.Minute), "worker-1", "Worker")}
	events = append(events, personNotes(holderPage, testNow.Add(-2*time.Minute))...)
	events = append(events,
		statusSet(testNow.Add(-48*time.Hour), "worker-1", "Worker", "in_progress"),
		wrote("comment.created", testNow.Add(-49*time.Hour), "older-1", "Older"))
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
	r.records.events["ABC-12"] = events
	r.agents.states["worker-1"], r.agents.states["older-1"] = live("worker-1"), live("older-1")
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); got["session"] != "worker-1" || got["live"] != true {
		t.Errorf("issue_holder = %v, want worker-1, the status setter a page down", got)
	}
	if limits := r.records.eventLimits(); !slices.Equal(limits, []int{holderPage, holderPage}) {
		t.Errorf("events read with limits %v, want two pages of %d", limits, holderPage)
	}
}

// TestHistoryNotReadIsNoHolder fails if, when the events or the role holders
// cannot be read, the holder's bound ends the read, or the history is longer
// than the pages issue_holder reads before the answer settles, it names or
// asks anyone. The holder could not be read: the voice says so, naming the
// key, as for any holder read that fails (R8), and sends nothing.
func TestHistoryNotReadIsNoHolder(t *testing.T) {
	unread := map[string]any{"error": holderUnread}
	r := newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "needs_review"}
	r.records.roleHolders["ABC-12"] = []dispatch.RoleHolder{{Role: "architect", Session: "arch-1"}, {Role: "reviewer", Session: "rev-1"}}
	r.records.events["ABC-12"] = []dispatch.Event{wrote("comment.created", testNow.Add(-time.Minute), "rev-1", "Reviewer")}
	r.records.holderEventsErr = &dispatch.APIError{Status: 500, Code: "INTERNAL", Route: "events"}
	r.agents.states["arch-1"], r.agents.states["rev-1"] = live("arch-1"), live("rev-1")
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); !maps.Equal(got, unread) {
		t.Errorf("events read failed: issue_holder = %v, want %v", got, unread)
	}
	r.records.holderEventsErr = nil
	r.records.roleHoldersErr = &dispatch.APIError{Status: 404, Code: "NOT_FOUND", Route: "role_holders"}
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); !maps.Equal(got, unread) {
		t.Errorf("role holders read failed: issue_holder = %v, want %v, not that Dispatch has no such issue", got, unread)
	}
	r.records.roleHoldersErr = nil
	r.records.holderEventsDelay, r.concierge.holderBound = time.Second, 50*time.Millisecond
	start := time.Now()
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); !maps.Equal(got, unread) || time.Since(start) > 500*time.Millisecond {
		t.Errorf("events slower than the bound: issue_holder = %v after %v, want %v at the bound", got, time.Since(start), unread)
	}

	// A running passer above every page read: it may have set the status in
	// an event older than the pages.
	events := []dispatch.Event{wrote("comment.created", testNow.Add(-time.Minute), "passer-1", "Coordinator")}
	events = append(events, personNotes(holderPage*holderPages, testNow.Add(-2*time.Minute))...)
	r = newRig(t)
	r.records.issues["ABC-12"] = &dispatch.Issue{Key: "ABC-12", Status: "in_progress"}
	r.records.events["ABC-12"] = events
	r.agents.states["passer-1"] = live("passer-1")
	if got := r.run(t, "issue_holder", map[string]string{"key": "ABC-12"}); !maps.Equal(got, unread) {
		t.Errorf("history past the pages: issue_holder = %v, want %v", got, unread)
	}
	if n := len(r.records.eventLimits()); n != holderPages {
		t.Errorf("%d pages read, want %d", n, holderPages)
	}
	r.model.turns = [][]*genai.GenerateContentResponse{{chunk(text("ABC-12 is in progress."))}}
	spoken := answerTurn(t, r, "ask the agent on ABC-12 whether the tap is merged")
	if len(r.agents.sent) != 0 || !slices.Contains(spoken, said{"I could not read who holds ABC-12.", voice.Fact}) {
		t.Errorf("spoken = %+v, sent %d; want that the holder could not be read, and nothing sent", spoken, len(r.agents.sent))
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
