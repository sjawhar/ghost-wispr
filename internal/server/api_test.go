package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type apiStoreStub struct {
	sessionsByDate map[string][]storage.Session
	sessions       map[string]storage.Session
	segments       map[string][]transcribe.Segment
	dates          []string
}

func newTestConfigStore(t *testing.T) *config.Store {
	t.Helper()
	for _, key := range []string{
		"DB_PATH", "AUDIO_DIR", "SILENCE_TIMEOUT",
		"MIC_SAMPLE_RATE", "MIC_SAMPLE_RATES",
		"SUMMARIZATION_MODEL", "GDRIVE_FOLDER_ID", "GOOGLE_CREDENTIALS_FILE",
		"DEEPGRAM_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "CONFIG",
	} {
		t.Setenv(config.EnvPrefix+key, "")
	}
	store, _, err := config.NewStore("")
	if err != nil {
		t.Fatalf("newTestConfigStore: %v", err)
	}
	return store
}

func (s apiStoreStub) GetSessionsByDate(date string, includeDiscarded bool) ([]storage.Session, error) {
	return s.sessionsByDate[date], nil
}

func (s apiStoreStub) GetSession(id string) (storage.Session, error) {
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return storage.Session{}, os.ErrNotExist
}

func (s apiStoreStub) GetSegments(sessionID string) ([]transcribe.Segment, error) {
	return s.segments[sessionID], nil
}

func (s apiStoreStub) GetDates() ([]string, error) {
	return s.dates, nil
}

func (s apiStoreStub) UpdateTitle(sessionID, title string) error {
	sess, ok := s.sessions[sessionID]
	if !ok {
		return os.ErrNotExist
	}
	sess.Title = title
	s.sessions[sessionID] = sess
	return nil
}

func (s apiStoreStub) DeleteSession(id string) error {
	if _, ok := s.sessions[id]; !ok {
		return os.ErrNotExist
	}
	delete(s.sessions, id)
	return nil
}

func (s apiStoreStub) MergeSessions(newID string, sourceIDs []string, startedAt, endedAt time.Time) error {
	endedAtPtr := &endedAt
	s.sessions[newID] = storage.Session{
		ID:            newID,
		StartedAt:     startedAt,
		EndedAt:       endedAtPtr,
		Status:        storage.SessionEnded,
		SummaryStatus: storage.SummaryPending,
	}

	for _, id := range sourceIDs {
		sess := s.sessions[id]
		sess.Status = storage.SessionMerged
		sess.MergedInto = newID
		s.sessions[id] = sess
	}

	return nil
}

func testStaticFS(t *testing.T) fs.FS {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatalf("write index.html failed: %v", err)
	}
	return os.DirFS(dir)
}

func TestAPISessionsList(t *testing.T) {
	started := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{
			"2026-02-26": {{ID: "s1", StartedAt: started, SummaryStatus: storage.SummaryCompleted}},
		},
		sessions: map[string]storage.Session{},
		segments: map[string][]transcribe.Segment{},
		dates:    []string{"2026-02-26"},
	}

	hub := NewHub()
	h, err := Handler(testStaticFS(t), hub, store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions?date=2026-02-26", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content-type, got %q", got)
	}
	if !strings.Contains(rr.Body.String(), "s1") {
		t.Fatalf("expected body to contain session id, got %s", rr.Body.String())
	}
}

func TestAPISessionDetail(t *testing.T) {
	started := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Summary: "hello", SummaryStatus: storage.SummaryCompleted},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {{Speaker: 0, Text: "line", StartTime: 0, EndTime: 1, Timestamp: started}},
		},
		dates: []string{"2026-02-26"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "segments") {
		t.Fatalf("expected detail response to contain segments, got %s", rr.Body.String())
	}
}

func TestDeleteSession_Success(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1"},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/does-not-exist", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDeleteSession_InvalidID(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/%2e%2e", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMergeSession_Success(t *testing.T) {
	start1 := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	end1 := start1.Add(10 * time.Minute)
	start2 := time.Date(2026, 2, 26, 11, 0, 0, 0, time.UTC)
	end2 := start2.Add(20 * time.Minute)

	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: start1, EndedAt: &end1, Status: storage.SessionEnded},
			"s2": {ID: "s2", StartedAt: start2, EndedAt: &end2, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/merge", strings.NewReader(`{"session_ids":["s1","s2"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var merged storage.Session
	if err := json.NewDecoder(rr.Body).Decode(&merged); err != nil {
		t.Fatalf("decode merged session failed: %v", err)
	}

	if merged.ID != "20260226100000-merged" {
		t.Fatalf("expected merged session id 20260226100000-merged, got %q", merged.ID)
	}
	if merged.Status != storage.SessionEnded {
		t.Fatalf("expected merged status ended, got %q", merged.Status)
	}
	if merged.SummaryStatus != storage.SummaryPending {
		t.Fatalf("expected summary status pending, got %q", merged.SummaryStatus)
	}
}

func TestMergeSession_TooFewSessions(t *testing.T) {
	start := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)

	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: start, EndedAt: &end, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/merge", strings.NewReader(`{"session_ids":["s1"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMergeSession_SessionNotFound(t *testing.T) {
	start := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)

	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: start, EndedAt: &end, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/merge", strings.NewReader(`{"session_ids":["s1","does-not-exist"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPIAudioRange(t *testing.T) {
	root := t.TempDir()
	audioFile := "audio.mp3"
	if err := os.WriteFile(filepath.Join(root, audioFile), []byte(strings.Repeat("a", 4096)), 0o644); err != nil {
		t.Fatalf("write audio file failed: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: audioFile},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    []string{"2026-02-26"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/audio", nil)
	req.Header.Set("Range", "bytes=0-1023")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("expected status 206, got %d", rr.Code)
	}
	if rr.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("expected Accept-Ranges bytes, got %q", rr.Header().Get("Accept-Ranges"))
	}
	if rr.Header().Get("Content-Range") == "" {
		t.Fatalf("expected Content-Range header")
	}
}

