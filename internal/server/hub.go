package server

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Hub struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[chan []byte]struct{})}
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

func (h *Hub) BroadcastSummaryReady(sessionID, summary, status, preset string) {
	h.broadcastEvent(SummaryReadyEvent{
		Event:     newEvent("summary_ready", time.Now().UTC()),
		SessionID: sessionID,
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

func (h *Hub) broadcastEvent(event any) {
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("event marshal error: %v", err)
		return
	}
	h.Broadcast(payload)
}
