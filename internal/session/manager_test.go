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
	title    map[string]string
	summary  map[string]string
	status   map[string]string
	preset   map[string]string
	audio    map[string]string

	refinementStatus    map[string]string
	refinedTranscript   map[string]string
	canonicalTranscript map[string]string
	transcriptSource    map[string]string
	errorMessage        map[string]string

	endSessionErr   error
	endSessionCalls int
}

func newStoreMock() *storeMock {
	return &storeMock{
		sessions:            map[string]time.Time{},
		segments:            map[string][]transcribe.Segment{},
		title:               map[string]string{},
		summary:             map[string]string{},
		status:              map[string]string{},
		preset:              map[string]string{},
		audio:               map[string]string{},
		refinementStatus:    map[string]string{},
		refinedTranscript:   map[string]string{},
		canonicalTranscript: map[string]string{},
		transcriptSource:    map[string]string{},
		errorMessage:        map[string]string{},
	}
}

func (s *storeMock) CreateSession(id string, startedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = startedAt
	s.status[id] = "active"
	s.refinementStatus[id] = "pending"
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

func (s *storeMock) UpdateSummary(sessionID, title, summary, status, preset, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title[sessionID] = title
	s.summary[sessionID] = summary
	s.status[sessionID] = status
	s.preset[sessionID] = preset
	return nil
}

func (s *storeMock) UpdateSummaryError(sessionID, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errorMessage[sessionID] = errMsg
	return nil
}

func (s *storeMock) UpdateTitle(sessionID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title[sessionID] = title
	return nil
}

func (s *storeMock) DiscardSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = "discarded"
	return nil
}

func (s *storeMock) CountSegments(sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.segments[sessionID]), nil
}

func (s *storeMock) UpdateRefinement(sessionID, transcript, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refinementStatus[sessionID] = status
	if transcript != "" {
		s.refinedTranscript[sessionID] = transcript
	}
	return nil
}

func (s *storeMock) GetRefinement(sessionID string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refinedTranscript[sessionID], s.refinementStatus[sessionID], nil
}

func (s *storeMock) Canonicalize(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.refinementStatus[sessionID]
	if status == "completed" && strings.TrimSpace(s.refinedTranscript[sessionID]) != "" {
		s.canonicalTranscript[sessionID] = s.refinedTranscript[sessionID]
		s.transcriptSource[sessionID] = "refined"
	} else {
		var b strings.Builder
		for _, seg := range s.segments[sessionID] {
			text := strings.TrimSpace(seg.Text)
			if text == "" {
				continue
			}
			b.WriteString(text)
			b.WriteByte('\n')
		}
		s.canonicalTranscript[sessionID] = b.String()
		s.transcriptSource[sessionID] = "streaming"
	}
	return nil
}

