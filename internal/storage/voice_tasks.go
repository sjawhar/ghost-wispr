package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// VoiceTaskState is where a question the voice sent an agent stands.
type VoiceTaskState string

const (
	// VoiceTaskWorking: sent, no answer yet.
	VoiceTaskWorking VoiceTaskState = "working"
	// VoiceTaskDone: the agent's answer landed.
	VoiceTaskDone VoiceTaskState = "done"
	// VoiceTaskCancelled: the exchange that asked ended before an answer.
	VoiceTaskCancelled VoiceTaskState = "cancelled"
	// VoiceTaskReplaced: a later question to the same agent took its place.
	VoiceTaskReplaced VoiceTaskState = "replaced"
)

// VoiceTask is one row of the voice_tasks ledger. It holds no words of the
// room: the asker's words went only in the message sent to the agent. Its ID
// is the Envoy event id of that message once it is sent (RekeyVoiceTask), so
// an answer that arrives after a restart still finds it by in_reply_to.
// AnswerWritten is the agent's answer as written, which the session's page
// shows; it is erased with its session.
type VoiceTask struct {
	ID            string
	SessionID     string
	AskedAt       time.Time
	IssueKey      string
	Holder        string
	State         VoiceTaskState
	AnswerWritten string
	SpokenAt      *time.Time
}

const voiceTaskColumns = `id, session_id, asked_at, issue_key, holder, state, answer_written, spoken_at`

// CreateVoiceTask records a working task. Its session must exist.
func (s *SQLiteStore) CreateVoiceTask(task *VoiceTask) error {
	if _, err := s.db.Exec(`INSERT INTO voice_tasks(id, session_id, asked_at, issue_key, holder, state) VALUES(?, ?, ?, ?, ?, ?)`,
		task.ID, task.SessionID, task.AskedAt.UTC().Format(time.RFC3339Nano), task.IssueKey, task.Holder, VoiceTaskWorking); err != nil {
		return fmt.Errorf("record voice task %s of session %s: %w", task.ID, task.SessionID, err)
	}
	return nil
}

// RekeyVoiceTask gives task id its new id: the event id Envoy gave the
// question it carried.
func (s *SQLiteStore) RekeyVoiceTask(id, newID string) error {
	result, err := s.db.Exec(`UPDATE voice_tasks SET id = ? WHERE id = ?`, newID, id)
	if err != nil {
		return fmt.Errorf("rekey voice task %s: %w", id, err)
	}
	return oneRow(result, "rekey voice task "+id)
}

// DeleteVoiceTask removes a task whose question was never sent.
func (s *SQLiteStore) DeleteVoiceTask(id string) error {
	if _, err := s.db.Exec(`DELETE FROM voice_tasks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete voice task %s: %w", id, err)
	}
	return nil
}

// VoiceTask reads one task; sql.ErrNoRows when there is none.
func (s *SQLiteStore) VoiceTask(id string) (VoiceTask, error) {
	return scanVoiceTask(s.db.QueryRow(`SELECT `+voiceTaskColumns+` FROM voice_tasks WHERE id = ?`, id))
}

// VoiceTasks lists a session's tasks, oldest first.
func (s *SQLiteStore) VoiceTasks(sessionID string) ([]VoiceTask, error) {
	rows, err := s.db.Query(`SELECT `+voiceTaskColumns+` FROM voice_tasks WHERE session_id = ? ORDER BY asked_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list voice tasks of session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	tasks := []VoiceTask{}
	for rows.Next() {
		task, err := scanVoiceTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list voice tasks of session %s: %w", sessionID, err)
	}
	return tasks, nil
}

// AnswerVoiceTask keeps the agent's written answer. It reports true when the
// task was working and is now done. A task that already has an answer keeps
// it. A replaced or cancelled task keeps the answer for the session's page
// and stays as it is, so nobody is told of it again.
func (s *SQLiteStore) AnswerVoiceTask(id, written string) (bool, error) {
	result, err := s.db.Exec(`UPDATE voice_tasks
		SET answer_written = ?, state = CASE WHEN state = ? THEN ? ELSE state END
		WHERE id = ? AND answer_written = ''`,
		written, VoiceTaskWorking, VoiceTaskDone, id)
	if err != nil {
		return false, fmt.Errorf("answer voice task %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("answer voice task %s: %w", id, err)
	}
	task, err := s.VoiceTask(id)
	if err != nil {
		return false, err
	}
	return changed == 1 && task.State == VoiceTaskDone, nil
}

// EndVoiceTask moves a working task to replaced or cancelled, and reports
// whether it was working.
func (s *SQLiteStore) EndVoiceTask(id string, state VoiceTaskState) (bool, error) {
	if state != VoiceTaskReplaced && state != VoiceTaskCancelled {
		return false, fmt.Errorf("end voice task %s: %q is not replaced or cancelled", id, state)
	}
	result, err := s.db.Exec(`UPDATE voice_tasks SET state = ? WHERE id = ? AND state = ?`, state, id, VoiceTaskWorking)
	if err != nil {
		return false, fmt.Errorf("end voice task %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("end voice task %s: %w", id, err)
	}
	return changed == 1, nil
}

// MarkVoiceTaskSpoken records when the task's answer was spoken.
func (s *SQLiteStore) MarkVoiceTaskSpoken(id string, at time.Time) error {
	result, err := s.db.Exec(`UPDATE voice_tasks SET spoken_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("mark voice task %s spoken: %w", id, err)
	}
	return oneRow(result, "mark voice task "+id+" spoken")
}

func oneRow(result sql.Result, what string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if changed == 0 {
		return fmt.Errorf("%s: %w", what, sql.ErrNoRows)
	}
	return nil
}

func scanVoiceTask(row interface{ Scan(...any) error }) (VoiceTask, error) {
	var task VoiceTask
	var askedAt, state string
	var spokenAt sql.NullString
	if err := row.Scan(&task.ID, &task.SessionID, &askedAt, &task.IssueKey, &task.Holder, &state, &task.AnswerWritten, &spokenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VoiceTask{}, err
		}
		return VoiceTask{}, fmt.Errorf("read voice task: %w", err)
	}
	task.State = VoiceTaskState(state)
	var err error
	if task.AskedAt, err = time.Parse(time.RFC3339Nano, askedAt); err != nil {
		return VoiceTask{}, fmt.Errorf("read voice task %s asked_at: %w", task.ID, err)
	}
	if spokenAt.Valid {
		at, err := time.Parse(time.RFC3339Nano, spokenAt.String)
		if err != nil {
			return VoiceTask{}, fmt.Errorf("read voice task %s spoken_at: %w", task.ID, err)
		}
		task.SpokenAt = &at
	}
	return task, nil
}
