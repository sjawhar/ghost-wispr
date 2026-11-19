package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// testNow is the clock every test runs at.
var testNow = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// meetingStart is when the test meeting's session started.
var meetingStart = testNow.Add(-20 * time.Minute)

type fakeRecords struct {
	mu         sync.Mutex
	hits       []dispatch.SearchHit
	issues     map[string]*dispatch.Issue
	events     map[string][]dispatch.Event
	docs       map[string]string
	err        error
	queries    []string
	issueCalls []string
	limits     []int
	// block, when set, holds each Issue call (of blockKey only, when that is
	// set) until it is closed or the call's context ends.
	block    chan struct{}
	blockKey string
	// projects is Dispatch's project keys; nil is a project list that does
	// not answer.
	projects []string
	// projectsBlock, when set, holds each Projects call until it is closed
	// or the call's context ends.
	projectsBlock chan struct{}
	// holderEventsErr, when set, is the answer of issue_holder's read of
	// events (holderEvents), and of no other read.
	holderEventsErr error
}

func (f *fakeRecords) Projects(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	block := f.projectsBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.projects == nil {
		return nil, &dispatch.APIError{Status: 503, Code: "UNAVAILABLE", Route: "projects"}
	}
	return append([]string(nil), f.projects...), nil
}

func (f *fakeRecords) Search(_ context.Context, query string, _ int) ([]dispatch.SearchHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	return f.hits, f.err
}

func (f *fakeRecords) Issue(ctx context.Context, key string) (*dispatch.Issue, error) {
	f.mu.Lock()
	f.issueCalls = append(f.issueCalls, key)
	block := f.block
	if f.blockKey != "" && key != f.blockKey {
		block = nil
	}
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	issue, ok := f.issues[key]
	if !ok {
		return nil, &dispatch.APIError{Status: 404, Code: "NOT_FOUND", Message: "not found", Route: "issue"}
	}
	return issue, nil
}

// searches is a copy of the queries Search was called with.
func (f *fakeRecords) searches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// calls is a copy of the keys Issue was called with.
func (f *fakeRecords) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issueCalls...)
}

func (f *fakeRecords) Events(_ context.Context, key string, limit int) ([]dispatch.Event, error) {
	f.mu.Lock()
	f.limits = append(f.limits, limit)
	holderErr := f.holderEventsErr
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if limit == holderEvents && holderErr != nil {
		return nil, holderErr
	}
	return f.events[key], nil
}

// eventLimits is a copy of the limits Events was called with.
func (f *fakeRecords) eventLimits() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.limits...)
}

func (f *fakeRecords) DocumentText(ctx context.Context, key, slug string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, ok := f.docs[key+"/"+slug]
	if !ok {
		return "", &dispatch.APIError{Status: 404, Code: "NOT_FOUND", Route: "document"}
	}
	return text, nil
}

type fakeAgents struct {
	mu     sync.Mutex
	id     string
	states map[string]envoy.SessionInfo
	sent   []envoy.SendRequest
	// attempts is every send asked, the failed ones too.
	attempts []envoy.SendRequest
	sendErr  error
	// landedErr, when set, is the answer of a send whose message Envoy
	// stored all the same.
	landedErr error
	// stored holds the idempotency key of each message stored. A send of a
	// stored key stores nothing and answers Duplicate, with an event id that
	// names no message, as Envoy does.
	stored map[string]bool
	// onSend runs inside Send before it answers, with Send's own context and
	// the event id it is about to answer.
	onSend func(ctx context.Context, eventID string) error
	// onLookup runs inside Lookup before it answers.
	onLookup func(id string)
	// lookupErr, when set, is the answer of every Lookup after the first
	// lookupsBeforeErr.
	lookupErr        error
	lookupsBeforeErr int
	lookups          int
}

func (f *fakeAgents) ID() string { return f.id }

func (f *fakeAgents) Lookup(_ context.Context, id string) (envoy.SessionInfo, error) {
	if f.onLookup != nil {
		f.onLookup(id)
	}
	f.mu.Lock()
	f.lookups++
	failing := f.lookupErr != nil && f.lookups > f.lookupsBeforeErr
	f.mu.Unlock()
	if failing {
		return envoy.SessionInfo{}, f.lookupErr
	}
	if info, ok := f.states[id]; ok {
		return info, nil
	}
	return envoy.SessionInfo{SessionID: id, State: envoy.StateGone}, nil
}

func (f *fakeAgents) Send(ctx context.Context, request *envoy.SendRequest) (envoy.SendResult, error) {
	f.mu.Lock()
	f.attempts = append(f.attempts, *request)
	if f.sendErr != nil {
		f.mu.Unlock()
		return envoy.SendResult{}, f.sendErr
	}
	if f.stored[request.IdempotencyKey] {
		f.mu.Unlock()
		return envoy.SendResult{EventID: fmt.Sprintf("unstored-%d", len(f.attempts)), RecipientState: envoy.StateLive, State: "connected", Duplicate: true}, nil
	}
	if f.stored == nil {
		f.stored = map[string]bool{}
	}
	f.stored[request.IdempotencyKey] = true
	f.sent = append(f.sent, *request)
	eventID := fmt.Sprintf("evt-%d", len(f.sent))
	onSend, landedErr := f.onSend, f.landedErr
	f.mu.Unlock()
	if landedErr != nil {
		return envoy.SendResult{}, landedErr
	}
	if onSend != nil {
		if err := onSend(ctx, eventID); err != nil {
			return envoy.SendResult{}, err
		}
	}
	return envoy.SendResult{EventID: eventID, RecipientState: envoy.StateLive, State: "connected"}, nil
}

