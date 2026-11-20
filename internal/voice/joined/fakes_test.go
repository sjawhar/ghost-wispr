package joined

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

const testWait = 2 * time.Second

// fakeAgents is the unit's Envoy session: each send is recorded, and each
// lookup answers the state the test set, live when it set none.
type fakeAgents struct {
	mu     sync.Mutex
	states map[string]envoy.SessionInfo
	sent   []envoy.SendRequest
}

func newFakeAgents() *fakeAgents { return &fakeAgents{states: map[string]envoy.SessionInfo{}} }

func (f *fakeAgents) ID() string { return "unit-session" }

func (f *fakeAgents) Lookup(_ context.Context, id string) (envoy.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.states[id]; ok {
		return info, nil
	}
	return envoy.SessionInfo{SessionID: id, State: envoy.StateLive, Title: "Agent " + id}, nil
}

func (f *fakeAgents) Send(_ context.Context, request *envoy.SendRequest) (envoy.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, *request)
	return envoy.SendResult{EventID: fmt.Sprintf("evt-out-%d", len(f.sent)), RecipientState: envoy.StateLive, State: "connected"}, nil
}

func (f *fakeAgents) setState(id string, info envoy.SessionInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[id] = info
}

// to returns the messages sent to agent, in order.
func (f *fakeAgents) to(agent string) []envoy.SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []envoy.SendRequest
	for _, m := range f.sent {
		if m.Target == agent {
			out = append(out, m)
		}
	}
	return out
}

// fakeFloor is the Supervisor as the roster raises hands on it.
type fakeFloor struct {
	mu      sync.Mutex
	raised  []*voice.Hand
	lowered []string
	err     error
}

func (f *fakeFloor) RaiseHand(h *voice.Hand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.raised = append(f.raised, h)
	return nil
}

func (f *fakeFloor) LowerHands(agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lowered = append(f.lowered, agent)
}

func (f *fakeFloor) hands() []*voice.Hand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.raised)
}

func (f *fakeFloor) loweredFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lowered)
}

// notes records the /ws notifications and the keyterms the roster set.
type notes struct {
	mu       sync.Mutex
	events   []string
	keyterms [][]string
}

func (n *notes) notify(kind, sessionID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, kind+":"+sessionID)
}

func (n *notes) setKeyterms(keys []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.keyterms = append(n.keyterms, slices.Clone(keys))
}

func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.events)
}

func (n *notes) lastKeyterms() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.keyterms) == 0 {
		return nil
	}
	return n.keyterms[len(n.keyterms)-1]
}

// testClock is a test clock the roster reads.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *testClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

// rig is a roster on fakes, with meeting sess-1 recording since start.
type rig struct {
	roster  *Roster
	agents  *fakeAgents
	floor   *fakeFloor
	notes   *notes
	clock   *testClock
	mu      sync.Mutex
	session string
}

var meetingStart = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// titles are the invented issues' titles Dispatch answers.
var titles = map[string]string{
	"ABC-12":     "Agents join a meeting and ask for the floor",
	"XYZ-9":      "Rotate the signing keys",
	"INFRA-1027": "An office meeting recorder with a light that shows it records, and their ghost voice",
}

func newRig(t *testing.T, change func(*Config)) *rig {
	t.Helper()
	r := &rig{agents: newFakeAgents(), floor: &fakeFloor{}, notes: &notes{}, clock: &testClock{now: meetingStart.Add(5 * time.Minute)}, session: "sess-1"}
	cfg := &Config{
		Session: func() string {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.session
		},
		Started: func(string) time.Time { return meetingStart },
		Floor:   r.floor,
		Title: func(_ context.Context, key string) (string, error) {
			if title, ok := titles[key]; ok {
				return title, nil
			}
			return "", ErrNoSuchIssue
		},
		Notify:    r.notes.notify,
		Keyterms:  r.notes.setKeyterms,
		PublicURL: "https://recorder.example",
		Now:       r.clock.Now,
		Logf:      t.Logf,
	}
	if change != nil {
		change(cfg)
	}
	r.roster = New(cfg)
	r.roster.SetAgents(r.agents)
	t.Cleanup(r.roster.Close)
	return r
}

func (r *rig) setSession(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = id
}

var events atomic.Int64

// deliver hands the roster one Envoy message from agent, issued now on the
// roster's clock, and fails unless the roster took it.
func (r *rig) deliver(t *testing.T, agent, text string) *envoy.Message {
	t.Helper()
	m := &envoy.Message{EventID: fmt.Sprintf("evt-in-%d", events.Add(1)), SourceSession: agent, Text: text, IssuedAt: r.clock.Now()}
	handled, err := r.roster.Deliver(context.Background(), m)
	if err != nil || !handled {
		t.Fatalf("Deliver(%q) = %v, %v; want it taken", text, handled, err)
	}
	return m
}

// join joins agent on key, and waits for the roster's answer to it.
func (r *rig) join(t *testing.T, agent, key string) {
	t.Helper()
	before := len(r.agents.to(agent))
	r.deliver(t, agent, "ghost-wispr join "+key)
	eventually(t, "the answer to the join", func() bool { return len(r.agents.to(agent)) > before })
	if !slices.ContainsFunc(r.roster.Joined(), func(m Member) bool { return m.Agent == agent }) {
		t.Fatalf("%s is not on the roster after its join: %+v", agent, r.roster.Joined())
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pointers returns the pointers agent was sent.
func (r *rig) pointers(agent string) []envoy.SendRequest {
	var out []envoy.SendRequest
	for _, m := range r.agents.to(agent) {
		if strings.Contains(m.Message, "came up in the meeting") {
			out = append(out, m)
		}
	}
	return out
}

// segment is a final segment of the room's in meeting sess-1, said over the
// 3 s before the roster's now.
func (r *rig) segment(text string, call bool) *voice.Segment {
	now := r.clock.Now()
	return &voice.Segment{SessionID: "sess-1", Text: text, Call: call, From: now.Add(-3 * time.Second), To: now}
}
