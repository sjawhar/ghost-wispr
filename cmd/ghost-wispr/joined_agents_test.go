package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/joined"
)

// recordingAgents is the unit's Envoy session: lookups answer live, and sends
// are recorded.
type recordingAgents struct {
	mu   sync.Mutex
	sent []envoy.SendRequest
}

func (a *recordingAgents) ID() string { return "unit-session" }

func (a *recordingAgents) Lookup(_ context.Context, id string) (envoy.SessionInfo, error) {
	return envoy.SessionInfo{SessionID: id, State: envoy.StateLive, Title: "Worker " + id}, nil
}

func (a *recordingAgents) Send(_ context.Context, r *envoy.SendRequest) (envoy.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, *r)
	return envoy.SendResult{EventID: fmt.Sprintf("evt-%d", len(a.sent)), RecipientState: envoy.StateLive}, nil
}

// raisingVoice is a voice that records the hands raised and lowered on it.
type raisingVoice struct {
	closingsHeard
	mu      sync.Mutex
	raised  []*voice.Hand
	lowered []string
}

func (v *raisingVoice) RaiseHand(h *voice.Hand) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.raised = append(v.raised, h)
	return nil
}

func (v *raisingVoice) LowerHands(membership string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lowered = append(v.lowered, membership)
}

func testRoster(t *testing.T, link *voiceLink) *joined.Roster {
	t.Helper()
	roster := startJoinedAgents(t.Context(), &joinedDeps{
		session:   func() string { return "meeting" },
		store:     conciergeStore(t),
		floor:     link,
		notify:    func(string, string) {},
		publicURL: "https://recorder.example",
	})
	t.Cleanup(roster.Close)
	return roster
}

