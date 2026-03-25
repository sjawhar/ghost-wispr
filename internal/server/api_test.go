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
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type apiStoreStub struct {
	sessionsByDate      map[string][]storage.Session
	sessions            map[string]storage.Session
	segments            map[string][]transcribe.Segment
	dates               []string
	searchResults       map[string][]storage.SearchResult
	searchErr           error
	aggregateResult     *storage.AggregateResult
	aggregateErr        error
	allEmbeddings       []storage.StoredEmbedding
	allEmbeddingsErr    error
	getEventsSinceFunc  func(cursor int64, limit int) ([]storage.StoredEvent, error)
}

type embeddingClientStub struct {
	vectors [][]float32
	err     error
}

func (s embeddingClientStub) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.vectors, nil
}

var _ embedding.Client = embeddingClientStub{}

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

func (s apiStoreStub) GetSegmentsInTimeRange(sessionID string, startTime, endTime float64) ([]transcribe.Segment, error) {
	all := s.segments[sessionID]
	var result []transcribe.Segment
	for _, seg := range all {
		if seg.StartTime >= startTime && seg.EndTime <= endTime {
			result = append(result, seg)
		}
	}
	return result, nil
}

func (s apiStoreStub) GetDates() ([]string, error) {
	return s.dates, nil
}

func (s apiStoreStub) Search(query string, opts storage.SearchOptions) ([]storage.SearchResult, error) {
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if s.searchResults == nil {
		return []storage.SearchResult{}, nil
	}
	return s.searchResults[query], nil
}

func (s apiStoreStub) AggregateSessions(opts storage.AggregateOptions) (storage.AggregateResult, error) {
	if s.aggregateErr != nil {
		return storage.AggregateResult{}, s.aggregateErr
	}
	if s.aggregateResult != nil {
		return *s.aggregateResult, nil
	}
	return storage.AggregateResult{Groups: []storage.AggregateGroup{}}, nil
}

func (s apiStoreStub) GetAllEmbeddings() ([]storage.StoredEmbedding, error) {
	if s.allEmbeddingsErr != nil {
		return nil, s.allEmbeddingsErr
	}
	if s.allEmbeddings == nil {
		return []storage.StoredEmbedding{}, nil
	}
	return s.allEmbeddings, nil
}

func (s apiStoreStub) GetEventsSince(cursor int64, limit int) ([]storage.StoredEvent, error) {
	if s.getEventsSinceFunc != nil {
		return s.getEventsSinceFunc(cursor, limit)
	}
	return []storage.StoredEvent{}, nil
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

type healthCheckerStub struct{}

func (h *healthCheckerStub) IsDeepgramConnected() bool            { return true }
func (h *healthCheckerStub) IsDBHealthy(ctx context.Context) bool { return true }
func (h *healthCheckerStub) IsMicOpen() bool                      { return true }
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
	h, err := Handler(testStaticFS(t), hub, store, &ControlHooks{}, "", nil)
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

func TestAPISearchReturnsResults(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
		searchResults: map[string][]storage.SearchResult{
			"alpha": {
				{SessionID: "s1", Title: "Alpha planning", Snippet: "<mark>alpha</mark> snippet", Rank: -1.25},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=alpha", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(rr.Body).Decode(&results); err != nil {
		t.Fatalf("decode search results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}
	if results[0].SessionID != "s1" || !strings.Contains(results[0].Snippet, "<mark>") {
		t.Fatalf("unexpected search result payload: %+v", results[0])
	}
}

func TestAPISearchEmptyQueryReturnsEmptyArray(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=%20%20%20", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(rr.Body).Decode(&results); err != nil {
		t.Fatalf("decode search results: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty search results, got %+v", results)
	}
}

func TestSpeakerFilter_ByName(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Title: "Meeting", SpeakerNames: `{"0": {"name": "Ben", "confidence": "mentioned"}, "1": {"name": "Alice", "confidence": "mentioned"}}`},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "budget discussion", StartTime: 0, EndTime: 1},
				{Speaker: 1, Text: "timeline", StartTime: 1, EndTime: 2},
			},
		},
		searchResults: map[string][]storage.SearchResult{
			"budget": {
				{SessionID: "s1", Title: "Meeting", Snippet: "<mark>budget</mark> discussion", Rank: -1.0},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=budget&speaker=Ben", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(rr.Body).Decode(&results); err != nil {
		t.Fatalf("decode search results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for speaker Ben, got %d", len(results))
	}
}

