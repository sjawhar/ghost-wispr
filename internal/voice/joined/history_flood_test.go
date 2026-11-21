package joined

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// The tests here hold the command history to its sending sessions: one that
// floods can only make its own commands replayable, never get another
// agent's command refused or forgotten.

// sendLeave delivers a leave from agent with event id, as Envoy does, and
// fails the test if the roster refuses it.
func sendLeave(t *testing.T, r *rig, agent, id string) {
	t.Helper()
	m := &envoy.Message{EventID: id, SourceSession: agent, Text: "ghost-wispr leave", IssuedAt: r.clock.Now()}
	if _, err := r.roster.Deliver(context.Background(), m); err != nil {
		t.Fatalf("the leave %s from %s was refused: %v", id, agent, err)
	}
}

// isJoined reports whether agent has a membership.
func isJoined(r *rig, agent string) bool {
	for _, m := range r.roster.Joined() {
		if m.Agent == agent {
			return true
		}
	}
	return false
}

// TestAFloodingSessionCannotBlockAnotherAgent fails if one sending session's
// flood, more commands than the whole history holds, gets another agent's
// join refused, rather than taken at once (a refused join that Envoy keeps
// bringing back is lost once Envoy's window ends), forgets another agent's
// command taken before the flood, grows the flooding session's history past
// its cap, or gets the flood's own commands refused. Whether a join taken
// before the flood is still remembered is read after its agent leaves: a
// join delivered again to a member keeps its membership whether or not it
// acts, but delivered again after a leave it would join again. The flood
// forgets only its own oldest commands: its first join, delivered again,
// joins it again.
func TestAFloodingSessionCannotBlockAnotherAgent(t *testing.T) {
	r := newRig(t, func(cfg *Config) { cfg.Logf = func(string, ...any) {} })
	ctx := context.Background()
	before := r.deliver(t, "agent-b", "ghost-wispr join ABC-12")
	if !isJoined(r, "agent-b") {
		t.Fatalf("control: agent-b did not join: %+v", r.roster.Joined())
	}
	flooderJoin := r.deliver(t, "flooder", "ghost-wispr join XYZ-9")
	sendLeave(t, r, "flooder", "flood-leave")
	refused := 0
	for i := range maxCommands + 1 {
		m := &envoy.Message{EventID: fmt.Sprintf("flood-%d", i), SourceSession: "flooder", Text: "ghost-wispr leave", IssuedAt: r.clock.Now()}
		if _, err := r.roster.Deliver(ctx, m); err != nil {
			refused++
		}
	}
	join := &envoy.Message{EventID: "after-flood", SourceSession: "agent-c", Text: "ghost-wispr join INFRA-1027", IssuedAt: r.clock.Now()}
	if _, err := r.roster.Deliver(ctx, join); err != nil {
		t.Fatalf("another agent's join after the flood was refused: %v", err)
	}
	if !isJoined(r, "agent-c") {
		t.Fatalf("another agent's join after the flood was not taken: %+v", r.roster.Joined())
	}
	if refused > 0 {
		t.Fatalf("%d of the flood's own commands were refused: each would come back until Envoy's window ended, and be lost", refused)
	}
	sendLeave(t, r, "agent-b", "agent-b-leave")
	if isJoined(r, "agent-b") {
		t.Fatal("control: agent-b's leave did not end its membership")
	}
	if _, err := r.roster.Deliver(ctx, before); err != nil {
		t.Fatal(err)
	}
	if isJoined(r, "agent-b") {
		t.Fatal("agent-b's join, taken before the flood, acted again when delivered after its leave: the flood made it forget another agent's command")
	}
	r.roster.mu.Lock()
	kept := r.roster.commands.sessionSize("flooder")
	r.roster.mu.Unlock()
	if kept > perSessionCommands {
		t.Fatalf("the flooding session's history holds %d commands, past its cap of %d", kept, perSessionCommands)
	}
	if _, err := r.roster.Deliver(ctx, flooderJoin); err != nil {
		t.Fatal(err)
	}
	if !isJoined(r, "flooder") {
		t.Fatal("the flood's first join, delivered again, was still remembered: its session kept more than its cap")
	}
}

// TestTheBackstopForgetsTheLargestSessionsFirst fails if the history, at its
// bound over all sessions, refuses a command, grows past the bound, or
// forgets a command of a session holding few rather than of the sessions
// holding the most.
func TestTheBackstopForgetsTheLargestSessionsFirst(t *testing.T) {
	r := newRig(t, func(cfg *Config) { cfg.Logf = func(string, ...any) {} })
	const keeps = 10
	for i := range keeps {
		sendLeave(t, r, "keeper", fmt.Sprintf("keep-%d", i))
	}
	each := perSessionCommands / 2
	for s := range maxCommands / each {
		agent := fmt.Sprintf("crowd-%04d", s)
		for i := range each {
			sendLeave(t, r, agent, fmt.Sprintf("%s-%d", agent, i))
		}
	}
	r.roster.mu.Lock()
	defer r.roster.mu.Unlock()
	if n := r.roster.commands.size(); n != maxCommands {
		t.Fatalf("the history holds %d commands at its bound of %d", n, maxCommands)
	}
	for i := range keeps {
		if !r.roster.commands.has("keeper", commandKeyOf("keeper", fmt.Sprintf("keep-%d", i))) {
			t.Fatalf("the backstop forgot keep-%d of the session holding %d commands, while sessions held %d", i, keeps, each)
		}
	}
}
