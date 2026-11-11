package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// appendHook, when set, runs first in AppendSegment, outside the lock.
	appendHook func()
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
	if s.appendHook != nil {
		s.appendHook()
	}
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

func (s *storeMock) UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames string, _ int) error {
	return s.UpdateSummary(sessionID, title, summary, status, preset, speakerNames)
}

// StartSummary keeps the title and summary, as the store does.
func (s *storeMock) StartSummary(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[sessionID] = storage.SummaryRunning
	return nil
}

// FailSummary keeps the title and summary, as the store does.
func (s *storeMock) FailSummary(sessionID, errMsg string) (storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[sessionID] = storage.SummaryFailed
	s.errorMessage[sessionID] = errMsg
	return storage.Session{ID: sessionID, Title: s.title[sessionID], Summary: s.summary[sessionID],
		SummaryStatus: storage.SummaryFailed, SummaryPreset: s.preset[sessionID], SummaryError: errMsg}, nil
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

// ClaimRefinement claims the refinement only while the session still names
// audioPath, as the real store does.
func (s *storeMock) ClaimRefinement(sessionID, audioPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if audioPath == "" || s.audio[sessionID] != audioPath {
		return false, nil
	}
	s.refinementStatus[sessionID] = "running"
	delete(s.refinedTranscript, sessionID)
	return true, nil
}

func (s *storeMock) GetRefinement(sessionID string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refinedTranscript[sessionID], s.refinementStatus[sessionID], nil
}

func (s *storeMock) Canonicalize(sessionID string) (int, error) {
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
	return 0, nil
}

func (s *storeMock) GetCanonicalTranscript(sessionID string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canonicalTranscript[sessionID], s.transcriptSource[sessionID], nil
}

func (s *storeMock) GetCanonicalTranscriptWithGeneration(sessionID string) (string, string, int, error) {
	transcript, source, err := s.GetCanonicalTranscript(sessionID)
	return transcript, source, 0, err
}

type recorderMock struct {
	mu       sync.Mutex
	started  []string
	ended    int
	endedIDs []string // the session each EndSession was asked to end

	startErr error
	// endErr, when set, fails every EndSession; unsavedPath is the raw file
	// DiscardUnsaved then reports, and discards counts its calls.
	endErr      error
	unsavedPath string
	discards    int
	// startHook and endHook, when set, run first in StartSession and
	// EndSession, outside the mock's lock, so a test can hold a call open.
	startHook func()
	endHook   func()
}

func (r *recorderMock) StartSession(id string) error {
	if r.startHook != nil {
		r.startHook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return r.startErr
	}
	r.started = append(r.started, id)
	return nil
}

// EndSession saves the recording of the session it is given, as the real
// recorder does: a session it never started has nothing to save, so a manager
// that ends the wrong session gets no recording back.
func (r *recorderMock) EndSession(sessionID string) (string, error) {
	if r.endHook != nil {
		r.endHook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended++
	r.endedIDs = append(r.endedIDs, sessionID)
	if r.endErr != nil {
		return "", r.endErr
	}
	if sessionID == "" || !slices.Contains(r.started, sessionID) {
		return "", nil
	}
	return "data/audio/" + sessionID + ".mp3", nil
}

func (r *recorderMock) DiscardUnsaved() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discards++
	return r.unsavedPath
}

func (r *recorderMock) counts() (started, ended int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started), r.ended
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
}

func (s syncerMock) SyncSession(_ context.Context, sessionID string) error {
	if s.called != nil {
		s.called <- sessionID
	}
	return nil
}

type publisherMock struct {
	called  chan string
	started chan string
	ended   chan string
}

func (p publisherMock) PublishSummaryReady(_ context.Context, sessionID string) error {
	if p.called != nil {
		p.called <- sessionID
	}
	return nil
}

func (p publisherMock) PublishSessionStarted(_ context.Context, sessionID string) error {
	if p.started != nil {
		p.started <- sessionID
	}
	return nil
}

func (p publisherMock) PublishSessionEnded(_ context.Context, sessionID string) error {
	if p.ended != nil {
		p.ended <- sessionID
	}
	return nil
}

type blockingPublisher struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (p blockingPublisher) PublishSessionStarted(_ context.Context, _ string) error {
	p.entered <- struct{}{}
	<-p.release
	return nil
}

func (blockingPublisher) PublishSessionEnded(context.Context, string) error {
	return nil
}

