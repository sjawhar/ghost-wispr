package server

import "time"

const EventVersion = 1

type Event struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	Timestamp string `json:"timestamp"`
}

type LiveTranscriptEvent struct {
	Event
	Speaker   int     `json:"speaker"`
	Text      string  `json:"text"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
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
	Summary   string `json:"summary"`
	Status    string `json:"status"`
}

type StatusChangedEvent struct {
	Event
	Paused bool `json:"paused"`
}

type ConnectionEvent struct {
	Event
	Connected bool `json:"connected"`
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