// TestTheEnvoySessionCarriesJoinsToTheRoster fails if the unit's Envoy
// session, as startConcierge opens it, does not hand a join to the joined
// agents' roster and every other message to the Concierge, if the Concierge
// does not list the joined agent, or if the agents stay joined once the
// session closes.
func TestTheEnvoySessionCarriesJoinsToTheRoster(t *testing.T) {
	agents := &recordingAgents{}
	session := &fakeEnvoySession{Agents: agents}
	delivers := make(chan func(context.Context, *envoy.Message) error, 1)
	original := openEnvoySession
	t.Cleanup(func() { openEnvoySession = original })
	openEnvoySession = func(_ context.Context, _ *envoy.Listener, cfg *envoy.SessionConfig) (envoySession, error) {
		delivers <- cfg.Deliver
		return session, nil
	}
	var link voiceLink
	roster := testRoster(t, &link)
	cfg := geminiKeyConfig("test")
	cfg.Concierge.EnvoyURL = "http://127.0.0.1:1"
	c, stop, err := startConcierge(t.Context(), &cfg, conciergeStore(t), discardLogger, nil, roster)
	if err != nil {
		t.Fatal(err)
	}
	var deliver func(context.Context, *envoy.Message) error
	select {
	case deliver = <-delivers:
	case <-time.After(2 * time.Second):
		t.Fatal("the Envoy session did not open")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// The roster takes the session just after Deliver is set; until then it
		// refuses the join with ErrNotReady, so Envoy delivers it again.
		err := deliver(t.Context(), &envoy.Message{EventID: "e1", SourceSession: "agent-1", Text: "ghost-wispr join"})
		if err != nil && !errors.Is(err, joined.ErrNotReady) {
			t.Fatalf("deliver the join: %v", err)
		}
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := roster.Joined(); len(got) != 1 || got[0].Agent != "agent-1" {
		t.Fatalf("roster %+v, want agent-1 joined", got)
	}
	if err := deliver(t.Context(), &envoy.Message{EventID: "e2", SourceSession: "agent-2", InReplyTo: "evt-x", Text: "It merged."}); err != nil {
		t.Fatalf("deliver a reply: %v", err)
	}
	if len(roster.Joined()) != 1 {
		t.Fatal("a reply that is no join changed the roster")
	}
	raw, err := c.Tools(&voice.Exchange{SessionID: "meeting", WakeAt: time.Now()}).Run(t.Context(), voice.ToolCall{Name: "list_joined_agents"})
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Agents []struct {
			Session string `json:"session"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil || len(listed.Agents) != 1 || listed.Agents[0].Session != "agent-1" {
		t.Fatalf("list_joined_agents = %s, want agent-1", raw)
	}
	stop(context.Background())
	if got := roster.Joined(); len(got) != 0 {
		t.Fatalf("roster %+v after the Envoy session closed, want none", got)
	}
}

// TestAJoinedAgentsHandReachesTheVoice fails if a hand the roster raises does
// not reach the voice, or is taken with no voice running.
func TestAJoinedAgentsHandReachesTheVoice(t *testing.T) {
	var link voiceLink
	if err := link.RaiseHand(&voice.Hand{ID: "h"}); !errors.Is(err, voice.ErrHandVoiceOff) {
		t.Fatalf("RaiseHand with no voice = %v, want ErrHandVoiceOff", err)
	}
	v := &raisingVoice{}
	link.set(v)
	if err := link.RaiseHand(&voice.Hand{ID: "h1"}); err != nil {
		t.Fatal(err)
	}
	link.LowerHands("agent-1#1")
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.raised) != 1 || v.raised[0].ID != "h1" || !slices.Equal(v.lowered, []string{"agent-1#1"}) {
		t.Fatalf("raised %+v, lowered %q; want h1 and agent-1", v.raised, v.lowered)
	}
}

// TestSessionChangesReachTheJoinedAgents fails if a session end or delete,
// wired as main wires them, does not reach the roster.
func TestSessionChangesReachTheJoinedAgents(t *testing.T) {
	runParallel(t)
	var link voiceLink
	link.set(&forgetting{})
	var hooks conciergeHooks
	var joinedLink joinedHooks
	other := &forgetting{}
	manager := &sessionHookHolder{}
	forgotten := wireVoiceSessionHooks(manager, &hooks, &link, &joinedLink, other)
	roster := testRoster(t, &link)
	agents := &recordingAgents{}
	roster.Open().SetAgents(agents)
	joinedLink.set(roster)
	join := func() {
		t.Helper()
		if handled, err := roster.Deliver(t.Context(), &envoy.Message{EventID: "e", SourceSession: "agent-1", Text: "ghost-wispr join"}); !handled || err != nil {
			t.Fatalf("join: %v %v", handled, err)
		}
	}
	join()
	manager.ended("meeting")
	if len(roster.Joined()) != 0 {
		t.Fatal("the agent stayed joined after its session ended")
	}
	join()
	forgotten([]string{"meeting"})
	if len(roster.Joined()) != 0 {
		t.Fatal("the agent stayed joined after its session was deleted")
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if !slices.Equal(other.ended, []string{"meeting"}) || !slices.Equal(other.deleted, []string{"meeting"}) {
		t.Fatalf("other hooks got ended %q deleted %q, want each once", other.ended, other.deleted)
	}
}

// TestTheAPIListsJoinedAgentsAndGivesTheJoinMessage fails if the API's hooks
// do not list the meeting's joined agents, refuse a bad key as one, or give a
// join message naming the unit's Envoy session.
func TestTheAPIListsJoinedAgentsAndGivesTheJoinMessage(t *testing.T) {
	runParallel(t)
	var link voiceLink
	roster := testRoster(t, &link)
	hooks := &server.ControlHooks{}
	wireJoinedRoutes(hooks, roster)
	if _, err := hooks.JoinMessage("ABC-12"); err == nil {
		t.Fatal("a join message with no Envoy session")
	}
	roster.Open().SetAgents(&recordingAgents{})
	if _, err := roster.Deliver(t.Context(), &envoy.Message{EventID: "e", SourceSession: "agent-1", Text: "ghost-wispr join"}); err != nil {
		t.Fatal(err)
	}
	if got := hooks.JoinedAgents("meeting"); len(got) != 1 || got[0].Agent != "agent-1" {
		t.Fatalf("joined agents %+v, want agent-1", got)
	}
	if got := hooks.JoinedAgents("other"); len(got) != 0 {
		t.Fatalf("another session's joined agents %+v, want none", got)
	}
	if _, err := hooks.JoinMessage("not a key"); !errors.Is(err, server.ErrBadIssueKey) {
		t.Fatalf("a bad key = %v, want ErrBadIssueKey", err)
	}
	message, err := hooks.JoinMessage("ABC-12")
	if err != nil || !containsAll(message, "unit-session", "https://recorder.example", "ghost-wispr join ABC-12") {
		t.Fatalf("join message %q, %v", message, err)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
