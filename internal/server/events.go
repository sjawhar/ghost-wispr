package server

import "time"

const EventVersion = 1

type Event struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	Timestamp string `json:"timestamp"`
}

// eventType returns the event type string for use in event storage filtering.
func (e Event) eventType() string { return e.Type }

type LiveTranscriptEvent struct {
	Event
	SessionID string  `json:"session_id"`
	Speaker   int     `json:"speaker"`
	Text      string  `json:"text"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
}

type LiveTranscriptInterimEvent struct {
	Event
	SessionID string  `json:"session_id,omitempty"`
	Speaker   int     `json:"speaker"`
	Text      string  `json:"text"`
	StartTime float64 `json:"start_time"`
}

type SessionStartedEvent struct {
	Event
	SessionID string `json:"session_id"`
}

type SessionEndedEvent struct {
	Event
	SessionID string  `json:"session_id"`
	Duration  float64 `json:"duration"`
}

type SummaryReadyEvent struct {
	Event
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	Preset    string `json:"summary_preset"`
	// Error says why making the summary failed, on a failed status.
	Error string `json:"summary_error,omitempty"`
}

type StatusChangedEvent struct {
	Event
	Paused bool `json:"paused"`
}

type ConnectionEvent struct {
	Event
	Connected bool `json:"connected"`
}

// ComponentStatusEvent is a component's status. A status about one session
// (its refinement, summary or sync) names it in SessionID; a status of the
// whole unit (the microphone, Deepgram) has none.
type ComponentStatusEvent struct {
	Event
	SessionID string `json:"session_id,omitempty"`
	Component string `json:"component"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

// SessionDeletedEvent tells a web client that a session was deleted, with
// the sessions merged into it, so it drops them: their ids only. It is never
// stored.
type SessionDeletedEvent struct {
	Event
	SessionID        string   `json:"session_id"`
	SourceSessionIDs []string `json:"source_session_ids"`
}

func newEvent(eventType string, now time.Time) Event {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Event{
		Type:      eventType,
		Version:   EventVersion,
		Timestamp: now.UTC().Format(time.RFC3339Nano),
	}
}
