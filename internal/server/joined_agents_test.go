package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func serveJoined(t *testing.T, hooks *ControlHooks, path string) *httptest.ResponseRecorder {
	t.Helper()
	store := &apiStoreStub{sessions: map[string]storage.Session{"s1": {ID: "s1", Status: storage.SessionActive}}}
	h, err := Handler(testStaticFS(t), NewHub(), store, hooks, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

// TestJoinedAgentsListEachAgentOfTheMeeting (R17) fails if GET
// /api/sessions/{id}/joined-agents does not list the agents that joined the
// session, with their issue, title and join time, or lists any with no
// joined-agents hook.
func TestJoinedAgentsListEachAgentOfTheMeeting(t *testing.T) {
	joinedAt := time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC)
	hooks := &ControlHooks{JoinedAgents: func(sessionID string) []JoinedAgent {
		if sessionID != "s1" {
			return nil
		}
		return []JoinedAgent{{Agent: "agent-1", IssueKey: "ABC-12", Title: "Agents join a meeting", JoinedAt: joinedAt}}
	}}
	rr := serveJoined(t, hooks, "/api/sessions/s1/joined-agents")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rr.Code, rr.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rr.Body, err)
	}
	if len(got) != 1 || got[0]["agent"] != "agent-1" || got[0]["issue_key"] != "ABC-12" || got[0]["title"] != "Agents join a meeting" || got[0]["joined_at"] != "2026-11-19T10:05:00Z" {
		t.Fatalf("joined agents %v, want agent-1 on ABC-12 with its title and join time", got)
	}
	for _, hooks := range []*ControlHooks{{}, hooks} {
		rr := serveJoined(t, hooks, "/api/sessions/s2/joined-agents")
		if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[]" {
			t.Fatalf("another session's joined agents: %d %s, want 200 []", rr.Code, rr.Body)
		}
	}
	if rr := serveJoined(t, hooks, "/api/sessions/..%2f/joined-agents"); rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("a bad session id answered %d", rr.Code)
	}
}

// TestJoinMessageForAnAgent (R17) fails if GET /api/voice/join-message does
// not give the message a person sends an agent to join, or does not say why
// there is none.
func TestJoinMessageForAnAgent(t *testing.T) {
	hooks := &ControlHooks{JoinMessage: func(key string) (string, error) {
		switch key {
		case "ABC-12":
			return "Join the office meeting as the agent of ABC-12.", nil
		case "bad":
			return "", ErrBadIssueKey
		default:
			return "", errors.New("the unit has no Envoy session")
		}
	}}
	rr := serveJoined(t, hooks, "/api/voice/join-message?issue=ABC-12")
	var got map[string]string
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got["message"] != "Join the office meeting as the agent of ABC-12." {
		t.Fatalf("join message: %d %s", rr.Code, rr.Body)
	}
	if rr := serveJoined(t, hooks, "/api/voice/join-message?issue=bad"); rr.Code != http.StatusBadRequest {
		t.Fatalf("a bad key answered %d %s, want 400", rr.Code, rr.Body)
	}
	if rr := serveJoined(t, hooks, "/api/voice/join-message?issue=XYZ-9"); rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "no Envoy session") {
		t.Fatalf("no Envoy session answered %d %s, want 503 with the reason", rr.Code, rr.Body)
	}
	if rr := serveJoined(t, &ControlHooks{}, "/api/voice/join-message"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no meeting voice answered %d, want 503", rr.Code)
	}
}

// TestOpenAPISpecDocumentsJoinedAgents fails if the two routes are not in the
// API's description.
func TestOpenAPISpecDocumentsJoinedAgents(t *testing.T) {
	for _, path := range []string{"/api/sessions/{id}/joined-agents", "/api/voice/join-message"} {
		if get, _ := dig(t, OpenAPISpec(), "paths", path, "get").(map[string]any); get == nil {
			t.Errorf("GET %s is not in the OpenAPI spec", path)
		}
	}
}
