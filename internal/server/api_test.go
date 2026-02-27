package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	audioPath := filepath.Join(root, "audio.mp3")
	if err := os.WriteFile(audioPath, []byte(strings.Repeat("a", 4096)), 0o644); err != nil {
		t.Fatalf("write audio file failed: %v", err)
	}

	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", AudioPath: audioPath},
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
