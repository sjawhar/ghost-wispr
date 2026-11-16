package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// sessionIDPattern is the form of a session id: the app makes them from a
// start time ("20261116140449", "20261116140449-merged-2"), and the API
// accepts no other.
var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidSessionID reports whether id has the form of a session id.
func ValidSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// legacySessionStatuses are the component_status messages the session
// manager wrote about one session before its events named the session in
// session_id: the component, and the message up to the session's id, which
// ends it. A build before this one stored them with the session only in the
// message.
var legacySessionStatuses = []struct{ component, prefix string }{
	{"refinement", "Batch refinement completed for session "},
	{"refinement", "Batch refinement failed for session "},
	{"summary", "Summarization failed for session "},
	{"summary", "Failed to store summary for session "},
	{"summary", "Failed to retrieve transcript for session "},
	{"sync", "Google Drive synced for session "},
	{"sync", "Google Drive sync completed for session "},
	{"sync", "Google Drive sync failed for session "},
}

// legacyStatusSession returns the session that message, the message of a
// component_status event of component that an older build stored, names: the
// id that ends it in one of legacySessionStatuses' shapes, when all that
// follows the shape is one session id (ValidSessionID). It returns "" for any
// other message, so no event is read as naming a session whose id is only a
// part of its message.
func legacyStatusSession(component, message string) string {
	for _, shape := range legacySessionStatuses {
		if shape.component != component {
			continue
		}
		if id, ok := strings.CutPrefix(message, shape.prefix); ok && ValidSessionID(id) {
			return id
		}
	}
	return ""
}

// dropLegacyStatusEventsOfDeletedSessions deletes, in tx, every
// component_status event an older build stored about a session the database
// does not have, with no session_id and the session only in its message
// (legacyStatusSession), and returns how many it deleted.
func dropLegacyStatusEventsOfDeletedSessions(tx *sql.Tx) (int64, error) {
	// Every shape has "for session "; the match on it only narrows the rows
	// read, and legacyStatusSession decides.
	rows, err := tx.Query(`SELECT id, payload FROM mcp_events
		WHERE event_type = 'component_status' AND payload LIKE '%for session %'`)
	if err != nil {
		return 0, fmt.Errorf("read the stored status events: %w", err)
	}
	type named struct {
		eventID int64
		session string
	}
	var events []named
	for rows.Next() {
		var eventID int64
		var payload string
		if err := rows.Scan(&eventID, &payload); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan a stored status event: %w", err)
		}
		var event struct {
			SessionID string `json:"session_id"`
			Component string `json:"component"`
			Message   string `json:"message"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil || event.SessionID != "" {
			continue
		}
		if session := legacyStatusSession(event.Component, event.Message); session != "" {
			events = append(events, named{eventID, session})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("read the stored status events: %w", err)
	}
	var removed int64
	for _, event := range events {
		res, err := tx.Exec(`DELETE FROM mcp_events
			WHERE id = ? AND NOT EXISTS (SELECT 1 FROM sessions WHERE id = ?)`, event.eventID, event.session)
		if err != nil {
			return 0, fmt.Errorf("delete a stored status event of deleted session %s: %w", event.session, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count the stored status events of deleted session %s: %w", event.session, err)
		}
		removed += n
	}
	return removed, nil
}