// scriptedModel answers each GenerateContentStream call with the next turn's
// chunks, and records what it was sent.
type scriptedModel struct {
	mu       sync.Mutex
	turns    [][]*genai.GenerateContentResponse
	errs     []error
	requests [][]*genai.Content
	configs  []*genai.GenerateContentConfig
	models   []string
	// onCall runs at the start of each call, numbered from 0.
	onCall func(call int)
	// block, when set, holds every call until it is closed or the call's
	// context ends.
	block chan struct{}
}

func (m *scriptedModel) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	m.mu.Lock()
	call := len(m.requests)
	m.requests = append(m.requests, append([]*genai.Content(nil), contents...))
	m.configs = append(m.configs, config)
	m.models = append(m.models, model)
	var chunks []*genai.GenerateContentResponse
	if call < len(m.turns) {
		chunks = m.turns[call]
	}
	var err error
	if call < len(m.errs) {
		err = m.errs[call]
	}
	m.mu.Unlock()
	if m.onCall != nil {
		m.onCall(call)
	}
	block := m.block
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
		if err != nil {
			yield(nil, err)
			return
		}
		if call >= len(m.turns) {
			yield(nil, errors.New("scripted model: no turn left"))
		}
	}
}

func chunk(parts ...*genai.Part) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}}}}
}

func text(s string) *genai.Part { return &genai.Part{Text: s} }

func thought(s string) *genai.Part { return &genai.Part{Text: s, Thought: true} }

func call(id, name string, args map[string]any, signature string) *genai.Part {
	part := &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}
	if signature != "" {
		part.ThoughtSignature = []byte(signature)
	}
	return part
}

// rig is a Concierge on fakes and a real store holding one meeting.
type rig struct {
	concierge *Concierge
	records   *fakeRecords
	agents    *fakeAgents
	model     *scriptedModel
	store     *storage.SQLiteStore
	exchange  voice.Exchange
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("meeting-1", meetingStart); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		records: &fakeRecords{issues: map[string]*dispatch.Issue{}, events: map[string][]dispatch.Event{}, docs: map[string]string{}, projects: []string{"ABC", "XYZ", "DEF", "INFRA"}},
		agents:  &fakeAgents{id: "unit-session", states: map[string]envoy.SessionInfo{}},
		model:   &scriptedModel{},
		store:   store,
		exchange: voice.Exchange{
			SessionID: "meeting-1",
			WakeAt:    testNow.Add(-8 * time.Second),
			AskerTurn: "what is happening with the voice work",
		},
	}
	r.concierge, err = New(&Config{
		Model:     "test-model",
		Generator: r.model,
		Records:   r.records,
		Store:     store,
		PublicURL: "https://unit.example",
		Now:       func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.concierge.SetAgents(r.agents)
	eventually(t, "the project list was read", func() bool {
		r.concierge.mu.Lock()
		defer r.concierge.mu.Unlock()
		return r.concierge.projects != nil
	})
	return r
}

// run calls one tool through the exchange's runner and decodes its result. An
// answer that says Dispatch or Envoy is out of reach, and only such, comes
// with an error that wraps voice.ErrUnreachable.
func (r *rig) run(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.concierge.Tools(&r.exchange).Run(context.Background(), voice.ToolCall{ID: "call-1", Name: name, Args: raw})
	if err != nil && !errors.Is(err, voice.ErrUnreachable) {
		t.Fatalf("Run(%s): %v", name, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("Run(%s) answered no JSON object: %s", name, result)
	}
	if (err != nil) != outOfReach(decoded) {
		t.Fatalf("Run(%s) = %s, error %v: want voice.ErrUnreachable with an out-of-reach answer, and only with one", name, result, err)
	}
	return decoded
}

// name makes issue_holder name agent session id, live, in r's exchange, as
// ask_agent requires before it asks that session.
func (r *rig) name(t *testing.T, id string) {
	t.Helper()
	key := fmt.Sprintf("NAMED-%d", len(r.records.issues)+1)
	r.records.issues[key] = claimed(key, "session", id, "Agent "+id)
	if _, ok := r.agents.states[id]; !ok {
		r.agents.states[id] = envoy.SessionInfo{SessionID: id, State: envoy.StateLive}
	}
	if holder := r.run(t, "issue_holder", map[string]string{"key": key}); holder["session"] != id {
		t.Fatalf("issue_holder did not name %s: %v", id, holder)
	}
}

// ask names agent session id and asks it question.
func (r *rig) ask(t *testing.T, id, question, issueKey string) map[string]any {
	t.Helper()
	r.name(t, id)
	args := map[string]string{"session": id, "question": question}
	if issueKey != "" {
		args["issue_key"] = issueKey
	}
	return r.run(t, "ask_agent", args)
}

// captureLogs sends the Concierge's log to a buffer the test reads.
func (r *rig) captureLogs() *lockedBuffer {
	buffer := &lockedBuffer{}
	r.concierge.logger = slog.New(slog.NewTextHandler(buffer, nil))
	return buffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
