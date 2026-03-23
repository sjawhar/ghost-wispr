package server

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Hub struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
	logger  *slog.Logger
}

func NewHub(logger ...*slog.Logger) *Hub {
	l := logging.WithModule(slog.Default(), "server")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "server")
	}

	return &Hub{clients: make(map[chan []byte]struct{}), logger: l}
}

func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *Hub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *Hub) BroadcastLiveTranscript(seg transcribe.Segment) {
	h.broadcastEvent(LiveTranscriptEvent{
		Event:     newEvent("live_transcript", seg.Timestamp),
		Speaker:   seg.Speaker,
		Text:      seg.Text,
		StartTime: seg.StartTime,
		EndTime:   seg.EndTime,
	})
}

func (h *Hub) BroadcastLiveTranscriptInterim(speaker int, text string, startTime float64) {
	h.broadcastEvent(LiveTranscriptInterimEvent{
		Event:     newEvent("live_transcript_interim", time.Now().UTC()),
		Speaker:   speaker,
		Text:      text,
		StartTime: startTime,
	})
}

func (h *Hub) BroadcastSessionStarted(sessionID string) {
	h.broadcastEvent(SessionStartedEvent{
		Event:     newEvent("session_started", time.Now().UTC()),
		SessionID: sessionID,
	})
}

func (h *Hub) BroadcastSessionEnded(sessionID string, duration time.Duration) {
	h.broadcastEvent(SessionEndedEvent{
		Event:     newEvent("session_ended", time.Now().UTC()),
		SessionID: sessionID,
		Duration:  duration.Seconds(),
	})
}

func (h *Hub) BroadcastSummaryReady(sessionID, title, summary, status, preset string) {
	h.broadcastEvent(SummaryReadyEvent{
		Event:     newEvent("summary_ready", time.Now().UTC()),
		SessionID: sessionID,
		Title:     title,
		Summary:   summary,
		Status:    status,
		Preset:    preset,
	})
}

func (h *Hub) BroadcastStatusChanged(paused bool) {
	h.broadcastEvent(StatusChangedEvent{
		Event:  newEvent("status_changed", time.Now().UTC()),
		Paused: paused,
	})
}

func (h *Hub) BroadcastComponentStatus(component, status, message string) {
	h.broadcastEvent(ComponentStatusEvent{
		Event:     newEvent("component_status", time.Now().UTC()),
		Component: component,
		Status:    status,
		Message:   message,
	})
}

func (h *Hub) broadcastEvent(event any) {
	payload, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("failed to marshal websocket event", "operation", "marshal_event", "error", err)
		return
	}
	h.Broadcast(payload)
}