func (blockingPublisher) PublishSummaryReady(context.Context, string) error {
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

type componentStatusEvent struct {
	component string
	status    string
	message   string
}

type hubMock struct {
	mu                sync.Mutex
	liveCount         int
	startedCount      int
	endedCount        int
	summaryReady      int
	latestSession     string
	latestTitle       string
	latestSummary     string
	latestStatus      string
	latestPreset      string
	interimCount      int
	componentStatuses []componentStatusEvent
	// endedHook, when set, runs first in BroadcastSessionEnded, outside the
	// mock's lock: after the session has ended, before the end returns.
	endedHook func(sessionID string)
}

func (h *hubMock) BroadcastLiveTranscript(_ string, _ transcribe.Segment) {
	h.mu.Lock()
	h.liveCount++
	h.mu.Unlock()
}

func (h *hubMock) BroadcastLiveTranscriptInterim(_ string, _ int, _ string, _ float64) {
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
	if h.endedHook != nil {
		h.endedHook(sessionID)
	}
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
	h.componentStatuses = append(h.componentStatuses, componentStatusEvent{component: component, status: status, message: message})
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

// A session end whose database write failed is retried with the recording the
// failed end saved. The recorder is not asked again: it handed that recording
// over already, and a retry that asked it would save the session without it.
func TestManager_RetriedEndKeepsTheRecordingAFailedEndSaved(t *testing.T) {
	store := newStoreMock()
	store.endSessionErr = errors.New("disk I/O error")
	recorder := &recorderMock{}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)
	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	id := manager.currentSession()

	if err := manager.ForceEndSession(context.Background()); err == nil {
		t.Fatal("ForceEndSession succeeded while the database refused the end")
	}
	store.mu.Lock()
	store.endSessionErr = nil
	store.mu.Unlock()
	if err := manager.ForceEndSession(context.Background()); err != nil {
		t.Fatalf("retried ForceEndSession failed: %v", err)
	}

	if _, ended := recorder.counts(); ended != 1 {
		t.Errorf("recorder asked to end the session %d times, want once", ended)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got, want := store.audio[id], "data/audio/"+id+".mp3"; got != want {
		t.Errorf("session ended with recording %q, want %q", got, want)
	}
}

// Ending a session asks the recorder for that session's recording, by ID, and
// files what it gets back under that session: one session after another,
// each ends with its own recording.
func TestManager_EndingASessionEndsItsOwnRecording(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)
	var ids []string
	for range 2 {
		if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
			t.Fatalf("ensureSessionStarted failed: %v", err)
		}
		ids = append(ids, manager.currentSession())
		if err := manager.ForceEndSession(context.Background()); err != nil {
			t.Fatalf("ForceEndSession failed: %v", err)
		}
	}

	recorder.mu.Lock()
	endedIDs := slices.Clone(recorder.endedIDs)
	recorder.mu.Unlock()
	if !slices.Equal(endedIDs, ids) {
		t.Errorf("recorder asked to end %q, want the sessions ended, %q", endedIDs, ids)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, id := range ids {
		if got, want := store.audio[id], "data/audio/"+id+".mp3"; got != want {
			t.Errorf("session %s ended with recording %q, want %q", id, got, want)
		}
	}
}

// A recording that can never be saved (every save fails, as when its raw file
// was deleted) does not keep the session current for good: the end that fails
// to save it for the fifth time ends the session without it, tells the
// recorder to stop retrying it, and logs where its raw audio was left.
func TestManager_EndGivesUpOnARecordingThatCannotBeSaved(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{endErr: errors.New("encode wav fallback: no space left on device"), unsavedPath: "data/audio/raw.pcm"}
	var logs bytes.Buffer
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0, slog.New(slog.NewTextHandler(&logs, nil)))
	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("ensureSessionStarted failed: %v", err)
	}
	id := manager.currentSession()

	for attempt := 1; attempt < 5; attempt++ {
		if err := manager.ForceEndSession(context.Background()); err == nil {
			t.Fatalf("end %d succeeded with the recording unsaved", attempt)
		}
	}
	if err := manager.ForceEndSession(context.Background()); err != nil {
		t.Fatalf("end 5 failed: %v; want the session ended without its recording", err)
	}
	if got := manager.currentSession(); got != "" {
		t.Fatalf("session %s still current after the recorder gave up", got)
	}
	store.mu.Lock()
	status, audioPath := store.status[id], store.audio[id]
	store.mu.Unlock()
	if status != "ended" || audioPath != "" {
		t.Errorf("session %s is %q with recording %q, want ended with none", id, status, audioPath)
	}
	recorder.mu.Lock()
	discards := recorder.discards
	recorder.mu.Unlock()
	if discards != 1 {
		t.Errorf("recorder told to give up the recording %d times, want once", discards)
	}
	if !strings.Contains(logs.String(), "data/audio/raw.pcm") || !strings.Contains(logs.String(), id) {
		t.Errorf("log does not say where session %s's raw audio is: %s", id, logs.String())
	}
}

