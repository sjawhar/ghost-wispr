package joined

import (
	"context"
	"encoding/json"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestCallGoesOnce fails if a call to Wispr that names a joined agent's issue
// ("Wispr, ask the ABC-12 agent …") reaches that agent as anything but one
// ask_agent question: no pointer for the call, and issue_holder names the
// joined agent before the issue's claim.
func TestCallGoesOnce(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	afterJoin := len(r.agents.to("agent-1"))

	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("sess-1", meetingStart); err != nil {
		t.Fatal(err)
	}
	// Dispatch says another session holds ABC-12: the joined agent goes
	// first.
	records := &claimedRecords{key: "ABC-12", holder: "claim-holder"}
	c, err := concierge.New(&concierge.Config{
		Generator: idleModel{},
		Records:   records,
		Store:     store,
		Joined: func() []concierge.JoinedAgent {
			var agents []concierge.JoinedAgent
			for _, m := range r.roster.Joined() {
				agents = append(agents, concierge.JoinedAgent{SessionID: m.Agent, Title: m.Title, IssueKey: m.IssueKey})
			}
			return agents
		},
		Now: r.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	c.SetAgents(r.agents)

	call := "Wispr, ask the ABC-12 agent what is left before the release?"
	r.roster.OnSegment(r.segment(call, true))
	x := &voice.Exchange{SessionID: "sess-1", WakeAt: r.clock.Now(), AskerTurn: "ask the ABC-12 agent what is left before the release?"}
	tools := c.Tools(x)
	holder := runTool(t, tools, "issue_holder", map[string]string{"key": "ABC-12"})
	if holder["session"] != "agent-1" || holder["holder"] != "session" {
		t.Fatalf("issue_holder = %v, want the joined agent-1, live", holder)
	}
	asked := runTool(t, tools, "ask_agent", map[string]string{"session": "agent-1", "question": "What is left before the release?", "issue_key": "ABC-12"})
	if asked["task_id"] == nil {
		t.Fatalf("ask_agent = %v, want a task", asked)
	}

	never(t, "a pointer for the call", 200*time.Millisecond, func() bool { return len(r.pointers("agent-1")) > 0 })
	sent := r.agents.to("agent-1")[afterJoin:]
	if len(sent) != 1 || !strings.Contains(sent[0].Message, "What is left before the release?") {
		t.Fatalf("the call sent agent-1 %d messages %+v, want one ask_agent question", len(sent), sent)
	}
	if got := r.agents.to("claim-holder"); len(got) != 0 {
		t.Fatalf("the claim's holder was asked too: %+v", got)
	}
}

func runTool(t *testing.T, tools voice.ToolRunner, name string, args map[string]string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	result, err := tools.Run(context.Background(), voice.ToolCall{ID: "call-1", Name: name, Args: raw})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("%s answered %s", name, result)
	}
	return decoded
}

// claimedRecords is Dispatch with one issue, key, which session holder
// claims.
type claimedRecords struct{ key, holder string }

func (f *claimedRecords) Search(context.Context, string, int) ([]dispatch.SearchHit, error) {
	return nil, nil
}

func (f *claimedRecords) SearchKeywords(context.Context, string, int) ([]dispatch.SearchHit, error) {
	return nil, nil
}

func (f *claimedRecords) Issue(_ context.Context, key string) (*dispatch.Issue, error) {
	if key != f.key {
		return nil, &dispatch.APIError{Status: 404, Code: "NOT_FOUND", Route: "issue"}
	}
	issue := &dispatch.Issue{Key: key, Title: titles[key]}
	issue.Claim = &dispatch.Claim{}
	issue.Claim.Actor.Kind, issue.Claim.Actor.ID = "session", f.holder
	return issue, nil
}

func (f *claimedRecords) Events(context.Context, string, int) ([]dispatch.Event, error) {
	return nil, nil
}

func (f *claimedRecords) RoleHolders(context.Context, string) ([]dispatch.RoleHolder, error) {
	return nil, nil
}

func (f *claimedRecords) DocumentText(context.Context, string, string) (string, error) {
	return "", &dispatch.APIError{Status: 404, Code: "NOT_FOUND", Route: "document"}
}

func (f *claimedRecords) Projects(context.Context) ([]string, error) { return []string{"ABC"}, nil }

// idleModel is a model the test never calls.
type idleModel struct{}

func (idleModel) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(func(*genai.GenerateContentResponse, error) bool) {}
}
