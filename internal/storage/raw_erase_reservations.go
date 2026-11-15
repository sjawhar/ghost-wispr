package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/erase"
)

// RawEraseReservation is a raw microphone capture hidden before a session
// delete commits. It stays present when startup cannot restore the capture.
type RawEraseReservation struct {
	SessionID    string
	OriginalPath string
	HiddenPath   string
}

// ReserveRawSessionErase records the raw microphone capture's two names
// before the API moves it to the hidden one. The record commits before the
// rename, so startup can restore a session whose delete did not commit. A
// retry reuses the earlier hidden name instead of stranding it.
func (s *SQLiteStore) ReserveRawSessionErase(id, originalPath, hiddenPath string) (string, error) {
	originalPath, err := filepath.Abs(originalPath)
	if err != nil {
		return "", fmt.Errorf("find raw erase path for session %s: %w", id, err)
	}
	hiddenPath, err = filepath.Abs(hiddenPath)
	if err != nil {
		return "", fmt.Errorf("find hidden raw erase path for session %s: %w", id, err)
	}
	if filepath.Dir(originalPath) != filepath.Dir(hiddenPath) {
		return "", fmt.Errorf("reserve raw erase for session %s: paths are in different directories", id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin reserving raw erase for session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.ErrNoRows
		}
		return "", fmt.Errorf("read session %s for raw erase reservation: %w", id, err)
	}
	if err := recordEraseDirectory(tx, filepath.Dir(originalPath)); err != nil {
		return "", fmt.Errorf("reserve raw erase for session %s: %w", id, err)
	}
	var persistedHiddenPath string
	if err := tx.QueryRow(`INSERT INTO raw_erase_reservations(session_id, original_path, hidden_path)
		VALUES(?, ?, ?)
		ON CONFLICT(session_id, original_path) DO UPDATE SET session_id = excluded.session_id
		RETURNING hidden_path`,
		id, originalPath, hiddenPath).Scan(&persistedHiddenPath); err != nil {
		return "", fmt.Errorf("record raw erase reservation for session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit raw erase reservation for session %s: %w", id, err)
	}
	return persistedHiddenPath, nil
}

// RawEraseReservations lists raw captures still protected from automatic
// zeroing. Older databases have none until this version creates the table.
func (s *SQLiteStore) RawEraseReservations() ([]RawEraseReservation, error) {
	rows, err := s.db.Query(`SELECT session_id, original_path, hidden_path FROM raw_erase_reservations`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("list raw erase reservations: %w", err)
	}
	var reservations []RawEraseReservation
	for rows.Next() {
		var reservation RawEraseReservation
		if err := rows.Scan(&reservation.SessionID, &reservation.OriginalPath, &reservation.HiddenPath); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan raw erase reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list raw erase reservations: %w", err)
	}
	return reservations, nil
}

// ReservedRawErasePaths returns hidden raw paths that no automatic zeroing
// pass may touch while their reservation remains.
func (s *SQLiteStore) ReservedRawErasePaths() (map[string]bool, error) {
	reservations, err := s.RawEraseReservations()
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool, len(reservations))
	for _, reservation := range reservations {
		paths[reservation.HiddenPath] = true
	}
	return paths, nil
}

// CancelRawSessionErase forgets a reservation whose delete did not continue.
// A restore that fails deliberately keeps its reservation for startup to
// recover rather than allowing Leftovers to zero a live session's raw file.
func (s *SQLiteStore) CancelRawSessionErase(id, hiddenPath string) error {
	hiddenPath, err := filepath.Abs(hiddenPath)
	if err != nil {
		return fmt.Errorf("find hidden raw erase path for session %s: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM raw_erase_reservations WHERE session_id = ? AND hidden_path = ?`, id, hiddenPath); err != nil {
		return fmt.Errorf("cancel raw erase reservation for session %s: %w", id, err)
	}
	return nil
}

// restoreRawEraseReservations makes a raw recording visible again when its
// session survived the delete that hid it. It must run before any
// erase.Leftovers pass. A failed restore stays reserved, so the app starts
// and every later zeroing pass skips that file until a delete retry resolves
// it.
func (s *SQLiteStore) restoreRawEraseReservations() error {
	reservations, err := s.RawEraseReservations()
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		var exists int
		err := s.db.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, reservation.SessionID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read session %s for raw erase recovery: %w", reservation.SessionID, err)
		}
		hidden, err := os.Lstat(reservation.HiddenPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			slog.Warn("could not read a hidden raw recording during startup recovery", "session_id", reservation.SessionID, "hidden_path", reservation.HiddenPath, "error", err)
			continue
		case !hidden.Mode().IsRegular():
			slog.Warn("hidden raw recording is not a regular file during startup recovery", "session_id", reservation.SessionID, "hidden_path", reservation.HiddenPath)
			continue
		default:
			_, originalErr := os.Lstat(reservation.OriginalPath)
			switch {
			case errors.Is(originalErr, os.ErrNotExist):
				if err := erase.Restore(reservation.OriginalPath, reservation.HiddenPath); err != nil {
					slog.Warn("could not restore a raw recording during startup recovery", "session_id", reservation.SessionID, "original_path", reservation.OriginalPath, "hidden_path", reservation.HiddenPath, "error", err)
					continue
				}
			case originalErr != nil:
				slog.Warn("could not read a raw recording destination during startup recovery", "session_id", reservation.SessionID, "original_path", reservation.OriginalPath, "error", originalErr)
				continue
			}
		}
		if err := s.CancelRawSessionErase(reservation.SessionID, reservation.HiddenPath); err != nil {
			slog.Warn("could not clear a recovered raw erase reservation", "session_id", reservation.SessionID, "hidden_path", reservation.HiddenPath, "error", err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM raw_erase_reservations
		WHERE NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.id = raw_erase_reservations.session_id)`); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return fmt.Errorf("discard deleted raw erase reservations: %w", err)
	}
	return nil
}
