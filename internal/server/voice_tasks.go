package server

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// voiceTaskView is one question the voice sent an agent as the session's
// page shows it: the issue, the agent that holds it, where the question
// stands, when it was asked and its answer spoken, and the agent's answer as
// written.
type voiceTaskView struct {
	ID            string     `json:"id"`
	IssueKey      string     `json:"issue_key"`
	Holder        string     `json:"holder"`
	State         string     `json:"state"`
	AskedAt       time.Time  `json:"asked_at"`
	SpokenAt      *time.Time `json:"spoken_at,omitempty"`
	AnswerWritten string     `json:"answer_written"`
}

// registerVoiceTasksRoute serves GET /api/sessions/{id}/voice-tasks: the
// questions the voice sent agents in the session and in the sessions merged
// into it, whose page it now is, oldest first.
func registerVoiceTasksRoute(mux *http.ServeMux, store SessionStore) {
	mux.HandleFunc("GET /api/sessions/{id}/voice-tasks", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		if !storage.ValidSessionID(sessionID) {
			writeJSONError(w, http.StatusForbidden, "invalid session id")
			return
		}
		if _, err := store.GetSession(sessionID); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
			}
			writeJSONError(w, status, fmt.Sprintf("get session: %v", err))
			return
		}
		sources, err := store.SourceSessionIDs(sessionID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("list merged sessions: %v", err))
			return
		}

		var tasks []storage.VoiceTask
		for _, id := range append([]string{sessionID}, sources...) {
			of, err := store.VoiceTasks(id)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("list voice tasks: %v", err))
				return
			}
			tasks = append(tasks, of...)
		}
		slices.SortFunc(tasks, func(a, b storage.VoiceTask) int {
			return cmp.Or(a.AskedAt.Compare(b.AskedAt), cmp.Compare(a.ID, b.ID))
		})

		views := make([]voiceTaskView, 0, len(tasks))
		for i := range tasks {
			views = append(views, voiceTaskView{
				ID:            tasks[i].ID,
				IssueKey:      tasks[i].IssueKey,
				Holder:        tasks[i].Holder,
				State:         string(tasks[i].State),
				AskedAt:       tasks[i].AskedAt,
				SpokenAt:      tasks[i].SpokenAt,
				AnswerWritten: tasks[i].AnswerWritten,
			})
		}
		writeJSON(w, http.StatusOK, views)
	})
}
