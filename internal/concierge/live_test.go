package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// noModel is a model client that cannot be reached, for a run where no
// Vertex identity is: the answer is then spoken as written.
type noModel struct{}

func (noModel) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		yield(nil, errors.New("no model on this machine"))
	}
}

// TestLiveConciergeRoundTrip drives the Concierge against a real Dispatch,
// a real Envoy hub and, when GHOST_WISPR_LIVE_GCP_PROJECT names a project,
// real Gemini on Vertex (Google's default credentials). With
// GHOST_WISPR_LIVE_TURN it answers that spoken turn twice with the model,
// before the Envoy session exists so the turn can ask no agent: cold, and
// with Heard told the turn word by word while it is "spoken", and logs how
// long after the turn's end each first sentence came. With
// GHOST_WISPR_LIVE_TARGET it then searches for an issue by its description,
// reads who holds it, registers as an Envoy session, asks the target session
// one question, waits for its reply and reads the answer rewritten for the
// ear. It runs only when GHOST_WISPR_LIVE_CONCIERGE=1, and reads everything
// from the environment:
//
//	GHOST_WISPR_DISPATCH_URL, GHOST_WISPR_DISPATCH_TOKEN, GHOST_WISPR_ENVOY_URL,
//	GHOST_WISPR_LIVE_QUERY, GHOST_WISPR_LIVE_ISSUE, GHOST_WISPR_LIVE_TARGET,
//	GHOST_WISPR_LIVE_QUESTION, GHOST_WISPR_LIVE_TURN, GHOST_WISPR_LIVE_GCP_PROJECT,
//	GHOST_WISPR_LIVE_GCP_LOCATION (global by default), GHOST_WISPR_LIVE_MODEL
//	(DefaultModel by default) and GHOST_WISPR_LIVE_WAIT (20m by default).
//
// The target is asked as a joined agent of the meeting: ask_agent asks only a
// session the exchange's tools named.
func TestLiveConciergeRoundTrip(t *testing.T) {
	if os.Getenv("GHOST_WISPR_LIVE_CONCIERGE") != "1" {
		t.Skip("set GHOST_WISPR_LIVE_CONCIERGE=1 to run against real Dispatch and Envoy")
	}
	need := func(name string) string {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("%s is not set", name)
		}
		return value
	}
	wait := 20 * time.Minute
	if raw := os.Getenv("GHOST_WISPR_LIVE_WAIT"); raw != "" {
		var err error
		if wait, err = time.ParseDuration(raw); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait+5*time.Minute)
	defer cancel()

	var generator Generator = noModel{}
	if project := os.Getenv("GHOST_WISPR_LIVE_GCP_PROJECT"); project != "" {
		location := os.Getenv("GHOST_WISPR_LIVE_GCP_LOCATION")
		if location == "" {
			location = "global"
		}
		var err error
		if generator, err = NewGenerator(ctx, &genaiconfig.Options{Backend: genaiconfig.BackendVertex, Project: project, Location: location}); err != nil {
			t.Fatalf("build the Gemini client: %v", err)
		}
	}
	records, err := dispatch.NewClient(need("GHOST_WISPR_DISPATCH_URL"), need("GHOST_WISPR_DISPATCH_TOKEN"), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	meeting := "live-proof-" + time.Now().UTC().Format("20060102T150405")
	if err := store.CreateSession(meeting, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSpace(os.Getenv("GHOST_WISPR_LIVE_TARGET"))
	concierge, err := New(&Config{
		Model: os.Getenv("GHOST_WISPR_LIVE_MODEL"), Generator: generator, Records: records, Store: store,
		Joined: func() []JoinedAgent {
			if target == "" {
				return nil
			}
			return []JoinedAgent{{SessionID: target, Title: "the live test's target"}}
		},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	turn := os.Getenv("GHOST_WISPR_LIVE_TURN")
	if turn == "" && target == "" {
		t.Skip("set GHOST_WISPR_LIVE_TURN, GHOST_WISPR_LIVE_TARGET or both")
	}
	if turn != "" {
		// One warm-up turn opens the connections to Vertex and Dispatch and
		// is not counted; then cold and read-ahead runs alternate, so neither
		// always goes first.
		runs := []struct {
			label string
			heard bool
		}{{"warm-up", false}}
		for rep := range 3 {
			first, second := false, true
			if rep%2 == 1 {
				first, second = true, false
			}
			runs = append(runs, struct {
				label string
				heard bool
			}{fmt.Sprintf("run %d", rep+1), first}, struct {
				label string
				heard bool
			}{fmt.Sprintf("run %d", rep+1), second})
		}
		for _, run := range runs {
			x := &voice.Exchange{SessionID: meeting, WakeAt: time.Now()}
			if run.heard {
				// The asker says the turn at about three words a second.
				words := strings.Fields(turn)
				for i := range words {
					concierge.Heard(ctx, x, strings.Join(words[:i+1], " "))
					time.Sleep(330 * time.Millisecond)
				}
			}
			var spoken []string
			ended := time.Now()
			err := concierge.Answer(ctx, x, turn, func(_ context.Context, text string, mode voice.SayMode) error {
				if len(spoken) == 0 {
					t.Logf("%s heard=%v first sentence (%s) %s after the turn: %q", run.label, run.heard, mode, time.Since(ended).Round(time.Millisecond), text)
				}
				spoken = append(spoken, text)
				return nil
			})
			if err != nil {
				t.Fatalf("Answer(%q): %v", turn, err)
			}
			if len(spoken) == 0 || spoken[0] == sorry {
				t.Fatalf("the turn said %q", spoken)
			}
		}
	}
	if target == "" {
		return
	}

	state, err := LoadEnvoyState(store)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := envoy.NewListener(need("GHOST_WISPR_ENVOY_URL"), nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := envoy.OpenSession(ctx, listener, &envoy.SessionConfig{
		SessionID: state.SessionID, Resume: state.Resume, SaveResume: state.SaveResume,
		Title: "Ghost Wispr concierge live test", Dir: "/ghost-wispr", Host: "devbox", Plugin: "ghost-wispr", PluginVersion: "live-test",
		Deliver: concierge.Deliver,
	})
	if err != nil {
		t.Fatalf("open the Envoy session: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := session.Close(closeCtx); err != nil {
			t.Logf("close the Envoy session: %v", err)
		}
	}()
	concierge.SetAgents(session)
	t.Logf("registered as Envoy session %s", session.ID())
	if info, err := listener.Lookup(ctx, session.ID()); err != nil || info.State != envoy.StateLive {
		t.Fatalf("the registry does not show this session live: %+v, %v", info, err)
	} else {
		t.Logf("registry: %s is %s", session.ID(), info.State)
	}

	tools := concierge.Tools(&voice.Exchange{SessionID: meeting, WakeAt: time.Now().Add(-5 * time.Second)})
	run := func(name string, args map[string]string) map[string]any {
		raw, _ := json.Marshal(args)
		result, err := tools.Run(ctx, voice.ToolCall{ID: name, Name: name, Args: raw})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s(%s) = %s", name, raw, result)
		var decoded map[string]any
		if err := json.Unmarshal(result, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["error"] != nil {
			t.Fatalf("%s answered an error: %v", name, decoded["error"])
		}
		return decoded
	}

	issueKey := need("GHOST_WISPR_LIVE_ISSUE")
	found := run("search_issues", map[string]string{"query": need("GHOST_WISPR_LIVE_QUERY")})
	if !strings.Contains(mustJSON(t, found), `"key":"`+issueKey+`"`) {
		t.Fatalf("the search did not find %s", issueKey)
	}
	run("issue_holder", map[string]string{"key": issueKey})
	run("list_joined_agents", map[string]string{})
	asked := run("ask_agent", map[string]string{"session": target, "question": need("GHOST_WISPR_LIVE_QUESTION"), "issue_key": issueKey})
	taskID, _ := asked["task_id"].(string)
	t.Logf("asked at %s; waiting up to %s for the reply", time.Now().UTC().Format(time.RFC3339), wait)

	select {
	case answer := <-concierge.Answers():
		t.Logf("answer landed at %s: task %s, meeting %s, issue %s, spoken text %q", time.Now().UTC().Format(time.RFC3339), answer.TaskID, answer.SessionID, answer.IssueKey, answer.Text)
		if answer.TaskID != taskID {
			t.Fatalf("the answer is for task %s, want %s", answer.TaskID, taskID)
		}
	case <-time.After(wait):
		t.Fatalf("no reply within %s", wait)
	}
	status := run("task_status", map[string]string{"id": taskID})
	if status["state"] != "done" {
		t.Fatalf("task_status = %v, want done", status)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
