package main

import (
	"fmt"
	"log"
	"path/filepath"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const rawEraseReservationWarningKeyPrefix = "raw-erase-reservation-"

type rawEraseReservationStore interface {
	RawEraseReservations() ([]storage.RawEraseReservation, error)
	HasSession(id string) (bool, error)
}

// refreshRawEraseReservationWarnings reports each raw capture that startup
// could not restore. Its reservation keeps automatic erase passes away until a
// later DELETE succeeds or startup restores the capture. The reservation of a
// deleted session (an erase GC could not finish) stays until the next start
// erases it; its warning names the hidden file, not the session, which is
// gone. When the store cannot say whether the session exists, the warning
// names neither.
func refreshRawEraseReservationWarnings(store rawEraseReservationStore, registry *warningsRegistry) error {
	reservations, err := store.RawEraseReservations()
	if err != nil {
		return fmt.Errorf("list reserved raw erases: %w", err)
	}
	warnings := make(map[string]string, len(reservations))
	for _, reservation := range reservations {
		key := rawEraseReservationWarningKeyPrefix + reservation.SessionID + "-" + reservation.HiddenPath
		exists, err := store.HasSession(reservation.SessionID)
		if err != nil {
			log.Printf("warning: could not check whether session %s of a raw erase reservation exists: %v", reservation.SessionID, err)
		}
		if err == nil && exists {
			warnings[key] = fmt.Sprintf(
				"The raw recording for session %s could not be restored at startup. It is kept hidden until the session is deleted again.",
				reservation.SessionID,
			)
			continue
		}
		warnings[key] = fmt.Sprintf(
			"A raw recording is kept hidden as %s, and is erased when the app next starts.",
			filepath.Base(reservation.HiddenPath),
		)
	}
	registry.ReplacePrefix(rawEraseReservationWarningKeyPrefix, warnings)
	return nil
}
