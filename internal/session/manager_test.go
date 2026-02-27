package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type storeMock struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	segments map[string][]transcribe.Segment
	summary  map[string]string
	status   map[string]string
	audio    map[string]string
}

func newStoreMock() *storeMock {
	return &storeMock{
		sessions: map[string]time.Time{},
		segments: map[string][]transcribe.Segment{},
		summary:  map[string]string{},
		status:   map[string]string{},
		audio:    map[string]string{},
	}
}

func (s *storeMock) CreateSession(id string, startedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = startedAt
	s.status[id] = "active"
	return nil
}

func (s *storeMock) EndSession(id string, _ time.Time, audioPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = "ended"
	s.audio[id] = audioPath
	return nil
}

func (s *storeMock) AppendSegment(sessionID string, seg transcribe.Segment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segments[sessionID] = append(s.segments[sessionID], seg)
	return nil
}

func (s *storeMock) GetSegments(sessionID string) ([]transcribe.Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]transcribe.Segment(nil), s.segments[sessionID]...)
	return list, nil
}

func (s *storeMock) UpdateSummary(sessionID, summary, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary[sessionID] = summary
	s.status[sessionID] = status
	return nil
}

type recorderMock struct {
	mu      sync.Mutex
	started []string
	ended   int
}

func (r *recorderMock) StartSession(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, id)
	return nil
}

func (r *recorderMock) EndSession() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended++
	if len(r.started) == 0 {
		return "", nil
	}
	return "data/audio/" + r.started[len(r.started)-1] + ".mp3", nil
}

type summarizerMock struct {
	called chan string
}

func (s summarizerMock) Summarize(_ context.Context, sessionID, transcript string) (string, error) {
	if s.called != nil {
		s.called <- sessionID
	}
	return "## Summary\n- " + transcript, nil
}

type hubMock struct {
	mu            sync.Mutex
	liveCount     int
	startedCount  int
	endedCount    int
	summaryReady  int
	latestSession string
	latestSummary string
	latestStatus  string
}

func (h *hubMock) BroadcastLiveTranscript(_ transcribe.Segment) {
	h.mu.Lock()
	h.liveCount++
	h.mu.Unlock()
}

func (h *hubMock) BroadcastSessionStarted(sessionID string) {
	h.mu.Lock()
	h.startedCount++
	h.latestSession = sessionID
	h.mu.Unlock()
}

func (h *hubMock) BroadcastSessionEnded(sessionID string, _ time.Duration) {
	h.mu.Lock()
	h.endedCount++
	h.latestSession = sessionID
	h.mu.Unlock()
}

func (h *hubMock) BroadcastSummaryReady(sessionID, summary, status string) {
	h.mu.Lock()
	h.summaryReady++
	h.latestSession = sessionID
	h.latestSummary = summary
	h.latestStatus = status
	h.mu.Unlock()
}

func TestManagerLifecycle(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	hub := &hubMock{}
	summaryCalled := make(chan string, 1)
	summarizer := summarizerMock{called: summaryCalled}

	detector := NewDetector(20 * time.Millisecond)
	manager := NewManager(store, recorder, summarizer, hub, detector)

	var msg api.MessageResponse
	raw := []byte(`{
		"is_final": true,
		"channel": {
			"alternatives": [
				{
					"transcript": "hello world this is a full sentence",
					"words": [
						{"speaker": 0, "punctuated_word": "hello", "start": 0, "end": 0.5},
						{"speaker": 0, "punctuated_word": "world", "start": 0.5, "end": 1.0}
					]
				}
			]
		}
	}`)
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal deepgram message failed: %v", err)
	}

	if err := manager.Message(&msg); err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	hub.mu.Lock()
	if hub.startedCount != 1 {
		t.Fatalf("expected session_started broadcast count 1, got %d", hub.startedCount)
	}
	if hub.liveCount == 0 {
		t.Fatalf("expected live transcript broadcast")
	}
	sessionID := hub.latestSession
	hub.mu.Unlock()

	if sessionID == "" {
		t.Fatal("expected session id")
	}

	if len(store.segments[sessionID]) == 0 {
		t.Fatal("expected persisted segments")
	}

	if err := manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatalf("UtteranceEnd failed: %v", err)
	}

	select {
	case <-summaryCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected summary generation to be triggered")
	}

	time.Sleep(30 * time.Millisecond)

	hub.mu.Lock()
	if hub.endedCount != 1 {
		t.Fatalf("expected session_ended broadcast count 1, got %d", hub.endedCount)
	}
	if hub.summaryReady != 1 {
		t.Fatalf("expected summary_ready broadcast count 1, got %d", hub.summaryReady)
	}
	hub.mu.Unlock()

	if recorder.ended == 0 {
		t.Fatal("expected recorder EndSession to be called")
	}
}
