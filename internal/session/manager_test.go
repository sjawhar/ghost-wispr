package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type storeMock struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	segments map[string][]transcribe.Segment
	summary  map[string]string
	status   map[string]string
	preset   map[string]string
	audio    map[string]string

	endSessionErr   error
	endSessionCalls int
}

func newStoreMock() *storeMock {
	return &storeMock{
		sessions: map[string]time.Time{},
		segments: map[string][]transcribe.Segment{},
		summary:  map[string]string{},
		status:   map[string]string{},
		preset:   map[string]string{},
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
	s.endSessionCalls++
	if s.endSessionErr != nil {
		return s.endSessionErr
	}
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

func (s *storeMock) UpdateSummary(sessionID, summary, status, preset string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary[sessionID] = summary
	s.status[sessionID] = status
	s.preset[sessionID] = preset
	return nil
}

type recorderMock struct {
	mu      sync.Mutex
	started []string
	ended   int

	startErr error
}

func (r *recorderMock) StartSession(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return r.startErr
	}
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

func (s summarizerMock) Summarize(_ context.Context, sessionID, transcript string) (string, string, error) {
	if s.called != nil {
		s.called <- sessionID
	}
	return "## Summary\n- " + transcript, "default", nil
}

type contextProbeSummarizer struct {
	delay  time.Duration
	stateC chan error
}

func (s contextProbeSummarizer) Summarize(ctx context.Context, _ string, transcript string) (string, string, error) {
	time.Sleep(s.delay)
	select {
	case <-ctx.Done():
		if s.stateC != nil {
			s.stateC <- ctx.Err()
		}
		return "", "default", ctx.Err()
	default:
		if s.stateC != nil {
			s.stateC <- nil
		}
		return "## Summary\n- " + transcript, "default", nil
	}
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
	latestPreset  string
	interimCount  int
}

func (h *hubMock) BroadcastLiveTranscript(_ transcribe.Segment) {
	h.mu.Lock()
	h.liveCount++
	h.mu.Unlock()
}

func (h *hubMock) BroadcastLiveTranscriptInterim(_ int, _ string, _ float64) {
	h.mu.Lock()
	h.interimCount++
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

func (h *hubMock) BroadcastSummaryReady(sessionID, summary, status, preset string) {
	h.mu.Lock()
	h.summaryReady++
	h.latestSession = sessionID
	h.latestSummary = summary
	h.latestStatus = status
	h.latestPreset = preset
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
		"speech_final": true,
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

func TestManager_AutoSummaryContextNotCanceled(t *testing.T) {
	store := newStoreMock()
	stateC := make(chan error, 1)
	summarizer := contextProbeSummarizer{delay: 20 * time.Millisecond, stateC: stateC}
	manager := NewManager(store, nil, summarizer, nil, NewDetector(time.Hour))

	now := time.Now().UTC()
	if err := manager.ensureSessionStarted(now); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "hello"}); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	if err := manager.endCurrentSession(ctx); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}
	cancel()

	select {
	case err := <-stateC:
		if err != nil {
			t.Fatalf("expected summary context to remain active after endCurrentSession returns, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for summary call")
	}
}

func TestManager_ForceEndSession_SummaryCompletes(t *testing.T) {
	store := newStoreMock()
	stateC := make(chan error, 1)
	summarizer := contextProbeSummarizer{delay: 20 * time.Millisecond, stateC: stateC}
	manager := NewManager(store, nil, summarizer, nil, NewDetector(time.Hour))

	now := time.Now().UTC()
	if err := manager.ensureSessionStarted(now); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "hello"}); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := manager.ForceEndSession(ctx); err != nil {
		t.Fatalf("ForceEndSession failed: %v", err)
	}

	select {
	case err := <-stateC:
		if err != nil {
			t.Fatalf("expected summary generation to continue after ForceEndSession returns, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for summary call")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		status := store.status[sessionID]
		store.mu.Unlock()
		if status == storage.SummaryCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected summary status %q, got %q", storage.SummaryCompleted, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManager_EndSession_StoreFailurePreservesState(t *testing.T) {
	store := newStoreMock()
	store.endSessionErr = errors.New("store end failed")
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour))

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}

	startedSessionID := manager.currentSession()
	if startedSessionID == "" {
		t.Fatal("expected active session")
	}

	err := manager.endCurrentSession(context.Background())
	if err == nil {
		t.Fatal("expected endCurrentSession to fail")
	}

	if got := manager.currentSession(); got == "" {
		t.Fatal("expected manager to preserve currentSessionID on end failure")
	}
}

func TestManager_StartSession_RecorderFailureRollsBack(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{startErr: errors.New("recorder start failed")}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour))

	err := manager.ensureSessionStarted(time.Now().UTC())
	if err == nil {
		t.Fatal("expected ensureSessionStarted to fail")
	}

	if got := manager.currentSession(); got != "" {
		t.Fatalf("expected currentSessionID to be cleared, got %q", got)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.endSessionCalls != 1 {
		t.Fatalf("expected EndSession rollback to be called once, got %d", store.endSessionCalls)
	}
}

// buildMsg creates a Deepgram MessageResponse from JSON for testing.
func buildMsg(t *testing.T, raw string) *api.MessageResponse {
	t.Helper()
	var msg api.MessageResponse
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	return &msg
}

