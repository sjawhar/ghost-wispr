package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// EventStore persists broadcast events, and says whether a session still
// exists (HasSession), so the hub sends no status about a deleted session to
// a client that connects. StoreEvent gives up when ctx ends: a numbered
// status's store runs in the state callback of a client that may be closing.
type EventStore interface {
	StoreEvent(ctx context.Context, eventType, payload string) error
	HasSession(id string) (bool, error)
}

type Hub struct {
	mu                sync.RWMutex
	statusMu          sync.Mutex
	clients           map[chan []byte]*hubClient
	componentStatuses map[string]ComponentStatusEvent
	statusChanged     *StatusChangedEvent
	logger            *slog.Logger
	eventStore        EventStore
}

func NewHub(logger ...*slog.Logger) *Hub {
	l := logging.WithModule(slog.Default(), "server")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "server")
	}

	return &Hub{
		clients:           make(map[chan []byte]*hubClient),
		componentStatuses: make(map[string]ComponentStatusEvent),
		logger:            l,
	}
}

// SetEventStore sets an optional event store for durable event persistence.
func (h *Hub) SetEventStore(store EventStore) {
	h.eventStore = store
}

// Subscribe returns a channel of every event the hub broadcasts from now on.
// An ordinary event is dropped for a subscriber that has fallen
// subscriberQueue events behind; a component's status is not, and the
// subscriber ends on each component's latest status (hubClient).
func (h *Hub) Subscribe() chan []byte {
	c := newHubClient()
	h.mu.Lock()
	h.clients[c.ch] = c
	h.mu.Unlock()
	return c.ch
}