func TestAPIDates(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          []string{"2026-02-26", "2026-02-25"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/dates", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "2026-02-26") {
		t.Fatalf("expected date in response, got %s", rr.Body.String())
	}
}

func TestAPIAudioPathTraversalBlocked(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/%2e%2e%2f%2e%2e%2fetc%2fpasswd/audio", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("expected forbidden/notfound for traversal, got %d body=%s", rr.Code, string(body))
	}
}

func TestAPIStatusWithWarnings(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		IsPaused: func() bool { return false },
		Warnings: func() []string {
			return []string{"Deepgram API key not configured"}
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"paused":false`) {
		t.Fatalf("expected paused:false in response, got %s", body)
	}
	if !strings.Contains(body, `"warnings"`) {
		t.Fatalf("expected warnings field in response, got %s", body)
	}
	if !strings.Contains(body, "Deepgram API key not configured") {
		t.Fatalf("expected warning message in response, got %s", body)
	}
	if !strings.Contains(body, `"active_session_id":""`) {
		t.Fatalf("expected empty active_session_id in response, got %s", body)
	}
	if !strings.Contains(body, `"active_session_started_at":""`) {
		t.Fatalf("expected empty active_session_started_at in response, got %s", body)
	}
}

func TestAPIStatusNoWarnings(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"warnings":[]`) {
		t.Fatalf("expected empty warnings array in response, got %s", body)
	}
	if !strings.Contains(body, `"active_session_id":""`) {
		t.Fatalf("expected empty active_session_id in response, got %s", body)
	}
	if !strings.Contains(body, `"active_session_started_at":""`) {
		t.Fatalf("expected empty active_session_started_at in response, got %s", body)
	}
}

func TestAPIStatusWithActiveSession(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	startedAt := time.Date(2026, 3, 22, 18, 4, 5, 123456789, time.UTC)
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		ActiveSession: func() (string, time.Time) {
			return "session-123", startedAt
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"active_session_id":"session-123"`) {
		t.Fatalf("expected active session id in response, got %s", body)
	}
	if !strings.Contains(body, `"active_session_started_at":"2026-03-22T18:04:05.123456789Z"`) {
		t.Fatalf("expected active session start timestamp in response, got %s", body)
	}
}

func TestAPIStatusWithNoActiveSession(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		ActiveSession: func() (string, time.Time) {
			return "", time.Time{}
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"active_session_id":""`) {
		t.Fatalf("expected empty active_session_id in response, got %s", body)
	}
	if !strings.Contains(body, `"active_session_started_at":""`) {
		t.Fatalf("expected empty active_session_started_at in response, got %s", body)
	}
}

func TestGetPresets(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Presets: func() map[string]config.Preset {
			return map[string]config.Preset{
				"brief": {
					Description:  "Short summary",
					SystemPrompt: "ignore",
				},
				"detailed": {
					Description:  "Long summary",
					SystemPrompt: "ignore",
				},
			}
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/presets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if got["brief"] != "Short summary" {
		t.Fatalf("expected brief preset description, got %q", got["brief"])
	}
	if got["detailed"] != "Long summary" {
		t.Fatalf("expected detailed preset description, got %q", got["detailed"])
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 presets, got %d", len(got))
	}
}

func TestGetPresetsEmpty(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/presets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty presets object, got %v", got)
	}
}

func TestResummarize(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	type resummarizeCall struct {
		sessionID string
		preset    string
	}

	called := make(chan resummarizeCall, 1)
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			called <- resummarizeCall{sessionID: sessionID, preset: preset}
			return nil
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test123/resummarize", strings.NewReader(`{"preset":"detailed"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", rr.Code)
	}

	select {
	case got := <-called:
		if got.sessionID != "test123" {
			t.Fatalf("expected sessionID test123, got %q", got.sessionID)
		}
		if got.preset != "detailed" {
			t.Fatalf("expected preset detailed, got %q", got.preset)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected resummarize to be called")
	}
}

func TestResummarizeNotConfigured(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test123/resummarize", strings.NewReader(`{"preset":"detailed"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rr.Code)
	}
}

func TestResummarizeInvalidSessionID(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			return nil
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/%2e%2e/resummarize", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", rr.Code)
	}
}