func TestSpeakerFilter_ByIndex(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Title: "Meeting", SpeakerNames: `{"0": {"name": "Ben", "confidence": "mentioned"}, "1": {"name": "Alice", "confidence": "mentioned"}}`},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "budget discussion", StartTime: 0, EndTime: 1},
				{Speaker: 1, Text: "timeline", StartTime: 1, EndTime: 2},
			},
		},
		searchResults: map[string][]storage.SearchResult{
			"budget": {
				{SessionID: "s1", Title: "Meeting", Snippet: "<mark>budget</mark> discussion", Rank: -1.0},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=budget&speaker=0", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(rr.Body).Decode(&results); err != nil {
		t.Fatalf("decode search results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for speaker 0, got %d", len(results))
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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
	}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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
	}, "", nil)
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
	}, "", nil)
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
	}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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
	}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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
	}, "", nil)
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
	}, "", nil)
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
	}, "", nil)
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

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
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
	}, "", nil)
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
	}, "", nil)
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
	}, "", nil)
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
	}, &ControlHooks{}, "", nil)
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
	}, "", nil, cfgStore)
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
	}, "", nil, cfgStore)
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
	}, "", nil, cfgStore)
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
	}, "", nil, cfgStore)
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
	}, "", nil, cfgStore)
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
	}, &ControlHooks{}, "", nil, cfgStore)
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
	}, &ControlHooks{}, "", nil, cfgStore)
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

func TestFaultInjectionDisabledWithoutTestMode(t *testing.T) {
	store := apiStoreStub{
		sessions: map[string]storage.Session{},
		segments: map[string][]transcribe.Segment{},
	}

	closed := false
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		FaultDeepgramDisconnect: func() error {
			closed = true
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/test/fault/deepgram-disconnect", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when test mode not enabled, got %d", rr.Code)
	}
	if closed {
		t.Fatal("expected fault NOT to be triggered without test mode")
	}
}

func TestFaultInjectionEnabledWithTestMode(t *testing.T) {
	t.Setenv("GHOST_WISPR_TEST_MODE", "true")

	store := apiStoreStub{
		sessions: map[string]storage.Session{},
		segments: map[string][]transcribe.Segment{},
	}

	closed := false
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		FaultDeepgramDisconnect: func() error {
			closed = true
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/test/fault/deepgram-disconnect", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with test mode, got %d", rr.Code)
	}

	var result map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["triggered"] != true {
		t.Fatalf("expected triggered=true, got %v", result["triggered"])
	}
	if !closed {
		t.Fatal("expected fault to be triggered")
	}
}

func TestFaultInjectionNoHandler(t *testing.T) {
	t.Setenv("GHOST_WISPR_TEST_MODE", "true")

	store := apiStoreStub{
		sessions: map[string]storage.Session{},
		segments: map[string][]transcribe.Segment{},
	}

	// No FaultDeepgramDisconnect set
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/test/fault/deepgram-disconnect", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no handler configured, got %d", rr.Code)
	}
}

// --- Manual Session Trigger Tests ---

func TestManualSessionStart_Success(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StartSession: func(_ context.Context, titleHint string) (string, error) {
			if titleHint != "My Meeting" {
				return "", errors.New("unexpected title hint")
			}
			return "20260323120000", nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/start", strings.NewReader(`{"title_hint":"My Meeting"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["session_id"] != "20260323120000" {
		t.Fatalf("expected session_id 20260323120000, got %q", resp["session_id"])
	}
}

