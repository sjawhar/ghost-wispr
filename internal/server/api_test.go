package server

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type apiStoreStub struct {
	sessionsByDate map[string][]storage.Session
	sessions       map[string]storage.Session
	segments       map[string][]transcribe.Segment
	dates          []string
}

func (s apiStoreStub) GetSessionsByDate(date string) ([]storage.Session, error) {
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
	h, err := Handler(testStaticFS(t), hub, store, ControlHooks{})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{
		IsPaused: func() bool { return false },
		Warnings: func() []string {
			return []string{"Deepgram API key not configured"}
		},
	})
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
}

func TestAPIStatusNoWarnings(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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
}

func TestGetPresets(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{
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
	})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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
	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			called <- resummarizeCall{sessionID: sessionID, preset: preset}
			return nil
		},
	})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			return nil
		},
	})
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
	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			called <- struct{}{}
			return nil
		},
	})
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
	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			called <- struct{}{}
			return nil
		},
	})
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

	h, err := Handler(testStaticFS(t), NewHub(), store, ControlHooks{})
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