// Pressing Record while a Stop of a manual session is still restoring the
// silence timeout reads the detector's timeout under its own lock, so the
// two presses do not race (this test fails under go test -race otherwise).
func TestManager_ManualStartWhileAStopRestoresTheSilenceTimeout(t *testing.T) {
	manager := NewManager(newStoreMock(), &recorderMock{}, nil, &hubMock{}, NewDetector(time.Hour), 0)
	ctx := context.Background()
	id, err := manager.ManualStartSession(ctx, "")
	if err != nil {
		t.Fatalf("ManualStartSession failed: %v", err)
	}
	for range 100 {
		stopped := make(chan error, 1)
		go func(id string) { stopped <- manager.StopSession(ctx, id) }(id)
		for {
			if id, err = manager.ManualStartSession(ctx, ""); err == nil {
				break
			}
			if !errors.Is(err, ErrSessionAlreadyActive) {
				t.Fatalf("ManualStartSession failed: %v", err)
			}
			runtime.Gosched()
		}
		if err := <-stopped; err != nil {
			t.Fatalf("StopSession failed: %v", err)
		}
	}
}

// Record pressed while a manual session's Stop is finishing (the session has
// ended, the Stop has not returned) starts a manual session that keeps its own
// 5-minute silence timeout: the earlier Stop must not reset the detector or
// clear what the new session needs to restore. The new session's own Stop then
// restores the default.
func TestManager_RecordDuringAStopKeepsTheNewManualTimeout(t *testing.T) {
	detector := NewDetector(30 * time.Second)
	hub := &hubMock{}
	manager := NewManager(newStoreMock(), &recorderMock{}, nil, hub, detector, 0)
	ctx := context.Background()
	first, err := manager.ManualStartSession(ctx, "")
	if err != nil {
		t.Fatalf("ManualStartSession failed: %v", err)
	}

	var second string
	var secondErr error
	var once sync.Once
	hub.endedHook = func(string) {
		once.Do(func() { second, secondErr = manager.ManualStartSession(ctx, "") })
	}
	if err := manager.StopSession(ctx, first); err != nil {
		t.Fatalf("StopSession(%s) failed: %v", first, err)
	}
	if secondErr != nil {
		t.Fatalf("Record pressed after the first session ended: %v", secondErr)
	}
	if got := detector.Timeout(); got != 5*time.Minute {
		t.Errorf("silence timeout during the second manual session = %v, want 5m", got)
	}

	if err := manager.StopSession(ctx, second); err != nil {
		t.Fatalf("StopSession(%s) failed: %v", second, err)
	}
	if got := detector.Timeout(); got != 30*time.Second {
		t.Errorf("silence timeout after the second manual session = %v, want the default 30s", got)
	}
}

// A Stop of a manual session that fails leaves the session current, so it
// keeps its 5-minute silence timeout until an end of it succeeds.
func TestManager_FailedStopKeepsTheManualTimeout(t *testing.T) {
	detector := NewDetector(30 * time.Second)
	store := newStoreMock()
	manager := NewManager(store, &recorderMock{}, nil, &hubMock{}, detector, 0)
	ctx := context.Background()
	id, err := manager.ManualStartSession(ctx, "")
	if err != nil {
		t.Fatalf("ManualStartSession failed: %v", err)
	}

	store.mu.Lock()
	store.endSessionErr = errors.New("disk I/O error")
	store.mu.Unlock()
	if err := manager.StopSession(ctx, id); err == nil {
		t.Fatal("StopSession succeeded while the database refused the end")
	}
	if got := detector.Timeout(); got != 5*time.Minute {
		t.Errorf("silence timeout after a failed Stop, with the session still current = %v, want 5m", got)
	}

	store.mu.Lock()
	store.endSessionErr = nil
	store.mu.Unlock()
	if err := manager.StopSession(ctx, id); err != nil {
		t.Fatalf("retried StopSession failed: %v", err)
	}
	if got := detector.Timeout(); got != 30*time.Second {
		t.Errorf("silence timeout after the manual session ended = %v, want the default 30s", got)
	}
}

// A manual session the silence timer ends gets the default timeout back too,
// as one ended by Stop does.
func TestManager_SilenceEndOfAManualSessionRestoresTheTimeout(t *testing.T) {
	detector := NewDetector(30 * time.Second)
	manager := NewManager(newStoreMock(), &recorderMock{}, nil, &hubMock{}, detector, 0)
	if _, err := manager.ManualStartSession(context.Background(), ""); err != nil {
		t.Fatalf("ManualStartSession failed: %v", err)
	}
	// What the silence timer calls.
	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("endCurrentSession failed: %v", err)
	}
	if got := detector.Timeout(); got != 30*time.Second {
		t.Errorf("silence timeout after the silence timer ended a manual session = %v, want the default 30s", got)
	}
}

// Test helpers: start a session directly, and read the current one.
func (m *Manager) ensureSessionStarted(now time.Time) error {
	m.mu.Lock()
	_, err := m.claimSessionLocked(now)
	return err
}