func TestManualSessionStart_NoBody(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StartSession: func(_ context.Context, titleHint string) (string, error) {
			if titleHint != "" {
				return "", errors.New("expected empty title hint")
			}
			return "20260323120000", nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestManualSessionStart_AlreadyActive(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StartSession: func(_ context.Context, titleHint string) (string, error) {
			return "", session.ErrSessionAlreadyActive
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestManualSessionStart_NotConfigured(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestManualSessionStop_ByID_Success(t *testing.T) {
	stopped := ""
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StopSession: func(_ context.Context, sessionID string) error {
			stopped = sessionID
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test-session/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if stopped != "test-session" {
		t.Fatalf("expected stopped session test-session, got %q", stopped)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "stopped" {
		t.Fatalf("expected status stopped, got %q", resp["status"])
	}
}

func TestManualSessionStop_ByID_NotFound(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StopSession: func(_ context.Context, sessionID string) error {
			return session.ErrNoActiveSession
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/nonexistent/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestManualSessionStop_ByID_InvalidID(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StopSession: func(_ context.Context, sessionID string) error { return nil },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/%2e%2e/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestManualSessionStop_Current_Success(t *testing.T) {
	called := false
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		EndSession: func(_ context.Context) error {
			called = true
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/current/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("expected EndSession to be called")
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "stopped" {
		t.Fatalf("expected status stopped, got %q", resp["status"])
	}
}

func TestManualSessionStop_Current_NoActive(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		EndSession: func(_ context.Context) error { return session.ErrNoActiveSession },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/current/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestManualSessionStop_NotConfigured(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/test-session/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPISessionDetail_IncludesTranscriptSource(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {
				ID:                  "s1",
				StartedAt:           started,
				Summary:             "test",
				SummaryStatus:       storage.SummaryCompleted,
				TranscriptSource:    "refined",
				CanonicalTranscript: "refined transcript content",
			},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {{Speaker: 0, Text: "line", StartTime: 0, EndTime: 1, Timestamp: started}},
		},
		dates: []string{"2026-03-23"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Session struct {
			TranscriptSource    string `json:"transcript_source"`
			CanonicalTranscript string `json:"canonical_transcript"`
		} `json:"session"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Session.TranscriptSource != "refined" {
		t.Fatalf("expected transcript_source 'refined' in API response, got %q", resp.Session.TranscriptSource)
	}
	if resp.Session.CanonicalTranscript != "refined transcript content" {
		t.Fatalf("expected canonical_transcript in API response, got %q", resp.Session.CanonicalTranscript)
	}
}

func TestSearchEndpointWithFilters(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{
		searchResults: map[string][]storage.SearchResult{
			"test": {
				{SessionID: "sess-1", Title: "Meeting 1", Snippet: "test content", Rank: 0.5},
				{SessionID: "sess-2", Title: "Meeting 2", Snippet: "test content", Rank: 0.3},
			},
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	// Test with date_from filter
	req := httptest.NewRequest("GET", "/api/search?q=test&date_from=2026-03-25T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(w.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestSearchEndpointWithPresetFilter(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{
		searchResults: map[string][]storage.SearchResult{
			"test": {
				{SessionID: "sess-1", Title: "Meeting", Snippet: "test content", Rank: 0.5},
			},
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	// Test with preset filter
	req := httptest.NewRequest("GET", "/api/search?q=test&preset=meeting", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(w.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestSearchEndpointBackwardCompatible(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{
		searchResults: map[string][]storage.SearchResult{
			"test": {
				{SessionID: "sess-1", Title: "Test", Snippet: "test content", Rank: 0.5},
			},
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	// Test backward compatibility: ?q=term without filters should work
	req := httptest.NewRequest("GET", "/api/search?q=test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(w.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestSemanticSearch_Success(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{
		sessions: map[string]storage.Session{
			"sess-top": {ID: "sess-top", Title: "Top Session", StartedAt: time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)},
			"sess-low": {ID: "sess-low", Title: "Lower Session", StartedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)},
		},
		allEmbeddings: []storage.StoredEmbedding{
			{SessionID: "sess-top", ChunkIndex: 0, Vector: []float32{1, 0}},
			{SessionID: "sess-low", ChunkIndex: 0, Vector: []float32{0, 1}},
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, embeddingClientStub{vectors: [][]float32{{1, 0}}})

	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=planning&limit=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var payload struct {
		Results []struct {
			SessionID  string  `json:"session_id"`
			Title      string  `json:"title"`
			ChunkText  string  `json:"chunk_text"`
			Similarity float32 `json:"similarity"`
			ChunkIndex int     `json:"chunk_index"`
		} `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(payload.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(payload.Results))
	}
	if payload.Results[0].SessionID != "sess-top" {
		t.Fatalf("expected top session to be sess-top, got %q", payload.Results[0].SessionID)
	}
	if payload.Results[0].Title != "Top Session" {
		t.Fatalf("expected title Top Session, got %q", payload.Results[0].Title)
	}
}

func TestSemanticSearch_NoClient(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), apiStoreStub{}, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=planning", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected status 501, got %d body=%s", rr.Code, rr.Body.String())
	}

	var payload map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["error"] != "semantic search unavailable" {
		t.Fatalf("expected semantic unavailable error, got %q", payload["error"])
	}
	if payload["suggestion"] != "use /api/search for keyword search" {
		t.Fatalf("expected keyword search suggestion, got %q", payload["suggestion"])
	}
}

func TestSemanticSearch_NoEmbeddings(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, embeddingClientStub{vectors: [][]float32{{1, 0}}})

	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=planning", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected status 422, got %d body=%s", w.Code, w.Body.String())
	}

	var payload map[string]string
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["error"] != "no embeddings indexed yet" {
		t.Fatalf("expected no embeddings error, got %q", payload["error"])
	}
}

func TestContextEndpoint_Success(t *testing.T) {
	started := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "early talk", StartTime: 10.0, EndTime: 15.0, Timestamp: started},
				{Speaker: 1, Text: "the budget discussion", StartTime: 60.0, EndTime: 65.0, Timestamp: started},
				{Speaker: 0, Text: "follow up items", StartTime: 120.0, EndTime: 125.0, Timestamp: started},
				{Speaker: 1, Text: "unrelated later", StartTime: 600.0, EndTime: 605.0, Timestamp: started},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/context?q=budget&seconds=200", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		SessionID     string               `json:"session_id"`
		Query         string               `json:"query"`
		MatchTime     float64              `json:"match_time"`
		Segments      []transcribe.Segment `json:"segments"`
		WindowSeconds float64              `json:"window_seconds"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.SessionID != "s1" {
		t.Fatalf("expected session_id s1, got %q", resp.SessionID)
	}
	if resp.Query != "budget" {
		t.Fatalf("expected query 'budget', got %q", resp.Query)
	}
	if resp.MatchTime != 60.0 {
		t.Fatalf("expected match_time 60.0, got %f", resp.MatchTime)
	}
	if resp.WindowSeconds != 200.0 {
		t.Fatalf("expected window_seconds 200.0, got %f", resp.WindowSeconds)
	}
	// Window: [60-100, 60+100] = [-40, 160] — segments at 10-15, 60-65, 120-125 should match
	if len(resp.Segments) != 3 {
		t.Fatalf("expected 3 segments in context window, got %d", len(resp.Segments))
	}
}

func TestContextEndpoint_DefaultSeconds(t *testing.T) {
	started := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "keyword here", StartTime: 100.0, EndTime: 105.0, Timestamp: started},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/context?q=keyword", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		WindowSeconds float64 `json:"window_seconds"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.WindowSeconds != 300.0 {
		t.Fatalf("expected default window_seconds 300.0, got %f", resp.WindowSeconds)
	}
}

func TestContextEndpoint_SessionNotFound(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/nonexistent/context?q=test", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContextEndpoint_NoMatch(t *testing.T) {
	started := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "nothing relevant", StartTime: 10.0, EndTime: 15.0, Timestamp: started},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/context?q=nonexistent", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected status 422, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContextEndpoint_MissingQuery(t *testing.T) {
	started := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/context", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAggregateEndpoint_Success(t *testing.T) {
	started1 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	started2 := time.Date(2026, 3, 25, 14, 0, 0, 0, time.UTC)
	started3 := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC)

	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		aggregateResult: &storage.AggregateResult{
			SessionCount: 3,
			Groups: []storage.AggregateGroup{
				{
					Key:   "2026-03-26",
					Count: 1,
					Sessions: []storage.SessionSummary{
						{ID: "s3", Title: "Standup", StartedAt: started3, SummaryPreset: "standup"},
					},
				},
				{
					Key:   "2026-03-25",
					Count: 2,
					Sessions: []storage.SessionSummary{
						{ID: "s1", Title: "Meeting", StartedAt: started1, SummaryPreset: "meeting"},
						{ID: "s2", Title: "Review", StartedAt: started2, SummaryPreset: "meeting"},
					},
				},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/aggregate", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var result storage.AggregateResult
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.SessionCount != 3 {
		t.Fatalf("expected session_count 3, got %d", result.SessionCount)
	}
	if len(result.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(result.Groups))
	}
	if result.Groups[0].Key != "2026-03-26" {
		t.Fatalf("expected first group key 2026-03-26, got %q", result.Groups[0].Key)
	}
	if result.Groups[1].Count != 2 {
		t.Fatalf("expected second group count 2, got %d", result.Groups[1].Count)
	}
	if result.Groups[1].Sessions[0].ID != "s1" {
		t.Fatalf("expected first session ID s1, got %q", result.Groups[1].Sessions[0].ID)
	}
}

func TestAggregateEndpoint_WithQueryParams(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		aggregateResult: &storage.AggregateResult{
			SessionCount: 1,
			Groups: []storage.AggregateGroup{
				{Key: "meeting", Count: 1, Sessions: []storage.SessionSummary{{ID: "s1", Title: "Meeting", SummaryPreset: "meeting"}}},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/aggregate?group_by=preset&preset=meeting&date_from=2026-03-20T00:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var result storage.AggregateResult
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.SessionCount != 1 {
		t.Fatalf("expected session_count 1, got %d", result.SessionCount)
	}
}

func TestAggregateEndpoint_InvalidGroupBy(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/aggregate?group_by=invalid", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOpenAPISpec(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}

	var spec map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&spec); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Verify OpenAPI 3.1 structure
	if openapi, ok := spec["openapi"]; !ok || openapi != "3.1.0" {
		t.Fatalf("expected openapi 3.1.0, got %v", openapi)
	}

	if _, ok := spec["info"]; !ok {
		t.Fatalf("expected info field")
	}

	if _, ok := spec["paths"]; !ok {
		t.Fatalf("expected paths field")
	}

	paths := spec["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatalf("expected paths to have entries")
	}

	// Verify security scheme
	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("expected components field")
	}
	securitySchemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("expected securitySchemes field")
	}
	if _, ok := securitySchemes["basicAuth"]; !ok {
		t.Fatalf("expected basicAuth security scheme")
	}

	// Verify some expected paths exist
	expectedPaths := []string{
		"/healthz/live",
		"/api/version",
		"/api/search",
		"/api/search/semantic",
		"/api/events",
		"/api/sessions",
		"/api/sessions/aggregate",
		"/api/sessions/{id}",
		"/api/sessions/{id}/segments",
		"/api/sessions/{id}/context",
	}
	for _, path := range expectedPaths {
		if _, ok := paths[path]; !ok {
			t.Errorf("expected path %q in spec", path)
		}
	}
}

func TestSegmentsSpeakerFilter(t *testing.T) {
	started := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	store := apiStoreStub{
		sessions: map[string]storage.Session{
			"s1": {
				ID:           "s1",
				Title:        "Meeting",
				StartedAt:    started,
				SpeakerNames: `{"0": {"name": "Ben", "confidence": "mentioned"}, "1": {"name": "Alice", "confidence": "mentioned"}}`,
			},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "budget discussion", StartTime: 0, EndTime: 1, Timestamp: started},
				{Speaker: 1, Text: "timeline", StartTime: 1, EndTime: 2, Timestamp: started},
				{Speaker: 0, Text: "next steps", StartTime: 2, EndTime: 3, Timestamp: started},
			},
		},
	}

	mux := http.NewServeMux()
	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	// Test filtering by speaker name
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/segments?speaker=Ben", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var segments []transcribe.Segment
	if err := json.NewDecoder(w.Body).Decode(&segments); err != nil {
		t.Fatalf("decode segments: %v", err)
	}

	if len(segments) != 2 {
		t.Fatalf("expected 2 segments for speaker Ben, got %d", len(segments))
	}
	for _, seg := range segments {
		if seg.Speaker != 0 {
			t.Fatalf("expected speaker 0, got %d", seg.Speaker)
		}
	}

	// Test filtering by speaker index
	req = httptest.NewRequest(http.MethodGet, "/api/sessions/s1/segments?speaker=1", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	if err := json.NewDecoder(w.Body).Decode(&segments); err != nil {
		t.Fatalf("decode segments: %v", err)
	}

	if len(segments) != 1 {
		t.Fatalf("expected 1 segment for speaker 1, got %d", len(segments))
	}
	if segments[0].Speaker != 1 {
		t.Fatalf("expected speaker 1, got %d", segments[0].Speaker)
	}
}

func TestEventsEndpoint_Success(t *testing.T) {
	store := &apiStoreStub{
		sessions: map[string]storage.Session{},
	}
	
	// Mock GetEventsSince to return some events
	store.getEventsSinceFunc = func(cursor int64, limit int) ([]storage.StoredEvent, error) {
		if cursor == 0 && limit == 51 { // limit+1 for has_more detection
			return []storage.StoredEvent{
				{ID: 1, EventType: "session_started", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 2, EventType: "session_ended", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 3, EventType: "summary_ready", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
			}, nil
		}
		return []storage.StoredEvent{}, nil
	}
	
	mux := http.NewServeMux()
	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)
	
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}
	
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	
	events, ok := resp["events"].([]interface{})
	if !ok {
		t.Fatalf("expected events array in response")
	}
	
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	
	nextCursor, ok := resp["next_cursor"].(float64)
	if !ok || int64(nextCursor) != 3 {
		t.Fatalf("expected next_cursor=3, got %v", nextCursor)
	}
	
	hasMore, ok := resp["has_more"].(bool)
	if !ok || hasMore {
		t.Fatalf("expected has_more=false, got %v", hasMore)
	}
}

func TestEventsEndpoint_TypeFilter(t *testing.T) {
	store := &apiStoreStub{
		sessions: map[string]storage.Session{},
	}
	
	// Mock GetEventsSince to return events with different types
	store.getEventsSinceFunc = func(cursor int64, limit int) ([]storage.StoredEvent, error) {
		if cursor == 0 && limit == 51 { // limit+1 for has_more detection
			return []storage.StoredEvent{
				{ID: 1, EventType: "session_started", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 2, EventType: "session_ended", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 3, EventType: "summary_ready", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
			}, nil
		}
		return []storage.StoredEvent{}, nil
	}
	
	mux := http.NewServeMux()
	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)
	
	req := httptest.NewRequest(http.MethodGet, "/api/events?types=session_ended,summary_ready", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}
	
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	
	events, ok := resp["events"].([]interface{})
	if !ok {
		t.Fatalf("expected events array in response")
	}
	
	if len(events) != 2 {
		t.Fatalf("expected 2 filtered events, got %d", len(events))
	}
}

func TestEventsEndpoint_EmptyQueue(t *testing.T) {
	store := &apiStoreStub{
		sessions: map[string]storage.Session{},
	}
	
	// Mock GetEventsSince to return no events
	store.getEventsSinceFunc = func(cursor int64, limit int) ([]storage.StoredEvent, error) {
		return []storage.StoredEvent{}, nil
	}
	
	mux := http.NewServeMux()
	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)
	
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}
	
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	
	events, ok := resp["events"].([]interface{})
	if !ok {
		t.Fatalf("expected events array in response")
	}
	
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
	
	nextCursor, ok := resp["next_cursor"].(float64)
	if !ok || int64(nextCursor) != 0 {
		t.Fatalf("expected next_cursor=0, got %v", nextCursor)
	}
	
	hasMore, ok := resp["has_more"].(bool)
	if !ok || hasMore {
		t.Fatalf("expected has_more=false, got %v", hasMore)
	}
}