func (s *storeMock) GetCanonicalTranscript(sessionID string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canonicalTranscript[sessionID], s.transcriptSource[sessionID], nil
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

type transcriptCapturingSummarizer struct {
	called      chan string
	transcriptC chan string
}

func (s transcriptCapturingSummarizer) Summarize(_ context.Context, sessionID, transcript string) (string, string, string, string, error) {
	if s.called != nil {
		s.called <- sessionID
	}
	if s.transcriptC != nil {
		s.transcriptC <- transcript
	}
	return "Auto title", "## Summary\n- " + transcript, "default", "{}", nil
}

type batchTranscriberMock struct {
	transcript string
	err        error
	delay      time.Duration
	calledPath chan string
}

func (b batchTranscriberMock) Transcribe(_ context.Context, audioPath string) (string, error) {
	if b.calledPath != nil {
		b.calledPath <- audioPath
	}
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	if b.err != nil {
		return "", b.err
	}
	return b.transcript, nil
}

type syncerMock struct {
	called chan string
	err    error
}

func (s syncerMock) SyncSession(_ context.Context, sessionID string) error {
	if s.called != nil {
		s.called <- sessionID
	}
	return s.err
}

type publisherMock struct {
	called chan string
}

func (p publisherMock) PublishSummaryReady(_ context.Context, sessionID string) error {
	if p.called != nil {
		p.called <- sessionID
	}
	return nil
}

func (s summarizerMock) Summarize(_ context.Context, sessionID, transcript string) (string, string, string, string, error) {
	if s.called != nil {
		s.called <- sessionID
	}
	return "Auto title", "## Summary\n- " + transcript, "default", "{}", nil
}

type contextProbeSummarizer struct {
	delay  time.Duration
	stateC chan error
}

func (s contextProbeSummarizer) Summarize(ctx context.Context, _ string, transcript string) (string, string, string, string, error) {
	time.Sleep(s.delay)
	select {
	case <-ctx.Done():
		if s.stateC != nil {
			s.stateC <- ctx.Err()
		}
		return "", "", "default", "{}", ctx.Err()
	default:
		if s.stateC != nil {
			s.stateC <- nil
		}
		return "Auto title", "## Summary\n- " + transcript, "default", "{}", nil
	}
}

type hubMock struct {
	mu            sync.Mutex
	liveCount     int
	startedCount  int
	endedCount    int
	summaryReady  int
	latestSession string
	latestTitle   string
	latestSummary string
	latestStatus  string
	latestPreset  string
	interimCount  int
	components    map[string]struct {
		status  string
		message string
	}
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

func (h *hubMock) BroadcastSummaryReady(sessionID, title, summary, status, preset string) {
	h.mu.Lock()
	h.summaryReady++
	h.latestSession = sessionID
	h.latestTitle = title
	h.latestSummary = summary
	h.latestStatus = status
	h.latestPreset = preset
	h.mu.Unlock()
}

func (h *hubMock) BroadcastComponentStatus(component, status, message string) {
	h.mu.Lock()
	if h.components == nil {
		h.components = map[string]struct {
			status  string
			message string
		}{}
	}
	h.components[component] = struct {
		status  string
		message string
	}{status: status, message: message}
	h.mu.Unlock()
}

func TestManagerLifecycle(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	hub := &hubMock{}
	summaryCalled := make(chan string, 1)
	summarizer := summarizerMock{called: summaryCalled}

	detector := NewDetector(20 * time.Millisecond)
	manager := NewManager(store, recorder, summarizer, hub, detector, 0)

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
	manager := NewManager(store, nil, summarizer, nil, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, nil, summarizer, nil, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)

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
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)

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

func TestManager_GenerateSummary_TriggersSyncWhenSummarizerMissing(t *testing.T) {
	store := newStoreMock()
	syncCalled := make(chan string, 1)
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetSyncer(syncerMock{called: syncCalled})

	manager.generateSummary(context.Background(), "session-no-summarizer", time.Time{})

	select {
	case got := <-syncCalled:
		if got != "session-no-summarizer" {
			t.Fatalf("expected sync session id %q, got %q", "session-no-summarizer", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected sync to trigger when summarizer is nil")
	}
}

func TestManager_GenerateSummary_TriggersSyncAfterSummaryCompleted(t *testing.T) {
	store := newStoreMock()
	if err := store.CreateSession("session-with-summary", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := store.AppendSegment("session-with-summary", transcribe.Segment{Text: "hello sync"}); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	summaryCalled := make(chan string, 1)
	syncCalled := make(chan string, 1)
	manager := NewManager(store, nil, summarizerMock{called: summaryCalled}, nil, NewDetector(time.Hour), 0)
	manager.SetSyncer(syncerMock{called: syncCalled})

	manager.generateSummary(context.Background(), "session-with-summary", time.Time{})

	select {
	case <-summaryCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarizer to run")
	}

	select {
	case got := <-syncCalled:
		if got != "session-with-summary" {
			t.Fatalf("expected sync session id %q, got %q", "session-with-summary", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected sync to trigger after summary completion")
	}
}

func TestManager_GenerateSummary_TriggersPublisherAfterSummaryCompleted(t *testing.T) {
	store := newStoreMock()
	if err := store.CreateSession("session-with-publish", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := store.AppendSegment("session-with-publish", transcribe.Segment{Text: "hello publish"}); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	summaryCalled := make(chan string, 1)
	publishCalled := make(chan string, 1)
	manager := NewManager(store, nil, summarizerMock{called: summaryCalled}, nil, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(publisherMock{called: publishCalled})

	manager.generateSummary(context.Background(), "session-with-publish", time.Time{})

	select {
	case <-summaryCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarizer to run")
	}

	select {
	case got := <-publishCalled:
		if got != "session-with-publish" {
			t.Fatalf("expected publish session id %q, got %q", "session-with-publish", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected publisher to trigger after summary completion")
	}
}

func TestManager_DiscardedShortSession_DoesNotTriggerPublisher(t *testing.T) {
	store := newStoreMock()
	publishCalled := make(chan string, 1)
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 2)
	manager.SetEventPublisher(publisherMock{called: publishCalled})

	startedAt := time.Now().UTC()
	if err := manager.ensureSessionStarted(startedAt); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "too short"}); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}

	select {
	case got := <-publishCalled:
		t.Fatalf("expected no publish for discarded session, got %q", got)
	case <-time.After(200 * time.Millisecond):
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.status[sessionID]; got != storage.SessionDiscarded {
		t.Fatalf("expected discarded session status, got %q", got)
	}
}

func TestManager_BatchRefinement_SubmitsAndStoresRefinedTranscript(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	calledPath := make(chan string, 1)
	transcriptC := make(chan string, 1)

	manager := NewManager(store, recorder, transcriptCapturingSummarizer{transcriptC: transcriptC}, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{
		transcript: "refined canonical transcript",
		calledPath: calledPath,
	})
	manager.SetRefinementWaitTimeout(500 * time.Millisecond)

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "streaming transcript"}); err != nil {
		t.Fatalf("append segment failed: %v", err)
	}

	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}

	select {
	case gotPath := <-calledPath:
		if gotPath == "" || !strings.HasSuffix(gotPath, ".mp3") {
			t.Fatalf("expected mp3 audio path for batch refinement, got %q", gotPath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected batch transcription submission")
	}

	select {
	case usedTranscript := <-transcriptC:
		if !strings.Contains(usedTranscript, "refined canonical transcript") {
			t.Fatalf("expected summary to use refined transcript, got %q", usedTranscript)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarization call")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.refinementStatus[sessionID]; got != "completed" {
		t.Fatalf("expected refinement status completed, got %q", got)
	}
	if got := store.refinedTranscript[sessionID]; got != "refined canonical transcript" {
		t.Fatalf("expected refined transcript stored, got %q", got)
	}
}

func TestManager_BatchRefinement_TimeoutFallsBackToStreamingTranscript(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	transcriptC := make(chan string, 1)

	manager := NewManager(store, recorder, transcriptCapturingSummarizer{transcriptC: transcriptC}, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{
		transcript: "late refinement transcript",
		delay:      250 * time.Millisecond,
	})
	manager.SetRefinementWaitTimeout(25 * time.Millisecond)

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "streaming transcript only"}); err != nil {
		t.Fatalf("append segment failed: %v", err)
	}

	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}

	select {
	case usedTranscript := <-transcriptC:
		if strings.Contains(usedTranscript, "late refinement transcript") {
			t.Fatalf("expected streaming fallback transcript on timeout, got %q", usedTranscript)
		}
		if !strings.Contains(usedTranscript, "streaming transcript only") {
			t.Fatalf("expected summary to use streaming transcript, got %q", usedTranscript)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarization call")
	}

	time.Sleep(300 * time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.refinementStatus[sessionID]; got != "completed" {
		t.Fatalf("expected refinement status eventually completed, got %q", got)
	}
	if got := store.refinedTranscript[sessionID]; got != "late refinement transcript" {
		t.Fatalf("expected late refined transcript persisted, got %q", got)
	}
}

func TestManager_Canonicalization_UsesCanonicalTranscriptForSummary(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	transcriptC := make(chan string, 1)

	manager := NewManager(store, recorder, transcriptCapturingSummarizer{transcriptC: transcriptC}, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{
		transcript: "refined canonical transcript",
	})
	manager.SetRefinementWaitTimeout(500 * time.Millisecond)

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "streaming text"}); err != nil {
		t.Fatalf("append segment failed: %v", err)
	}

	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}

	// Verify the transcript passed to summarizer came from canonical (which should be refined).
	select {
	case usedTranscript := <-transcriptC:
		// The canonical transcript should have been set via Canonicalize,
		// which picks the refined version since batch refinement completed.
		if !strings.Contains(usedTranscript, "refined canonical transcript") {
			t.Fatalf("expected summary to use canonical (refined) transcript, got %q", usedTranscript)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarization call")
	}

	// Verify canonical state in store.
	store.mu.Lock()
	source := store.transcriptSource[sessionID]
	canonical := store.canonicalTranscript[sessionID]
	store.mu.Unlock()

	if source != "refined" {
		t.Fatalf("expected transcript source 'refined', got %q", source)
	}
	if !strings.Contains(canonical, "refined canonical transcript") {
		t.Fatalf("expected canonical transcript to contain refined, got %q", canonical)
	}
}

func TestManager_BatchRefinement_LateCompletionRecanonicalizes(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	transcriptC := make(chan string, 1)

	manager := NewManager(store, recorder, transcriptCapturingSummarizer{transcriptC: transcriptC}, nil, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{
		transcript: "late refined transcript",
		delay:      250 * time.Millisecond,
	})
	manager.SetRefinementWaitTimeout(25 * time.Millisecond)

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	sessionID := manager.currentSession()
	if err := store.AppendSegment(sessionID, transcribe.Segment{Text: "streaming text"}); err != nil {
		t.Fatalf("append segment failed: %v", err)
	}

	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}

	// Summary should use streaming fallback since refinement timed out.
	select {
	case usedTranscript := <-transcriptC:
		if strings.Contains(usedTranscript, "late refined transcript") {
			t.Fatalf("expected streaming fallback transcript on timeout, got %q", usedTranscript)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarization call")
	}

	// Wait for late refinement to complete and re-canonicalize.
	time.Sleep(400 * time.Millisecond)

	store.mu.Lock()
	source := store.transcriptSource[sessionID]
	canonical := store.canonicalTranscript[sessionID]
	store.mu.Unlock()

	// After late refinement, re-canonicalization should have updated to refined.
	if source != "refined" {
		t.Fatalf("expected transcript source 'refined' after late re-canonicalization, got %q", source)
	}
	if canonical != "late refined transcript" {
		t.Fatalf("expected re-canonicalized transcript to contain late refined, got %q", canonical)
	}
}

type failingSummarizerMock struct {
	err error
}

func (s failingSummarizerMock) Summarize(_ context.Context, _, _ string) (string, string, string, string, error) {
	return "", "", "default", "{}", s.err
}

func TestManager_GenerateSummary_StoresErrorOnFailure(t *testing.T) {
	store := newStoreMock()
	if err := store.CreateSession("sess-err", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := store.AppendSegment("sess-err", transcribe.Segment{Text: "hello world this is a test sentence"}); err != nil {
		t.Fatalf("AppendSegment failed: %v", err)
	}

	summarizer := failingSummarizerMock{err: errors.New("gemini json completion: 401 invalid API key")}
	manager := NewManager(store, nil, summarizer, nil, NewDetector(time.Hour), 0)

	manager.generateSummary(context.Background(), "sess-err", time.Now().UTC())

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.status["sess-err"] != storage.SummaryFailed {
		t.Fatalf("expected summary_status %q, got %q", storage.SummaryFailed, store.status["sess-err"])
	}
	if store.errorMessage["sess-err"] == "" {
		t.Fatal("expected error_message to be set, got empty string")
	}
	if !strings.Contains(store.errorMessage["sess-err"], "gemini json completion: 401 invalid API key") {
		t.Fatalf("expected error_message to contain original error, got %q", store.errorMessage["sess-err"])
	}
}

func TestManager_GenerateSummary_SyncSuccessClearsPriorSyncError(t *testing.T) {
	store := newStoreMock()
	hub := &hubMock{}

	if err := store.CreateSession("sess-sync-fail", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := store.CreateSession("sess-sync-ok", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	failCalled := make(chan string, 1)
	failManager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)
	failManager.SetSyncer(syncerMock{called: failCalled, err: errors.New("drive down")})
	failManager.generateSummary(context.Background(), "sess-sync-fail", time.Now().UTC())

	select {
	case <-failCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected failing sync to be called")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		hub.mu.Lock()
		got := hub.components["sync"].status
		hub.mu.Unlock()
		if got == storage.ComponentStatusError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected sync status %q after failure, got %q", storage.ComponentStatusError, got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	successCalled := make(chan string, 1)
	successManager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)
	successManager.SetSyncer(syncerMock{called: successCalled})
	successManager.generateSummary(context.Background(), "sess-sync-ok", time.Now().UTC())

	select {
	case <-successCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected successful sync to be called")
	}

	deadline = time.Now().Add(2 * time.Second)
	for {
		hub.mu.Lock()
		status := hub.components["sync"].status
		message := hub.components["sync"].message
		hub.mu.Unlock()
		if status == storage.ComponentStatusConnected {
			if !strings.Contains(message, "sess-sync-ok") {
				t.Fatalf("expected sync success message to mention session, got %q", message)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected sync status %q after later success, got %q", storage.ComponentStatusConnected, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
