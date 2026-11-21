package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// JoinedAgent is an agent that joined a meeting (R17), as the meeting's page
// lists it: its Envoy session, its issue, the issue's title, and when it
// joined.
type JoinedAgent struct {
	Agent    string    `json:"agent"`
	IssueKey string    `json:"issue_key"`
	Title    string    `json:"title"`
	JoinedAt time.Time `json:"joined_at"`
}

// ErrBadIssueKey is ControlHooks.JoinMessage's answer for a key that is not
// one.
var ErrBadIssueKey = errors.New("that is not an issue key; an issue key looks like ABC-12")

// registerJoinedAgentsRoutes serves the joined agents of a meeting, and the
// message a person sends an agent through Dispatch to have it join.
func registerJoinedAgentsRoutes(mux routeMux, controls *ControlHooks) {
	mux.HandleFunc("GET /api/sessions/{id}/joined-agents", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !storage.ValidSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}
		agents := []JoinedAgent{}
		if controls.JoinedAgents != nil {
			agents = append(agents, controls.JoinedAgents(sessionID)...)
		}
		writeJSON(w, http.StatusOK, agents)
	})

	mux.HandleFunc("GET /api/voice/join-message", func(w http.ResponseWriter, r *http.Request) {
		if controls.JoinMessage == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "the meeting voice is not running, so no agent can join")
			return
		}
		message, err := controls.JoinMessage(strings.TrimSpace(r.URL.Query().Get("issue")))
		switch {
		case errors.Is(err, ErrBadIssueKey):
			writeJSONError(w, http.StatusBadRequest, ErrBadIssueKey.Error())
		case err != nil:
			writeJSONError(w, http.StatusServiceUnavailable, "no agent can join now: "+err.Error())
		default:
			writeJSON(w, http.StatusOK, map[string]string{"message": message})
		}
	})
}
