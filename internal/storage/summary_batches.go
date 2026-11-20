package storage

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrUnknownSummaryBatch is a summary batch job this store has no record
// of: this unit never submitted it.
var ErrUnknownSummaryBatch = errors.New("this unit submitted no summary batch job of that name")

// SummaryClaim is a summary batch job's claim of a session's summary: the
// transcript generation its request read, and the summary attempt it
// started (StartBatchSummary). The job's result or failure applies only
// while both are still the session's (UpdateSummaryAtClaim,
// FailSummaryAtClaim).
type SummaryClaim struct {
	Generation int
	Attempt    int
}

// SummaryBatchSession is a batch job's claim of a session, and whether a
// terminal poll of the job has handled it.
type SummaryBatchSession struct {
	SummaryClaim
	State string
}

// RecordSummaryBatch persists each claim of batch job batchName, and
// provider, the provider that took the job, as the config names it: the job
// is polled there whatever summarization.model names later.
func (s *SQLiteStore) RecordSummaryBatch(batchName, provider string, claims map[string]SummaryClaim) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin record summary batch %s: %w", batchName, err)
	}
	defer func() { _ = tx.Rollback() }()
	for sessionID, claim := range claims {
		if _, err := tx.Exec(`INSERT INTO summary_batches(batch_name, session_id, generation, attempt, provider) VALUES(?, ?, ?, ?, ?)`, batchName, sessionID, claim.Generation, claim.Attempt, provider); err != nil {
			return fmt.Errorf("record summary batch %s session %s: %w", batchName, sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit summary batch %s: %w", batchName, err)
	}
	return nil
}

// SummaryBatchProvider returns the provider that took batch job batchName
// (RecordSummaryBatch): "" for a job recorded before the provider was kept,
// and ErrUnknownSummaryBatch for one the store has no record of.
func (s *SQLiteStore) SummaryBatchProvider(batchName string) (string, error) {
	var provider string
	err := s.db.QueryRow(`SELECT provider FROM summary_batches WHERE batch_name = ? LIMIT 1`, batchName).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrUnknownSummaryBatch, batchName)
	}
	if err != nil {
		return "", fmt.Errorf("read summary batch %s provider: %w", batchName, err)
	}
	return provider, nil
}

// SummaryBatchSessions returns every claim of batch job batchName and its
// processing state, including after an application restart.
func (s *SQLiteStore) SummaryBatchSessions(batchName string) (map[string]SummaryBatchSession, error) {
	rows, err := s.db.Query(`SELECT session_id, generation, attempt, state FROM summary_batches WHERE batch_name = ?`, batchName)
	if err != nil {
		return nil, fmt.Errorf("query summary batch %s sessions: %w", batchName, err)
	}
	defer func() { _ = rows.Close() }()

	sessions := map[string]SummaryBatchSession{}
	for rows.Next() {
		var sessionID string
		var session SummaryBatchSession
		if err := rows.Scan(&sessionID, &session.Generation, &session.Attempt, &session.State); err != nil {
			return nil, fmt.Errorf("scan summary batch %s session: %w", batchName, err)
		}
		sessions[sessionID] = session
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate summary batch %s sessions: %w", batchName, err)
	}
	return sessions, nil
}

// MarkSummaryBatchSessionDone records one consumed response.
func (s *SQLiteStore) MarkSummaryBatchSessionDone(batchName, sessionID, state string) error {
	_, err := s.db.Exec(`UPDATE summary_batches SET state = ? WHERE batch_name = ? AND session_id = ?`, state, batchName, sessionID)
	if err != nil {
		return fmt.Errorf("mark summary batch %s session %s: %w", batchName, sessionID, err)
	}
	return nil
}