func TestManager_BuffersUntilSpeechFinal(t *testing.T) {
	store := newStoreMock()
	hub := &hubMock{}
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour))

	// First is_final without speech_final — should buffer but NOT persist.
	msg1 := buildMsg(t, `{
		"is_final": true,
		"speech_final": false,
		"channel": {"alternatives": [{
			"transcript": "hello world",
			"words": [{"speaker": 0, "punctuated_word": "hello", "start": 0, "end": 0.5},
			           {"speaker": 0, "punctuated_word": "world", "start": 0.5, "end": 1.0}]
		}]}}`)
	if err := manager.Message(msg1); err != nil {
		t.Fatalf("Message msg1 failed: %v", err)
	}

	// Words buffered but nothing persisted yet.
	if len(store.segments) != 0 {
		t.Fatalf("expected no persisted segments after is_final without speech_final, got %d sessions with segments", len(store.segments))
	}

	// Second is_final with speech_final — should flush and persist ALL words.
	msg2 := buildMsg(t, `{
		"is_final": true,
		"speech_final": true,
		"channel": {"alternatives": [{
			"transcript": "how are you",
			"words": [{"speaker": 0, "punctuated_word": "how", "start": 1.1, "end": 1.4},
			           {"speaker": 0, "punctuated_word": "are", "start": 1.4, "end": 1.7},
			           {"speaker": 0, "punctuated_word": "you", "start": 1.7, "end": 2.0}]
		}]}}`)
	if err := manager.Message(msg2); err != nil {
		t.Fatalf("Message msg2 failed: %v", err)
	}

	// Should have persisted segments with all 5 words from both messages.
	sessionID := hub.latestSession
	if sessionID == "" {
		t.Fatal("expected session to have started")
	}
	segs := store.segments[sessionID]
	if len(segs) == 0 {
		t.Fatal("expected segments after speech_final flush")
	}
	// All words should be accumulated — verify total text coverage.
	var allText string
	for _, s := range segs {
		allText += s.Text + " "
	}
	if !strings.Contains(allText, "hello") || !strings.Contains(allText, "you") {
		t.Errorf("expected all words in persisted segments, got %q", allText)
	}
}

func TestManager_InterimBroadcast(t *testing.T) {
	store := newStoreMock()
	hub := &hubMock{}
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour))

	// Send interim (not is_final) message.
	msg := buildMsg(t, `{
		"is_final": false,
		"speech_final": false,
		"channel": {"alternatives": [{
			"transcript": "hello",
			"words": [{"speaker": 0, "punctuated_word": "hello", "start": 0, "end": 0.5}]
		}]}}`)
	if err := manager.Message(msg); err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	hub.mu.Lock()
	gotInterim := hub.interimCount
	hub.mu.Unlock()

	if gotInterim == 0 {
		t.Fatal("expected BroadcastLiveTranscriptInterim to be called for interim message")
	}
	if len(store.segments) != 0 {
		t.Fatal("expected no persisted segments for interim message")
	}
}

func TestManager_UtteranceEndFlushesBuffer(t *testing.T) {
	store := newStoreMock()
	hub := &hubMock{}
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour))

	// Buffer words via is_final without speech_final.
	msg := buildMsg(t, `{
		"is_final": true,
		"speech_final": false,
		"channel": {"alternatives": [{
			"transcript": "testing one two",
			"words": [{"speaker": 0, "punctuated_word": "testing", "start": 0, "end": 0.5},
			           {"speaker": 0, "punctuated_word": "one", "start": 0.5, "end": 0.8},
			           {"speaker": 0, "punctuated_word": "two", "start": 0.8, "end": 1.0}]
		}]}}`)
	if err := manager.Message(msg); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	if len(store.segments) != 0 {
		t.Fatal("expected no segments before UtteranceEnd")
	}

	// UtteranceEnd should flush the buffer.
	if err := manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatalf("UtteranceEnd failed: %v", err)
	}

	sessionID := hub.latestSession
	segs := store.segments[sessionID]
	if len(segs) == 0 {
		t.Fatal("expected segments after UtteranceEnd flush")
	}
}

func TestManager_ForceEndFlushesBuffer(t *testing.T) {
	store := newStoreMock()
	hub := &hubMock{}
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour))

	// Buffer words via is_final without speech_final.
	msg := buildMsg(t, `{
		"is_final": true,
		"speech_final": false,
		"channel": {"alternatives": [{
			"transcript": "before force end",
			"words": [{"speaker": 0, "punctuated_word": "before", "start": 0, "end": 0.4},
			           {"speaker": 0, "punctuated_word": "force", "start": 0.4, "end": 0.8},
			           {"speaker": 0, "punctuated_word": "end", "start": 0.8, "end": 1.0}]
		}]}}`)
	if err := manager.Message(msg); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	// Verify no segments persisted yet (buffer not flushed).
	if len(store.segments) != 0 {
		t.Fatal("expected no segments before ForceEndSession")
	}

	// ForceEndSession should flush buffer THEN end the session.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.ForceEndSession(ctx); err != nil && !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("ForceEndSession failed unexpectedly: %v", err)
	}

	// sessionID is set after flush (BroadcastSessionStarted fires inside flushBuffer).
	sessionID := hub.latestSession
	segs := store.segments[sessionID]
	if len(segs) == 0 {
		t.Fatal("expected buffered words to be flushed by ForceEndSession")
	}
}