func TestEventsEndpoint_Pagination(t *testing.T) {
	store := &apiStoreStub{
		sessions: map[string]storage.Session{},
	}
	
	// Mock GetEventsSince to simulate pagination
	store.getEventsSinceFunc = func(cursor int64, limit int) ([]storage.StoredEvent, error) {
		if cursor == 0 && limit == 3 { // limit+1 for has_more detection
			// Return 3 events (limit+1) to indicate has_more=true
			return []storage.StoredEvent{
				{ID: 1, EventType: "session_started", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 2, EventType: "session_ended", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 3, EventType: "summary_ready", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
			}, nil
		}
		if cursor == 2 && limit == 3 {
			return []storage.StoredEvent{
				{ID: 3, EventType: "summary_ready", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
				{ID: 4, EventType: "status_changed", Payload: `{"session_id":"s1"}`, CreatedAt: time.Now()},
			}, nil
		}
		return []storage.StoredEvent{}, nil
	}
	
	mux := http.NewServeMux()
	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)
	
	// First page
	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}
	
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	
	events, ok := resp["events"].([]interface{})
	if !ok {
		t.Fatalf("expected events array in response")
	}
	
	if len(events) != 2 {
		t.Fatalf("expected 2 events on first page, got %d", len(events))
	}
	
	nextCursor, ok := resp["next_cursor"].(float64)
	if !ok || int64(nextCursor) != 2 {
		t.Fatalf("expected next_cursor=2, got %v", nextCursor)
	}
	
	hasMore, ok := resp["has_more"].(bool)
	if !ok || !hasMore {
		t.Fatalf("expected has_more=true, got %v", hasMore)
	}
}