func TestAPI_Resummarize_InvalidJSONReturns400(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	called := make(chan struct{}, 1)
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			called <- struct{}{}
			return nil
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test123/resummarize", strings.NewReader(`{invalid json`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-called:
		t.Fatal("resummarize should not be called for invalid JSON")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAPI_Resummarize_ValidRequestStill202(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	called := make(chan struct{}, 1)
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			called <- struct{}{}
			return nil
		},
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test123/resummarize", strings.NewReader(`{"preset":"brief"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d body=%s", rr.Code, rr.Body.String())
	}

	select {
	case <-called:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected resummarize to be called")
	}
}

func TestAPI_SessionAudio_RejectsAbsolutePath(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: "/etc/passwd"},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/audio", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 for absolute path, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEndSession_Success(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		EndSession: func(_ context.Context) error { return nil },
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/session/end", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEndSession_NoActiveSession(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		EndSession: func(_ context.Context) error { return session.ErrNoActiveSession },
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/session/end", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEndSession_InternalError(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		EndSession: func(_ context.Context) error { return errors.New("db exploded") },
	}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/session/end", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEndSession_NotConfigured(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "")
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/session/end", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPI_GeneratePreset_MissingDescription(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		GeneratePreset: func(ctx context.Context, description string) (config.Preset, error) {
			return config.Preset{}, nil
		},
	}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPI_GeneratePreset_Success(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		GeneratePreset: func(ctx context.Context, description string) (config.Preset, error) {
			if description != "standup summary" {
				return config.Preset{}, errors.New("wrong description")
			}
			return config.Preset{
				Description:  description,
				SystemPrompt: "Summarize as bullet points",
				UserTemplate: "{{transcript}}",
			}, nil
		},
	}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"description": "standup summary"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp config.Preset
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.SystemPrompt != "Summarize as bullet points" {
		t.Fatalf("expected system prompt, got %q", resp.SystemPrompt)
	}
}

func TestAPI_RefinePreset_MissingFields(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		RefinePreset: func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error) {
			return config.Preset{}, nil
		},
	}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	// Missing name
	body := `{"feedback": "make it shorter"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/refine", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d", rr.Code)
	}

	// Missing feedback
	body2 := `{"name": "default"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/config/presets/refine", strings.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing feedback, got %d", rr2.Code)
	}
}

func TestAPI_RefinePreset_UnknownPreset(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		RefinePreset: func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error) {
			return config.Preset{}, nil
		},
	}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"name": "nonexistent", "feedback": "make it shorter"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/refine", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPI_RefinePreset_Success(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		RefinePreset: func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error) {
			return config.Preset{
				Description:  "Refined: " + current.Description,
				SystemPrompt: "Refined prompt",
				UserTemplate: "{{transcript}}",
			}, nil
		},
	}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"name": "default", "feedback": "make it shorter"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/presets/refine", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp config.Preset
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.SystemPrompt != "Refined prompt" {
		t.Fatalf("expected refined prompt, got %q", resp.SystemPrompt)
	}
}

func TestAPI_GetConfig_ExposesGDriveSyncAndGCDefaults(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		GDrive struct {
			SyncEnabled bool `json:"sync_enabled"`
		} `json:"gdrive"`
		GC struct {
			Enabled        bool `json:"enabled"`
			MaxAgeDays     int  `json:"max_age_days"`
			MaxAudioSizeMB int  `json:"max_audio_size_mb"`
		} `json:"gc"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if resp.GDrive.SyncEnabled {
		t.Fatalf("expected gdrive.sync_enabled default false")
	}
	if resp.GC.Enabled {
		t.Fatalf("expected gc.enabled default false")
	}
	if resp.GC.MaxAgeDays != 30 {
		t.Fatalf("expected gc.max_age_days default 30, got %d", resp.GC.MaxAgeDays)
	}
	if resp.GC.MaxAudioSizeMB != 1024 {
		t.Fatalf("expected gc.max_audio_size_mb default 1024, got %d", resp.GC.MaxAudioSizeMB)
	}
}

func TestAPI_PatchConfig_UpdatesGDriveSyncAndGC(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"gdrive":{"sync_enabled":true},"gc":{"enabled":true,"max_age_days":7,"max_audio_size_mb":256}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		GDrive struct {
			SyncEnabled bool `json:"sync_enabled"`
		} `json:"gdrive"`
		GC struct {
			Enabled        bool `json:"enabled"`
			MaxAgeDays     int  `json:"max_age_days"`
			MaxAudioSizeMB int  `json:"max_audio_size_mb"`
		} `json:"gc"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if !resp.GDrive.SyncEnabled {
		t.Fatalf("expected gdrive.sync_enabled true after patch")
	}
	if !resp.GC.Enabled {
		t.Fatalf("expected gc.enabled true after patch")
	}
	if resp.GC.MaxAgeDays != 7 {
		t.Fatalf("expected gc.max_age_days 7, got %d", resp.GC.MaxAgeDays)
	}
	if resp.GC.MaxAudioSizeMB != 256 {
		t.Fatalf("expected gc.max_audio_size_mb 256, got %d", resp.GC.MaxAudioSizeMB)
	}
}
