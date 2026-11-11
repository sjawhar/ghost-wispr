package storage

import (
	"database/sql"
	"errors"
	"fmt"
)

const AppStateCapturePaused = "capture_paused"

// GetAppState returns the value stored for key. An unset key returns ok=false.
func (s *SQLiteStore) GetAppState(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM app_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get app state %q: %w", key, err)
	}
	return value, true, nil
}

// SetAppState creates or replaces the value stored for key.
func (s *SQLiteStore) SetAppState(key, value string) error {
	if _, err := s.db.Exec(`
		INSERT INTO app_state(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value); err != nil {
		return fmt.Errorf("set app state %q: %w", key, err)
	}
	return nil
}
