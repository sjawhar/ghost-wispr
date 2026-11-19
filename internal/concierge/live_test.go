package concierge

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// noModel is a model client that cannot be reached: the live round trip runs
// where the unit's Vertex identity is not, so the answer is spoken as written.
type noModel struct{}

func (noModel) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		yield(nil, errors.New("no model on this machine"))
	}
}

// TestLiveConciergeRoundTrip drives the Concierge's tools against a real
// Dispatch and a real Envoy hub: it searches for an issue by its
// description, reads who holds it, registers as an Envoy session, asks the
// target session one question and waits for its reply. It runs only when
// GHOST_WISPR_LIVE_CONCIERGE=1; it reads the endpoints, the token and the
// target from the environment:
//
//	GHOST_WISPR_DISPATCH_URL, GHOST_WISPR_DISPATCH_TOKEN, GHOST_WISPR_ENVOY_URL,
//	GHOST_WISPR_LIVE_QUERY, GHOST_WISPR_LIVE_ISSUE, GHOST_WISPR_LIVE_TARGET,
//	GHOST_WISPR_LIVE_QUESTION, and GHOST_WISPR_LIVE_WAIT (a duration, 20m by default).
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
	ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Minute)
	defer cancel()

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
	concierge, err := New(&Config{Generator: noModel{}, Records: records, Store: store})
	if err != nil {
		t.Fatal(err)
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
	asked := run("ask_agent", map[string]string{"session": need("GHOST_WISPR_LIVE_TARGET"), "question": need("GHOST_WISPR_LIVE_QUESTION"), "issue_key": issueKey})
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