// SnapshotComponentStatuses returns each component's last status, for a
// client that connects. It leaves out, and forgets, a status about a session
// the event store no longer has: one announced before the session was
// deleted, or after (a refinement that ends after the delete). When the store
// cannot say, it leaves the status out and keeps it.
func (h *Hub) SnapshotComponentStatuses() []ComponentStatusEvent {
	h.mu.RLock()
	keys := make([]string, 0, len(h.componentStatuses))
	for key := range h.componentStatuses {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	snapshot := make([]ComponentStatusEvent, 0, len(keys))
	for _, key := range keys {
		snapshot = append(snapshot, h.componentStatuses[key])
	}
	h.mu.RUnlock()
	if h.eventStore == nil {
		return snapshot
	}

	kept := snapshot[:0]
	for _, event := range snapshot {
		if event.SessionID == "" {
			kept = append(kept, event)
			continue
		}
		exists, err := h.eventStore.HasSession(event.SessionID)
		switch {
		case err != nil:
			h.logger.Warn("could not check whether a status's session exists; the status is not sent", "operation", "snapshot_component_statuses", "session_id", event.SessionID, "component", event.Component, "error", err)
		case exists:
			kept = append(kept, event)
		default:
			h.mu.Lock()
			if h.componentStatuses[event.Component] == event {
				delete(h.componentStatuses, event.Component)
			}
			h.mu.Unlock()
		}
	}
	return kept
}

func (h *Hub) SnapshotStatusChanged() (StatusChangedEvent, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.statusChanged == nil {
		return StatusChangedEvent{}, false
	}
	return *h.statusChanged, true
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	c := h.clients[ch]
	delete(h.clients, ch)
	h.mu.Unlock()
	if c != nil {
		c.stop()
	}
	close(ch)
}

func (h *Hub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, c := range h.clients {
		c.send(msg)
	}
}

//nolint:gocritic // Hub's event API passes one immutable segment by value.
func (h *Hub) BroadcastLiveTranscript(sessionID string, seg transcribe.Segment) {
	h.broadcastEvent(LiveTranscriptEvent{
		Event:     newEvent("live_transcript", seg.Timestamp),
		SessionID: sessionID,
		Speaker:   seg.Speaker,
		Text:      seg.Text,
		StartTime: seg.StartTime,
		EndTime:   seg.EndTime,
	})
}

func (h *Hub) BroadcastLiveTranscriptInterim(sessionID string, speaker int, text string, startTime float64) {
	h.broadcastEvent(LiveTranscriptInterimEvent{
		Event:     newEvent("live_transcript_interim", time.Now().UTC()),
		SessionID: sessionID,
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

// BroadcastSummaryFailed announces that making session sessionID's summary
// failed, and why (reason), with the title, summary and preset it keeps.
func (h *Hub) BroadcastSummaryFailed(sessionID, title, summary, preset, reason string) {
	h.broadcastEvent(SummaryReadyEvent{
		Event:     newEvent("summary_ready", time.Now().UTC()),
		SessionID: sessionID,
		Title:     title,
		Summary:   summary,
		Status:    storage.SummaryFailed,
		Preset:    preset,
		Error:     reason,
	})
}

func (h *Hub) BroadcastStatusChanged(paused bool) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	event := StatusChangedEvent{
		Event:  newEvent("status_changed", time.Now().UTC()),
		Paused: paused,
	}
	h.mu.Lock()
	h.statusChanged = &event
	h.mu.Unlock()
	h.broadcastEvent(event)
}

// BroadcastComponentStatus announces the status of a part of the unit (the
// microphone, Deepgram): a status about no session.
func (h *Hub) BroadcastComponentStatus(component, status, message string) {
	h.broadcastComponentStatus(context.Background(), "", component, status, message, 0)
}

// BroadcastComponentStatusSeq announces the status of a part of the unit
// whose changes are numbered (seq, from 1): the event carries seq, and a
// status older than the one the hub last announced for component is dropped,
// so a status heard of late never replaces a newer one. Every subscriber ends
// on the component's latest status, gets its statuses in order, and never an
// older one after a newer one; one that falls behind may miss statuses in
// between (hubClient). Storing the event gives up when ctx ends.
func (h *Hub) BroadcastComponentStatusSeq(ctx context.Context, component, status, message string, seq uint64) {
	h.broadcastComponentStatus(ctx, "", component, status, message, seq)
}

// BroadcastSessionComponentStatus announces a component's status about
// session sessionID (its refinement, summary or sync). The event names the
// session in session_id, so deleting the session deletes the stored event.
// Work running at a delete reports on a session that is gone; no client is
// sent that status, and a client that connects in the moment between this
// check and the delete is not sent it either (SnapshotComponentStatuses).
func (h *Hub) BroadcastSessionComponentStatus(sessionID, component, status, message string) {
	if h.eventStore != nil {
		exists, err := h.eventStore.HasSession(sessionID)
		if err != nil {
			h.logger.Warn("could not check whether a status's session exists; the status is not sent", "operation", "broadcast_component_status", "session_id", sessionID, "component", component, "error", err)
			return
		}
		if !exists {
			return
		}
	}
	h.broadcastComponentStatus(context.Background(), sessionID, component, status, message, 0)
}

// broadcastComponentStatus records the status as the component's last, and
// offers it to every subscriber as the component's latest (hubClient), both
// under one lock, so the subscribers get a component's statuses in the order
// the hub records them. A numbered status (seq > 0) older than the recorded
// one is dropped.
func (h *Hub) broadcastComponentStatus(ctx context.Context, sessionID, component, status, message string, seq uint64) {
	event := ComponentStatusEvent{
		Event:     newEvent("component_status", time.Now().UTC()),
		SessionID: sessionID,
		Component: component,
		Status:    status,
		Message:   message,
		Seq:       seq,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("failed to marshal websocket event", "operation", "marshal_event", "error", err)
		return
	}

	h.mu.Lock()
	if last, ok := h.componentStatuses[component]; ok && seq > 0 && last.Seq > seq {
		h.mu.Unlock()
		return
	}
	h.componentStatuses[component] = event
	for _, c := range h.clients {
		c.sendLatest(component, payload)
	}
	h.mu.Unlock()

	h.storeEvent(ctx, event, payload)
}

// BroadcastSessionDeleted tells every connected client that session
// sessionID was deleted, with sourceIDs, the sessions merged into it, so each
// tab drops their title, summary, segments and statuses. The event carries
// their ids only and is not stored. The hub forgets its last status of each.
func (h *Hub) BroadcastSessionDeleted(sessionID string, sourceIDs []string) {
	deleted := make(map[string]bool, len(sourceIDs)+1)
	deleted[sessionID] = true
	for _, id := range sourceIDs {
		deleted[id] = true
	}
	h.mu.Lock()
	for component, status := range h.componentStatuses {
		if deleted[status.SessionID] {
			delete(h.componentStatuses, component)
		}
	}
	h.mu.Unlock()
	h.broadcastEvent(SessionDeletedEvent{
		Event:            newEvent("session_deleted", time.Now().UTC()),
		SessionID:        sessionID,
		SourceSessionIDs: append([]string{}, sourceIDs...),
	})
}

// voiceEventTypes are the voice's /ws events. Each carries a session id and
// never words, and none is stored: they go to open pages only.
var voiceEventTypes = map[string]bool{
	"voice_wake":             true,
	"voice_exchange_started": true,
	"voice_answer_ready":     true,
	"voice_spoken":           true,
	"voice_tasks_changed":    true, // the session's questions to agents changed
	"agent_joined":           true,
	"agent_left":             true,
}

// BroadcastVoiceEvent tells open pages about the voice: kind is one of
// voiceEventTypes, about recording session sessionID ("" before one started).
// Any other kind is dropped.
func (h *Hub) BroadcastVoiceEvent(kind, sessionID string) {
	if !voiceEventTypes[kind] {
		h.logger.Warn("dropped an unknown voice event", "operation", "broadcast_voice_event", "event_type", kind)
		return
	}
	// now keeps its monotonic reading for monoclock; UTC would strip it.
	now := time.Now()
	mono, _ := monoclock.Nanos(now)
	h.broadcastEvent(VoiceEvent{Event: newEvent(kind, now.UTC()), SessionID: sessionID, AtMonoNs: mono})
}

// storedEventTypes is the set of event types persisted to the event queue.
var storedEventTypes = map[string]struct{}{
	"session_started":  {},
	"session_ended":    {},
	"summary_ready":    {},
	"status_changed":   {},
	"component_status": {},
	"live_transcript":  {},
}

func (h *Hub) broadcastEvent(event any) {
	payload, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("failed to marshal websocket event", "operation", "marshal_event", "error", err)
		return
	}
	h.Broadcast(payload)
	h.storeEvent(context.Background(), event, payload)
}

// storeEvent persists an event of a stored type, after it was broadcast:
// best effort, never holding the broadcast, and given up when ctx ends.
func (h *Hub) storeEvent(ctx context.Context, event any, payload []byte) {
	if h.eventStore == nil {
		return
	}
	if evt, ok := event.(interface{ eventType() string }); ok {
		if _, store := storedEventTypes[evt.eventType()]; store {
			if err := h.eventStore.StoreEvent(ctx, evt.eventType(), string(payload)); err != nil {
				h.logger.Warn("failed to store event", "operation", "store_event", "event_type", evt.eventType(), "error", err)
			}
		}
	}
}