// --- Non-Regression Test Suite ---
// Verifies all existing REST API endpoints are unchanged after new additions.

func TestNonRegression(t *testing.T) {
	t.Run("status", testNonRegressionStatus)
	t.Run("dates", testNonRegressionDates)
	t.Run("search", testNonRegressionSearch)
	t.Run("pause", testNonRegressionPause)
	t.Run("resume", testNonRegressionResume)
	t.Run("config", testNonRegressionConfig)
	t.Run("version", testNonRegressionVersion)
	t.Run("presets", testNonRegressionPresets)
}

func testNonRegressionStatus(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		IsPaused: func() bool { return false },
		Warnings: func() []string { return []string{} },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	if !strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected application/json content-type, got %q", rr.Header().Get("Content-Type"))
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if _, ok := resp["paused"]; !ok {
		t.Fatalf("expected 'paused' field in response")
	}
	if _, ok := resp["warnings"]; !ok {
		t.Fatalf("expected 'warnings' field in response")
	}
}

func testNonRegressionDates(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          []string{"2026-03-25", "2026-03-24"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/dates", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var dates []string
	if err := json.NewDecoder(rr.Body).Decode(&dates); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if len(dates) != 2 {
		t.Fatalf("expected 2 dates, got %d", len(dates))
	}
}

func testNonRegressionSearch(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
		searchResults: map[string][]storage.SearchResult{
			"test": {
				{SessionID: "s1", Title: "Test Session", Snippet: "<mark>test</mark> content", Rank: -1.0},
			},
		},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=test", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var results []storage.SearchResult
	if err := json.NewDecoder(rr.Body).Decode(&results); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].SessionID != "s1" {
		t.Fatalf("expected session s1, got %s", results[0].SessionID)
	}
}

