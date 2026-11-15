package main

import (
	"fmt"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const rawEraseReservationWarningKeyPrefix = "raw-erase-reservation-"

type rawEraseReservationStore interface {
	RawEraseReservations() ([]storage.RawEraseReservation, error)
}

// refreshRawEraseReservationWarnings reports each raw capture that startup
// could not restore. Its reservation keeps automatic erase passes away until a
// later DELETE succeeds or startup restores the capture.
func refreshRawEraseReservationWarnings(store rawEraseReservationStore, registry *warningsRegistry) error {
	reservations, err := store.RawEraseReservations()
	if err != nil {
		return fmt.Errorf("list reserved raw erases: %w", err)
	}
	warnings := make(map[string]string, len(reservations))
	for _, reservation := range reservations {
		warnings[rawEraseReservationWarningKeyPrefix+reservation.SessionID+"-"+reservation.HiddenPath] = fmt.Sprintf(
			"The raw recording for session %s could not be restored at startup. It is kept hidden until the session is deleted again.",
			reservation.SessionID,
		)
	}
	registry.ReplacePrefix(rawEraseReservationWarningKeyPrefix, warnings)
	return nil
}