func (m *Manager) currentSession() string {
	id, _ := m.ActiveSession()
	return id
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

	store.mu.Lock()
	status := store.status["session-no-summarizer"]
	store.mu.Unlock()
	if status != storage.SummaryCompleted {
		t.Errorf("summary status = %q, want %q", status, storage.SummaryCompleted)
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

// A short session is discarded. Its session_started and session_ended still
// go to Envoy, as they go to the hub, so a consumer that saw it start sees it
// end; no summary_ready follows, since nothing is summarized.
func TestManager_DiscardedShortSession_SendsLifecycleEventsButNoSummary(t *testing.T) {
	store := newStoreMock()
	publisher := publisherMock{
		called:  make(chan string, 1),
		started: make(chan string, 1),
		ended:   make(chan string, 1),
	}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 2)
	manager.SetEventPublisher(publisher)

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

	for name, events := range map[string]chan string{"session_started": publisher.started, "session_ended": publisher.ended} {
		select {
		case got := <-events:
			if got != sessionID {
				t.Fatalf("%s session ID = %q, want %q", name, got, sessionID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %s event for the discarded session", name)
		}
	}
	select {
	case got := <-publisher.called:
		t.Fatalf("expected no summary_ready publish for discarded session, got %q", got)
	case <-time.After(200 * time.Millisecond):
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.status[sessionID]; got != storage.SessionDiscarded {
		t.Fatalf("expected discarded session status, got %q", got)
	}
}

func TestSessionStartAndEndAreSent(t *testing.T) {
	store := newStoreMock()
	publisher := publisherMock{
		started: make(chan string, 1),
		ended:   make(chan string, 1),
	}
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(publisher)

	startedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	if err := manager.ensureSessionStarted(startedAt); err != nil {
		t.Fatalf("start session: %v", err)
	}
	sessionID := manager.currentSession()
	if sessionID == "" {
		t.Fatal("expected session ID")
	}

	select {
	case got := <-publisher.started:
		if got != sessionID {
			t.Fatalf("started event session ID = %q, want %q", got, sessionID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected session_started event")
	}

	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("end session: %v", err)
	}
	select {
	case got := <-publisher.ended:
		if got != sessionID {
			t.Fatalf("ended event session ID = %q, want %q", got, sessionID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected session_ended event")
	}
}

func TestManualSessionStartIsSent(t *testing.T) {
	publisher := publisherMock{started: make(chan string, 1)}
	manager := NewManager(newStoreMock(), nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(publisher)

	sessionID, err := manager.ManualStartSession(context.Background(), "Manual topic")
	if err != nil {
		t.Fatalf("ManualStartSession: %v", err)
	}
	select {
	case got := <-publisher.started:
		if got != sessionID {
			t.Fatalf("started event session ID = %q, want %q", got, sessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected session_started event")
	}
}

func TestStopSessionStartPublishesSuppressesNewStartEvents(t *testing.T) {
	publisher := publisherMock{started: make(chan string, 1)}
	manager := NewManager(newStoreMock(), nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(publisher)
	manager.StopSessionStartPublishes()

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("start session: %v", err)
	}
	select {
	case got := <-publisher.started:
		t.Fatalf("unexpected session_started event for %q after shutdown began", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWaitForLifecyclePublishesHonorsContext(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	manager := NewManager(newStoreMock(), nil, nil, nil, NewDetector(time.Hour), 0)
	manager.SetEventPublisher(blockingPublisher{entered: entered, release: release})

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("start session: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("expected session_started publish")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.WaitForLifecyclePublishes(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForLifecyclePublishes error = %v, want deadline exceeded", err)
	}

	close(release)
	if err := manager.WaitForLifecyclePublishes(context.Background()); err != nil {
		t.Fatalf("WaitForLifecyclePublishes after release: %v", err)
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

func TestManager_GenerateSummary_BroadcastsSyncConnectedOnSuccess(t *testing.T) {
	store := newStoreMock()
	hub := &hubMock{}
	syncCalled := make(chan string, 1)
	manager := NewManager(store, nil, nil, hub, NewDetector(time.Hour), 0)
	manager.SetSyncer(syncerMock{called: syncCalled})

	manager.generateSummary(context.Background(), "session-sync-connected", time.Time{})

	select {
	case <-syncCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected sync to trigger when summarizer is nil")
	}

	deadline := time.After(2 * time.Second)
	for {
		hub.mu.Lock()
		found := false
		for _, e := range hub.componentStatuses {
			if e.component == "sync" && e.status == storage.ComponentStatusConnected {
				found = true
				break
			}
		}
		hub.mu.Unlock()
		if found {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected sync=connected component status broadcast on successful sync")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestManager_RefineSession_RefinesAndResummarizes(t *testing.T) {
	store := newStoreMock()
	if err := store.CreateSession("sess-refine", time.Now().UTC()); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := store.EndSession("sess-refine", time.Now().UTC(), "data/audio/sess-refine.mp3"); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}
	hub := &hubMock{}
	summaryCalled := make(chan string, 1)
	manager := NewManager(store, nil, summarizerMock{called: summaryCalled}, hub, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{transcript: "refined transcript text"})

	err := manager.RefineSession(context.Background(), "sess-refine", "data/audio/sess-refine.mp3", time.Time{})
	if err != nil {
		t.Fatalf("RefineSession: %v", err)
	}

	transcript, status, _ := store.GetRefinement("sess-refine")
	if status != storage.RefinementCompleted {
		t.Fatalf("expected refinement status %q, got %q", storage.RefinementCompleted, status)
	}
	if transcript != "refined transcript text" {
		t.Fatalf("expected refined transcript stored, got %q", transcript)
	}

	select {
	case <-summaryCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected summarizer to run after refinement")
	}
}

func TestManager_RefineSession_ErrorsWithoutBatchTranscriber(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, nil, summarizerMock{}, &hubMock{}, NewDetector(time.Hour), 0)

	err := manager.RefineSession(context.Background(), "sess-x", "data/audio/sess-x.mp3", time.Time{})
	if err == nil {
		t.Fatal("expected error when batch transcription is not configured")
	}
}

func TestManager_RefineSession_ErrorsWithoutAudio(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, nil, summarizerMock{}, &hubMock{}, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchTranscriberMock{transcript: "x"})

	err := manager.RefineSession(context.Background(), "sess-x", "  ", time.Time{})
	if err == nil {
		t.Fatal("expected error when session has no audio")
	}
}

// fileTranscriber reads the recording it is asked to transcribe, as a real
// batch transcriber does, and counts its calls.
type fileTranscriber struct{ calls atomic.Int32 }

func (f *fileTranscriber) Transcribe(_ context.Context, audioPath string) (string, error) {
	f.calls.Add(1)
	if _, err := os.ReadFile(audioPath); err != nil {
		return "", err
	}
	return "refined", nil
}

// Review note 5: a session's end saves its audio path, then its refinement
// marks itself running, and GC can remove the audio in between (it removes
// only what is not being refined). A refinement that starts on audio GC has
// removed does not run: there is no file to read, and marking it failed
// would offer a retry that can only fail.
func TestManager_RefinementDoesNotStartOnAudioGCRemoved(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "20261106100000"
	started := time.Now().UTC().Add(-time.Hour)
	saved := filepath.Join(t.TempDir(), id+".mp3")
	if err := os.WriteFile(saved, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(id, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), saved); err != nil {
		t.Fatalf("end session: %v", err)
	}
	// GC removes the recording to free space and records it.
	if err := os.Remove(saved); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET audio_path = '', audio_removed_at = ?, refinement_status = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), storage.RefinementNoAudio, id); err != nil {
		t.Fatalf("record the removal: %v", err)
	}

	transcriber := &fileTranscriber{}
	manager := NewManager(store, nil, nil, &hubMock{}, NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(transcriber)
	if err := manager.RefineSession(context.Background(), id, saved, started); err != nil {
		t.Fatalf("RefineSession: %v", err)
	}

	if n := transcriber.calls.Load(); n != 0 {
		t.Errorf("refinement transcribed audio GC had removed (%d calls)", n)
	}
	if _, status, err := store.GetRefinement(id); err != nil || status != storage.RefinementNoAudio {
		t.Errorf("refinement_status %q (%v), want %q: there is no audio to refine", status, err, storage.RefinementNoAudio)
	}
}

const pausedCaptureSpeechFinal = `{
	"is_final": true,
	"speech_final": true,
	"channel": {"alternatives": [{
		"transcript": "said before the pause",
		"words": [{"speaker": 0, "punctuated_word": "said", "start": 0, "end": 0.4},
		           {"speaker": 0, "punctuated_word": "before", "start": 0.4, "end": 0.8},
		           {"speaker": 0, "punctuated_word": "the", "start": 0.8, "end": 0.9},
		           {"speaker": 0, "punctuated_word": "pause", "start": 0.9, "end": 1.2}]
	}]}}`

// Deepgram's reply to Finalize: final, not end of speech.
const pausedCaptureFinalizeReply = `{
	"is_final": true,
	"speech_final": false,
	"from_finalize": true,
	"channel": {"alternatives": [{
		"transcript": "last words",
		"words": [{"speaker": 0, "punctuated_word": "last", "start": 1.3, "end": 1.6},
		           {"speaker": 0, "punctuated_word": "words", "start": 1.6, "end": 2.0}]
	}]}}`

func segmentText(t *testing.T, store *storeMock, sessionID string) string {
	t.Helper()
	segments, err := store.GetSegments(sessionID)
	if err != nil {
		t.Fatalf("GetSegments failed: %v", err)
	}
	var text []string
	for _, seg := range segments {
		text = append(text, seg.Text)
	}
	return strings.Join(text, " ")
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A transcript that Deepgram delivers while capture is paused must not open a
// session or a recording file; after resume, the next transcript does.
func TestManager_TranscriptWhilePausedStartsNoSessionOrRecording(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	hub := &hubMock{}
	manager := NewManager(store, recorder, nil, hub, NewDetector(time.Hour), 0)

	manager.PauseCapture()
	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message while paused failed: %v", err)
	}
	if err := manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatalf("UtteranceEnd while paused failed: %v", err)
	}

	if got := len(store.sessions); got != 0 {
		t.Fatalf("store has %d sessions after a transcript while paused, want 0", got)
	}
	if started, _ := recorder.counts(); started != 0 {
		t.Fatalf("recorder started %d sessions while paused, want 0", started)
	}
	if id, _ := manager.ActiveSession(); id != "" {
		t.Fatalf("active session %q while paused, want none", id)
	}
	hub.mu.Lock()
	startedEvents := hub.startedCount
	hub.mu.Unlock()
	if startedEvents != 0 {
		t.Fatalf("broadcast %d session_started events while paused, want 0", startedEvents)
	}

	manager.ResumeCapture()
	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message after resume failed: %v", err)
	}
	if started, _ := recorder.counts(); started != 1 {
		t.Fatalf("recorder started %d sessions after resume, want 1", started)
	}
	if id, _ := manager.ActiveSession(); id == "" {
		t.Fatal("no active session after a transcript following resume")
	}
}

// After a pause, Deepgram's reply to Finalize still belongs to the session that
// was recording: AwaitFinalize reports when it is in, and ending the session
// then saves those last words.
func TestManager_FinalizeReplyAfterPauseIsSavedToTheEndingSession(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	manager := NewManager(store, recorder, nil, &hubMock{}, NewDetector(time.Hour), 0)

	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	sessionID, _ := manager.ActiveSession()
	if sessionID == "" {
		t.Fatal("expected an active session before the pause")
	}

	manager.PauseCapture()
	finalized := manager.AwaitFinalize()
	select {
	case <-finalized:
		t.Fatal("AwaitFinalize reported before Deepgram replied")
	default:
	}
	if err := manager.Message(buildMsg(t, pausedCaptureFinalizeReply)); err != nil {
		t.Fatalf("Message (finalize reply) failed: %v", err)
	}
	waitClosed(t, finalized, "the finalize reply to be reported")

	if err := manager.ForceEndSession(context.Background()); err != nil {
		t.Fatalf("ForceEndSession failed: %v", err)
	}
	if id, _ := manager.ActiveSession(); id != "" {
		t.Fatalf("session %q still active after it ended", id)
	}
	if _, ended := recorder.counts(); ended != 1 {
		t.Fatalf("recorder ended %d sessions, want 1", ended)
	}
	if text := segmentText(t, store, sessionID); !strings.Contains(text, "last words") {
		t.Fatalf("finalize reply not saved to the session, segments: %q", text)
	}
}

// A Finalize reply with no words (nothing was buffered) still reports.
func TestManager_AwaitFinalizeReportsAnEmptyReply(t *testing.T) {
	manager := NewManager(newStoreMock(), &recorderMock{}, nil, &hubMock{}, NewDetector(time.Hour), 0)

	finalized := manager.AwaitFinalize()
	if err := manager.Message(buildMsg(t, `{"is_final": true, "from_finalize": true, "channel": {"alternatives": [{"transcript": ""}]}}`)); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	waitClosed(t, finalized, "the empty finalize reply to be reported")
}

// PauseCapture only blocks new sessions: it does not wait for a session start
// already under way, since the capture gate stays open until it returns.
// WaitForSessionStarts waits for that start, so the session end that follows a
// pause cannot run while the session is half created.
func TestManager_PauseCaptureDoesNotWaitForASessionStartInFlight(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	entered := make(chan struct{})
	release := make(chan struct{})
	recorder.startHook = func() {
		close(entered)
		<-release
	}
	manager := NewManager(store, recorder, nil, &hubMock{}, NewDetector(time.Hour), 0)

	messageErr := make(chan error, 1)
	go func() { messageErr <- manager.Message(buildMsg(t, pausedCaptureSpeechFinal)) }()
	waitClosed(t, entered, "the session start to reach the recorder")

	paused := make(chan struct{})
	go func() {
		manager.PauseCapture()
		close(paused)
	}()
	waitClosed(t, paused, "PauseCapture while a session start was in flight")

	waited := make(chan struct{})
	go func() {
		manager.WaitForSessionStarts()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("WaitForSessionStarts returned while a session start was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	waitClosed(t, waited, "WaitForSessionStarts after the session start finished")
	if err := <-messageErr; err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	if err := manager.ForceEndSession(context.Background()); err != nil {
		t.Fatalf("ForceEndSession failed: %v", err)
	}
	if started, ended := recorder.counts(); started != 1 || ended != 1 {
		t.Fatalf("recorder started %d and ended %d sessions, want 1 and 1", started, ended)
	}
}

// blockFirstEnd makes the recorder's first EndSession wait until release is
// closed, as a long encode does, and closes entered when it starts.
func blockFirstEnd(recorder *recorderMock) (entered, release chan struct{}) {
	entered = make(chan struct{})
	release = make(chan struct{})
	var first atomic.Bool
	recorder.endHook = func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	return entered, release
}

// Two ends of one session at once (the silence timer and a pause) end it once:
// the second waits for the first, then finds no session. It must not end the
// session again and overwrite its saved audio path.
func TestManager_ConcurrentEndsEndTheSessionOnce(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	entered, release := blockFirstEnd(recorder)
	manager := NewManager(store, recorder, nil, &hubMock{}, NewDetector(time.Hour), 0)
	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	sessionID, _ := manager.ActiveSession()

	firstErr := make(chan error, 1)
	go func() { firstErr <- manager.ForceEndSession(context.Background()) }()
	waitClosed(t, entered, "the first end to reach the recorder")

	secondErr := make(chan error, 1)
	go func() { secondErr <- manager.ForceEndSession(context.Background()) }()
	select {
	case err := <-secondErr:
		t.Fatalf("second end returned %v while the first was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first end failed: %v", err)
	}
	if err := <-secondErr; !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("second end after the first finished: got %v, want ErrNoActiveSession", err)
	}

	store.mu.Lock()
	calls, audioPath := store.endSessionCalls, store.audio[sessionID]
	store.mu.Unlock()
	if calls != 1 {
		t.Fatalf("store ended the session %d times, want 1", calls)
	}
	if want := "data/audio/" + sessionID + ".mp3"; audioPath != want {
		t.Fatalf("saved audio path = %q, want %q", audioPath, want)
	}
}

// A session that starts in the same second as the one before it (a short
// session ended by a pause, then a resume) gets its own ID. With the same ID
// its words and recording would be filed under the earlier session.
func TestManager_SessionsStartedInTheSameSecondGetDistinctIDs(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, &recorderMock{}, nil, &hubMock{}, NewDetector(time.Hour), 0)

	var ids []string
	for range 3 {
		if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
			t.Fatalf("Message failed: %v", err)
		}
		id, _ := manager.ActiveSession()
		ids = append(ids, id)
		if err := manager.ForceEndSession(context.Background()); err != nil {
			t.Fatalf("ForceEndSession failed: %v", err)
		}
	}
	id, err := manager.ManualStartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("ManualStartSession failed: %v", err)
	}
	ids = append(ids, id)

	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("session IDs %v repeat %s", ids, id)
		}
		seen[id] = true
	}
}

// A pause that lands while another end of the session (the silence timer's) is
// still encoding waits for that end. Otherwise the pause ends nothing, resume
// follows, and speech after the resume is filed into the session from before
// the pause.
func TestManager_PauseDuringSessionEndKeepsPostResumeSpeechOut(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	entered, release := blockFirstEnd(recorder)
	manager := NewManager(store, recorder, nil, &hubMock{}, NewDetector(time.Hour), 0)
	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	before, _ := manager.ActiveSession()

	timerEnd := make(chan error, 1)
	go func() { timerEnd <- manager.ForceEndSession(context.Background()) }()
	waitClosed(t, entered, "the silence timer's end to reach the recorder")

	// The capture pause: block starts, end the session, then the resume and
	// the first words after it.
	afterResume := buildMsg(t, `{
		"is_final": true,
		"speech_final": true,
		"channel": {"alternatives": [{
			"transcript": "after resume",
			"words": [{"speaker": 0, "punctuated_word": "after", "start": 0, "end": 0.3},
			           {"speaker": 0, "punctuated_word": "resume", "start": 0.3, "end": 0.7}]
		}]}}`)
	pauseThenResume := make(chan error, 1)
	go func() {
		manager.PauseCapture()
		if err := manager.ForceEndSession(context.Background()); err != nil && !errors.Is(err, ErrNoActiveSession) {
			pauseThenResume <- err
			return
		}
		manager.ResumeCapture()
		pauseThenResume <- manager.Message(afterResume)
	}()
	// Give a pause that does not wait time to run all the way through first.
	var pauseErr error
	pauseDone := false
	select {
	case pauseErr = <-pauseThenResume:
		pauseDone = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-timerEnd; err != nil {
		t.Fatalf("silence timer's end failed: %v", err)
	}
	if !pauseDone {
		select {
		case pauseErr = <-pauseThenResume:
		case <-time.After(5 * time.Second):
			t.Fatal("pause did not finish after the session end it waited for")
		}
	}
	if pauseErr != nil {
		t.Fatalf("pause, resume and message failed: %v", pauseErr)
	}

	if text := segmentText(t, store, before); strings.Contains(text, "after resume") {
		t.Fatalf("post-resume words landed in the pre-pause session %s: %q", before, text)
	}
	after, _ := manager.ActiveSession()
	if after == "" || after == before {
		t.Fatalf("active session after resume = %q, want a new session (pre-pause was %q)", after, before)
	}
}

// An end that has to wait for another end of the session gives up at its own
// deadline: POST /api/sessions/{id}/stop allows 5 s, not another end's encode.
func TestManager_EndWaitingOnAnotherEndHonorsItsDeadline(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	entered, release := blockFirstEnd(recorder)
	manager := NewManager(store, recorder, nil, &hubMock{}, NewDetector(time.Hour), 0)
	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message failed: %v", err)
	}
	sessionID, _ := manager.ActiveSession()

	firstErr := make(chan error, 1)
	go func() { firstErr <- manager.ForceEndSession(context.Background()) }()
	waitClosed(t, entered, "the first end to reach the recorder")
	defer func() {
		close(release)
		if err := <-firstErr; err != nil {
			t.Errorf("first end failed: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	stopErr := make(chan error, 1)
	go func() { stopErr <- manager.StopSession(ctx, sessionID) }()
	select {
	case err := <-stopErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StopSession past its deadline: got %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopSession ignored its deadline while another end was running")
	}
}

// Words that a flush took before a pause stay with the session they were
// taken for, even when the whole pause and resume happen while that flush is
// still saving them. They must not open the first session after the resume.
func TestManager_FlushTakenBeforePauseDoesNotOpenAPostResumeSession(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, &recorderMock{}, nil, &hubMock{}, NewDetector(time.Hour), 0)

	entered := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	store.appendHook = func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	twoSpeakers := buildMsg(t, `{
		"is_final": true,
		"speech_final": true,
		"channel": {"alternatives": [{
			"transcript": "alpha bravo",
			"words": [{"speaker": 0, "punctuated_word": "alpha", "start": 0, "end": 0.4},
			           {"speaker": 1, "punctuated_word": "bravo", "start": 0.5, "end": 0.9}]
		}]}}`)
	flushed := make(chan error, 1)
	go func() { flushed <- manager.Message(twoSpeakers) }()
	waitClosed(t, entered, "the first segment to reach the store")
	before, _ := manager.ActiveSession()

	manager.PauseCapture()
	if err := manager.ForceEndSession(context.Background()); err != nil {
		t.Fatalf("ForceEndSession failed: %v", err)
	}
	manager.ResumeCapture()
	close(release)
	if err := <-flushed; err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	if id, _ := manager.ActiveSession(); id != "" {
		t.Fatalf("a pre-pause word opened post-resume session %s", id)
	}
	if text := segmentText(t, store, before); !strings.Contains(text, "bravo") {
		t.Fatalf("pre-pause session %s lost the flushed word, segments: %q", before, text)
	}
}

func TestManager_ManualStartSessionRefusedWhilePaused(t *testing.T) {
	store := newStoreMock()
	recorder := &recorderMock{}
	manager := NewManager(store, recorder, nil, &hubMock{}, NewDetector(time.Hour), 0)

	manager.PauseCapture()
	id, err := manager.ManualStartSession(context.Background(), "")
	if !errors.Is(err, ErrCapturePaused) {
		t.Fatalf("ManualStartSession while paused: got (%q, %v), want ErrCapturePaused", id, err)
	}
	if got := len(store.sessions); got != 0 {
		t.Fatalf("store has %d sessions after a refused start, want 0", got)
	}
	if started, _ := recorder.counts(); started != 0 {
		t.Fatalf("recorder started %d sessions while paused, want 0", started)
	}

	manager.ResumeCapture()
	if _, err := manager.ManualStartSession(context.Background(), ""); err != nil {
		t.Fatalf("ManualStartSession after resume failed: %v", err)
	}
}

// Words Deepgram finalizes during a pause with no utterance end stay in the
// word buffer. Resume drops them, so they do not open the first session after
// the pause.
func TestManager_ResumeCaptureDropsWordsBufferedWhilePaused(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, &recorderMock{}, nil, &hubMock{}, NewDetector(time.Hour), 0)

	manager.PauseCapture()
	if err := manager.Message(buildMsg(t, `{
		"is_final": true,
		"speech_final": false,
		"channel": {"alternatives": [{
			"transcript": "stale words",
			"words": [{"speaker": 0, "punctuated_word": "stale", "start": 0, "end": 0.3},
			           {"speaker": 0, "punctuated_word": "words", "start": 0.3, "end": 0.6}]
		}]}}`)); err != nil {
		t.Fatalf("Message while paused failed: %v", err)
	}

	manager.ResumeCapture()
	if err := manager.Message(buildMsg(t, pausedCaptureSpeechFinal)); err != nil {
		t.Fatalf("Message after resume failed: %v", err)
	}
	sessionID, _ := manager.ActiveSession()
	if sessionID == "" {
		t.Fatal("no active session after a transcript following resume")
	}
	if text := segmentText(t, store, sessionID); strings.Contains(text, "stale") {
		t.Fatalf("session after resume has words from before the pause: %q", text)
	}
}