func testNonRegressionPause(t *testing.T) {
	called := false
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Pause: func() {
			called = true
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/pause", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", rr.Code)
	}
	if !called {
		t.Fatalf("expected Pause to be called")
	}
}

func testNonRegressionResume(t *testing.T) {
	called := false
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Resume: func() {
			called = true
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/resume", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", rr.Code)
	}
	if !called {
		t.Fatalf("expected Resume to be called")
	}
}

func testNonRegressionConfig(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if _, ok := resp["silence_timeout"]; !ok {
		t.Fatalf("expected 'silence_timeout' field in response")
	}
	if _, ok := resp["summarization"]; !ok {
		t.Fatalf("expected 'summarization' field in response")
	}
}

func testNonRegressionVersion(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if _, ok := resp["version"]; !ok {
		t.Fatalf("expected 'version' field in response")
	}
}

func testNonRegressionPresets(t *testing.T) {
	store := apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Presets: func() map[string]config.Preset {
			return map[string]config.Preset{
				"default": {
					Description:  "Default summary",
					SystemPrompt: "Summarize",
				},
			}
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/presets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}

	if len(resp) != 1 {
		t.Fatalf("expected 1 preset, got %d", len(resp))
	}
	if resp["default"] != "Default summary" {
		t.Fatalf("expected 'Default summary' description, got %q", resp["default"])
	}
}
