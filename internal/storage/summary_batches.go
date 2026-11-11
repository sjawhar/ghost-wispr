package storage

import (
	"fmt"
)

// RecordSummaryBatch persists the transcript generation each batch request read.
func (s *SQLiteStore) RecordSummaryBatch(batchName string, generations map[string]int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin record summary batch %s: %w", batchName, err)
	}
	defer func() { _ = tx.Rollback() }()
	for sessionID, generation := range generations {
		if _, err := tx.Exec(`INSERT INTO summary_batches(batch_name, session_id, generation) VALUES(?, ?, ?)`, batchName, sessionID, generation); err != nil {
			return fmt.Errorf("record summary batch %s session %s: %w", batchName, sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit summary batch %s: %w", batchName, err)
	}
	return nil
}

// SummaryBatchGenerations returns the persisted generation for every request
// in batchName, including after an application restart.
func (s *SQLiteStore) SummaryBatchGenerations(batchName string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT session_id, generation FROM summary_batches WHERE batch_name = ?`, batchName)
	if err != nil {
		return nil, fmt.Errorf("query summary batch %s: %w", batchName, err)
	}
	defer func() { _ = rows.Close() }()
	generations := map[string]int{}
	for rows.Next() {
		var sessionID string
		var generation int
		if err := rows.Scan(&sessionID, &generation); err != nil {
			return nil, fmt.Errorf("scan summary batch %s: %w", batchName, err)
		}
		generations[sessionID] = generation
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate summary batch %s: %w", batchName, err)
	}
	return generations, nil
}

// MarkSummaryBatchSessionDone records one consumed response.
func (s *SQLiteStore) MarkSummaryBatchSessionDone(batchName, sessionID, state string) error {
	_, err := s.db.Exec(`UPDATE summary_batches SET state = ? WHERE batch_name = ? AND session_id = ?`, state, batchName, sessionID)
	if err != nil {
		return fmt.Errorf("mark summary batch %s session %s: %w", batchName, sessionID, err)
	}
	return nil
}
