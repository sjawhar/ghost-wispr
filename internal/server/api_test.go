package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/tts"
)

type apiStoreStub struct {
	sessionsByDate     map[string][]storage.Session
	sessions           map[string]storage.Session
	segments           map[string][]transcribe.Segment
	dates              []string
	searchResults      map[string][]storage.SearchResult
	searchErr          error
	searchFunc         func(query string, opts storage.SearchOptions) ([]storage.SearchResult, error)
	aggregateResult    *storage.AggregateResult
	aggregateErr       error
	aggregateFunc      func(opts storage.AggregateOptions) (storage.AggregateResult, error)
	allEmbeddings      []storage.StoredEmbedding
	allEmbeddingsErr   error
	getEventsSinceFunc func(cursor int64, limit int) ([]storage.StoredEvent, error)
	// deletedAudioPath, when set, is the audio_path DeleteSession reports the
	// deleted row had, in place of the one GetSession showed.
	deletedAudioPath string
	// deleteAudioDir is the audio directory the last DeleteSession was given.
	deleteAudioDir string
}

type embeddingClientStub struct {
	vectors       [][]float32
	err           error
	taskTypes     []embedding.TaskType
	reportedModel string
}

func (s *embeddingClientStub) Embed(ctx context.Context, texts []string, taskType embedding.TaskType) ([][]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.taskTypes = append(s.taskTypes, taskType)
	return s.vectors, nil
}

func (s *embeddingClientStub) ReportedModel() string {
	return s.reportedModel
}

var _ embedding.Client = (*embeddingClientStub)(nil)

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

func (s *apiStoreStub) GetSessionsByDate(date string, includeDiscarded bool) ([]storage.Session, error) {
	return s.sessionsByDate[date], nil
}

func (s *apiStoreStub) GetSession(id string) (storage.Session, error) {
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return storage.Session{}, os.ErrNotExist
}

func (s *apiStoreStub) GetSegments(sessionID string) ([]transcribe.Segment, error) {
	return s.segments[sessionID], nil
}

func (s *apiStoreStub) GetSegmentsInTimeRange(sessionID string, startTime, endTime float64) ([]transcribe.Segment, error) {
	all := s.segments[sessionID]
	var result []transcribe.Segment
	for i := range all {
		if all[i].StartTime >= startTime && all[i].EndTime <= endTime {
			result = append(result, all[i])
		}
	}
	return result, nil
}

func (s *apiStoreStub) GetDates() ([]string, error) {
	return s.dates, nil
}

func (s *apiStoreStub) Search(query string, opts storage.SearchOptions) ([]storage.SearchResult, error) {
	if s.searchFunc != nil {
		return s.searchFunc(query, opts)
	}
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if s.searchResults == nil {
		return []storage.SearchResult{}, nil
	}
	return s.searchResults[query], nil
}

func (s *apiStoreStub) AggregateSessions(opts storage.AggregateOptions) (storage.AggregateResult, error) {
	if s.aggregateFunc != nil {
		return s.aggregateFunc(opts)
	}
	if s.aggregateErr != nil {
		return storage.AggregateResult{}, s.aggregateErr
	}
	if s.aggregateResult != nil {
		return *s.aggregateResult, nil
	}
	return storage.AggregateResult{Groups: []storage.AggregateGroup{}}, nil
}

func (s *apiStoreStub) GetAllEmbeddings() ([]storage.StoredEmbedding, error) {
	if s.allEmbeddingsErr != nil {
		return nil, s.allEmbeddingsErr
	}
	if s.allEmbeddings == nil {
		return []storage.StoredEmbedding{}, nil
	}
	return s.allEmbeddings, nil
}

func (s *apiStoreStub) GetEventsSince(cursor int64, limit int) ([]storage.StoredEvent, error) {
	if s.getEventsSinceFunc != nil {
		return s.getEventsSinceFunc(cursor, limit)
	}
	return []storage.StoredEvent{}, nil
}

func (s *apiStoreStub) UpdateTitle(sessionID, title string) error {
	sess, ok := s.sessions[sessionID]
	if !ok {
		return os.ErrNotExist
	}
	sess.Title = title
	s.sessions[sessionID] = sess
	return nil
}

func (s *apiStoreStub) DeleteSession(id, audioDir string) (string, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return "", os.ErrNotExist
	}
	s.deleteAudioDir = audioDir
	delete(s.sessions, id)
	if s.deletedAudioPath != "" {
		return s.deletedAudioPath, nil
	}
	return sess.AudioPath, nil
}

func (s *apiStoreStub) ReserveRawSessionErase(id, _, hiddenPath string) (string, error) {
	if _, ok := s.sessions[id]; !ok {
		return "", os.ErrNotExist
	}
	return hiddenPath, nil
}

func (*apiStoreStub) CancelRawSessionErase(string, string) error {
	return nil
}

func (*apiStoreStub) SourceSessionIDs(string) ([]string, error) {
	return nil, nil
}
func (s *apiStoreStub) MergeSessions(newID string, sourceIDs []string, startedAt, endedAt time.Time) error {
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

func (s *apiStoreStub) GetSessionsForBackfill() ([]string, error) {
	return nil, nil
}

type healthCheckerStub struct{}

func (h *healthCheckerStub) IsDeepgramConfigured() bool           { return true }
func (h *healthCheckerStub) IsDeepgramConnected() bool            { return true }
func (h *healthCheckerStub) IsDBHealthy(ctx context.Context) bool { return true }
func (h *healthCheckerStub) IsMicOpen() bool                      { return true }
func (h *healthCheckerStub) MicHold() string                      { return "" }
func (h *healthCheckerStub) IsLLMHealthy() string                 { return "ok" }
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
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{
			"2026-02-26": {{ID: "s1", StartedAt: started, SummaryStatus: storage.SummaryCompleted, RefinedRevision: 7}},
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
	var sessions []storage.Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	if len(sessions) != 1 || sessions[0].RefinedRevision != 7 {
		t.Fatalf("session list refined revision = %#v, want 7", sessions)
	}
}

func TestAPISearchReturnsResults(t *testing.T) {
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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

func TestAPISessionDetailCarriesStoredSegmentIDs(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {{ID: 42, Text: "stored segment"}},
		},
	}
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/s1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Segments []transcribe.Segment `json:"segments"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode session detail: %v", err)
	}
	if len(payload.Segments) != 1 || payload.Segments[0].ID != 42 {
		t.Fatalf("segment IDs = %+v, want stored ID 42", payload.Segments)
	}
}

func TestDeleteSession_Success(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1"},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	var deletedSession string
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{OnSessionDeleted: func(sessionID string) {
		deletedSession = sessionID
	}}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d body=%s", rr.Code, rr.Body.String())
	}
	if deletedSession != "s1" {
		t.Errorf("deleted session callback = %q, want s1", deletedSession)
	}
}

// deleteSessionWithRecording serves DELETE /api/sessions/s1 for a session
// whose recording holds audio, with finish in place of finishErasing, and
// returns the recording's path and a descriptor held on it.
func deleteSessionWithRecording(t *testing.T, audio []byte, finish func(string, []erase.Pending)) (string, *os.File, <-chan *httptest.ResponseRecorder) {
	t.Helper()
	recording := filepath.Join(t.TempDir(), "s1.mp3")
	if err := os.WriteFile(recording, audio, 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(recording)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	original := finishErasing
	t.Cleanup(func() { finishErasing = original })
	finishErasing = finish
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", AudioPath: recording}},
		segments:       map[string][]transcribe.Segment{},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil))
		answered <- rr
	}()
	return recording, held, answered
}

// TestDeleteSession_ErasesWhatTheDeletedRowNamed fails if the session delete
// erases the recordings it read before the delete rather than those the row
// named when it went. (DeleteSession records their directories before the
// row goes: storage.TestDeleteSessionRecordsWhereItsRecordingsAreErasedFirst.)
func TestDeleteSession_ErasesWhatTheDeletedRowNamed(t *testing.T) {
	dir := t.TempDir()
	read, deleted := filepath.Join(dir, "read.mp3"), filepath.Join(dir, "deleted.mp3")
	for _, path := range []string{read, deleted} {
		if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	finish := finishErasing
	t.Cleanup(func() { finishErasing = finish })
	finished := make(chan struct{})
	finishErasing = func(sessionID string, pending []erase.Pending) {
		defer close(finished)
		finish(sessionID, pending)
	}
	store := &apiStoreStub{
		sessionsByDate:   map[string][]storage.Session{},
		sessions:         map[string]storage.Session{"s1": {ID: "s1", AudioPath: read}},
		segments:         map[string][]transcribe.Segment{},
		deletedAudioPath: deleted,
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the erase after the response never finished")
	}
	if _, err := os.Stat(deleted); !os.IsNotExist(err) {
		t.Errorf("the recording the deleted row named is still there: %v", err)
	}
	if _, err := os.Stat(read); err != nil {
		t.Errorf("the recording read before the delete was erased: %v", err)
	}
}

// configStoreWithAudioDir is a config store whose audio_dir is dir.
func configStoreWithAudioDir(t *testing.T, dir string) *config.Store {
	t.Helper()
	cfgStore := newTestConfigStore(t)
	if err := cfgStore.Update(func(c *config.Config) { c.AudioDir = dir }); err != nil {
		t.Fatal(err)
	}
	return cfgStore
}

// TestDeleteSession_ErasesARecordingMovedWithTheAudioDir fails if the session
// delete erases only the recording where the row's entry points: after
// audio_dir changes and the recordings move with it, the recording is in the
// new audio directory (recording.Resolve), and it would stay on the unit,
// named by no row. It also fails if DeleteSession, which records the
// directories it is erased in, is not given that audio directory.
func TestDeleteSession_ErasesARecordingMovedWithTheAudioDir(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	moved := filepath.Join(newDir, "s1.mp3")
	if err := os.WriteFile(moved, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	finish := finishErasing
	t.Cleanup(func() { finishErasing = finish })
	finished := make(chan struct{})
	finishErasing = func(sessionID string, pending []erase.Pending) {
		defer close(finished)
		finish(sessionID, pending)
	}
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", AudioPath: filepath.Join(oldDir, "s1.mp3")}},
		segments:       map[string][]transcribe.Segment{},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, newDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the erase after the response never finished")
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Errorf("the recording moved with the audio directory is still there: %v", err)
	}
	if store.deleteAudioDir != newDir {
		t.Errorf("DeleteSession was given audio directory %q, want %s", store.deleteAudioDir, newDir)
	}
}

// TestAPIAudioServesARecordingMovedWithTheAudioDir fails if the audio route
// looks for a recording only where the row's entry points: after audio_dir
// changes and the recordings move with it, the recording is in the new audio
// directory (recording.Resolve).
func TestAPIAudioServesARecordingMovedWithTheAudioDir(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("new", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("new", "s1.mp3"), []byte("the moved recording"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", AudioPath: filepath.Join("old", "s1.mp3")}},
		segments:       map[string][]transcribe.Segment{},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, "new"))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/s1/audio", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "the moved recording" {
		t.Errorf("GET audio = %d %q, want the recording in the new audio directory", rr.Code, rr.Body.String())
	}
}

// TestDeleteSession_ZeroesItsAudio fails if deleting a session removes its
// recordings without overwriting their bytes before the file system frees
// them.
func TestDeleteSession_ZeroesItsAudio(t *testing.T) {
	audio := []byte("the session's audio")
	finish := finishErasing
	finished := make(chan struct{})
	recording, held, answered := deleteSessionWithRecording(t, audio, func(sessionID string, pending []erase.Pending) {
		defer close(finished)
		finish(sessionID, pending)
	})
	if rr := <-answered; rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the erase after the response never finished")
	}
	if _, err := os.Stat(recording); !os.IsNotExist(err) {
		t.Fatalf("the recording is still there: %v", err)
	}
	got := make([]byte, len(audio))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("the removed recording still holds %q", got)
	}
}

// TestDeleteSession_ZeroesItsRawPCM fails if a session delete leaves the raw
// microphone capture beside the saved recording. Raw PCM is whole-session
// audio, so it follows the same zero-before-unlink rule as the saved file.
func TestDeleteSession_ZeroesItsRawPCM(t *testing.T) {
	dir := t.TempDir()
	saved, raw := filepath.Join(dir, "s1.mp3"), filepath.Join(dir, "s1.pcm")
	if err := os.WriteFile(saved, []byte("saved session audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawBytes := []byte("raw microphone audio")
	if err := os.WriteFile(raw, rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	finish := finishErasing
	t.Cleanup(func() { finishErasing = finish })
	finished := make(chan struct{})
	finishErasing = func(sessionID string, pending []erase.Pending) {
		defer close(finished)
		finish(sessionID, pending)
	}
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", AudioPath: saved}},
		segments:       map[string][]transcribe.Segment{},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the erase after the response never finished")
	}
	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Fatalf("the raw PCM is still on disk: %v", err)
	}
	got := make([]byte, len(rawBytes))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("the removed raw PCM still holds %q", got)
	}
}

// TestDeleteSessionKeepsItsRowWhenRawPCMCannotBeHidden fails if a raw
// microphone capture that cannot be renamed is left behind after the session
// row has gone: the user then cannot retry its secure removal.
func TestDeleteSessionKeepsItsRowWhenRawPCMCannotBeHidden(t *testing.T) {
	dir := t.TempDir()
	original := beginReservedErasing
	t.Cleanup(func() { beginReservedErasing = original })
	beginReservedErasing = func(reservation erase.Reservation) (erase.Pending, error) {
		if reservation.OriginalPath() != filepath.Join(dir, "s1.pcm") {
			t.Errorf("raw erase path = %q, want %q", reservation.OriginalPath(), filepath.Join(dir, "s1.pcm"))
		}
		return erase.Pending{}, errors.New("rename raw pcm: input/output error")
	}
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", AudioPath: filepath.Join(dir, "s1.mp3")}},
		segments:       map[string][]transcribe.Segment{},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete = %d %s, want raw erase failure", rr.Code, rr.Body.String())
	}
	if _, found := store.sessions["s1"]; !found {
		t.Fatal("session row was deleted after its raw PCM could not be hidden")
	}
}

// TestDeleteSession_AnswersBeforeItZeroesTheAudio fails if deleting a session
// holds the request until its recordings are overwritten, a write as large as
// they are, or answers before they have left their names.
func TestDeleteSession_AnswersBeforeItZeroesTheAudio(t *testing.T) {
	audio := []byte("the session's audio")
	finish := finishErasing
	release, finished := make(chan struct{}), make(chan struct{})
	recording, held, answered := deleteSessionWithRecording(t, audio, func(sessionID string, pending []erase.Pending) {
		defer close(finished)
		<-release
		finish(sessionID, pending)
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case rr := <-answered:
		if rr.Code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session delete waited for its recordings to be overwritten before it answered")
	}
	if _, err := os.Stat(recording); !os.IsNotExist(err) {
		t.Fatalf("the recording still has its name when the delete answers: %v", err)
	}
	close(release)
	<-finished
	got := make([]byte, len(audio))
	if _, err := held.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, len(got))) {
		t.Errorf("the removed recording still holds %q", got)
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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

func TestDeleteSegmentAPI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		session   storage.Session
		hook      func(context.Context, string, int64) error
		wantCode  int
		wantError string
	}{
		{
			name:    "deleted",
			session: storage.Session{ID: "s1", Status: storage.SessionEnded},
			hook: func(_ context.Context, sessionID string, segmentID int64) error {
				if sessionID != "s1" || segmentID != 42 {
					t.Fatalf("delete hook received session=%q segment=%d", sessionID, segmentID)
				}
				return nil
			},
			wantCode: http.StatusNoContent,
		},
		{
			name:     "unknown segment",
			session:  storage.Session{ID: "s1", Status: storage.SessionEnded},
			hook:     func(context.Context, string, int64) error { return storage.ErrSegmentNotFound },
			wantCode: http.StatusNotFound,
		},
		{
			name:    "kept raw recording",
			session: storage.Session{ID: "s1", Status: storage.SessionEnded},
			hook: func(context.Context, string, int64) error {
				return fmt.Errorf("%w: /audio/s1.pcm", ErrSegmentRawAudioKept)
			},
			wantCode:  http.StatusUnprocessableEntity,
			wantError: "s1.pcm",
		},
		{
			name:    "active",
			session: storage.Session{ID: "s1", Status: storage.SessionActive},
			hook: func(context.Context, string, int64) error {
				t.Fatal("delete hook called for active session")
				return nil
			},
			wantCode:  http.StatusConflict,
			wantError: "This conversation is still being recorded. Delete the segment after it ends.",
		},
		{
			name:    "unknown session",
			session: storage.Session{},
			hook: func(context.Context, string, int64) error {
				t.Fatal("delete hook called for unknown session")
				return nil
			},
			wantCode:  http.StatusNotFound,
			wantError: "get session",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := tc.session
			if session.ID != "" && session.Status != storage.SessionActive {
				session.RefinementStatus = storage.RefinementCompleted
				session.RefinedRevision = 1
			}
			sessions := map[string]storage.Session{}
			if session.ID != "" {
				sessions[session.ID] = session
			}
			store := &apiStoreStub{sessionsByDate: map[string][]storage.Session{}, sessions: sessions, segments: map[string][]transcribe.Segment{}}
			handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
				DeleteSegment: func(ctx context.Context, sessionID string, segmentID int64, _ int) error {
					return tc.hook(ctx, sessionID, segmentID)
				},
			}, "", nil)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			req := httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode=text_and_audio&revision=1", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != tc.wantCode {
				t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), tc.wantCode)
			}
			if tc.wantError != "" && !strings.Contains(response.Body.String(), tc.wantError) {
				t.Fatalf("body = %q, want %q", response.Body.String(), tc.wantError)
			}
		})
	}
}

func TestMergeSession_Success(t *testing.T) {
	start1 := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	end1 := start1.Add(10 * time.Minute)
	start2 := time.Date(2026, 2, 26, 11, 0, 0, 0, time.UTC)
	end2 := start2.Add(20 * time.Minute)

	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: start1, EndedAt: &end1, Status: storage.SessionEnded},
			"s2": {ID: "s2", StartedAt: start2, EndedAt: &end2, Status: storage.SessionEnded},
		},
		segments: map[string][]transcribe.Segment{},
		dates:    nil,
	}

	var mergedSessions []string
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{OnSessionsMerged: func(sessionIDs []string) {
		mergedSessions = slices.Clone(sessionIDs)
	}}, "", nil)
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
	if !slices.Equal(mergedSessions, []string{"s1", "s2"}) {
		t.Errorf("merged sessions callback = %v, want [s1 s2]", mergedSessions)
	}
}

func TestMergeSession_TooFewSessions(t *testing.T) {
	start := time.Date(2026, 2, 26, 10, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)

	store := &apiStoreStub{
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

	store := &apiStoreStub{
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

// TestMergeSession_RefusesASessionThatOwesADriveReplacement fails if the
// merge route merges, or answers anything but 409 with the reason, while a
// session to merge still has a deleted segment on Google Drive.
func TestMergeSession_RefusesASessionThatOwesADriveReplacement(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"20261106100000", "20261106110000"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = '20261106110000'`); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/merge", strings.NewReader(`{"session_ids":["20261106100000","20261106110000"]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "deleted segment on Google Drive") {
		t.Fatalf("merge of a session owing a Drive replacement = %d %s, want 409 with the reason", rr.Code, rr.Body.String())
	}
	if sess, err := store.GetSession("20261106100000"); err != nil || sess.Status != storage.SessionEnded {
		t.Fatalf("after the refused merge the other session is %q (%v), want it ended", sess.Status, err)
	}
}

// TestDeleteSession_RefusesASessionThatOwesADriveReplacement fails if the
// delete route removes a session whose deleted segment is still on Google
// Drive: with its row go its replacement ledger and its status warning, and
// the Drive folder keeps the words.
func TestDeleteSession_RefusesASessionThatOwesADriveReplacement(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	const id = "20261106100000"
	if err := store.CreateSession(id, started); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+id, nil))

	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "deleted segment on Google Drive") {
		t.Fatalf("delete of a session owing a Drive replacement = %d %s, want 409 with the reason", rr.Code, rr.Body.String())
	}
	if _, err := store.GetSession(id); err != nil {
		t.Fatalf("the refused delete removed the session: %v", err)
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

	store := &apiStoreStub{
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

// TestAPIResponsesAreNotKeptByBrowsers fails if a recording or a session's
// transcript is served in a way a browser may keep in its disk cache: a
// segment deleted on the unit would stay there, and a spliced recording is
// served at the URL the unspliced one had.
func TestAPIResponsesAreNotKeptByBrowsers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "audio.mp3"), []byte(strings.Repeat("a", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{"s1": {ID: "s1", AudioPath: "audio.mp3"}},
		segments:       map[string][]transcribe.Segment{"s1": {{ID: 1, Text: "words on the unit"}}},
		dates:          []string{"2026-02-26"},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/sessions/s1/audio", "/api/sessions/s1", "/api/dates"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code >= 400 {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestAPIDates(t *testing.T) {
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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

func TestAPIStatusReportsCapturing(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func() bool
		want bool
	}{
		{name: "capturing", hook: func() bool { return true }, want: true},
		{name: "not capturing", hook: func() bool { return false }, want: false},
		{name: "no mic wiring", hook: nil, want: false},
	} {
		h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
			sessionsByDate: map[string][]storage.Session{},
			sessions:       map[string]storage.Session{},
			segments:       map[string][]transcribe.Segment{},
		}, &ControlHooks{
			IsCapturing: tc.hook,
		}, "", nil)
		if err != nil {
			t.Fatalf("Handler failed: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode status: %v (body=%s)", err, rr.Body.String())
		}
		if resp["capturing"] != tc.want {
			t.Fatalf("%s: capturing = %#v, want %v (body=%s)", tc.name, resp["capturing"], tc.want, rr.Body.String())
		}
	}
}

// A resume that has not brought the microphone back past the stall limit is
// reported in the status and as a warning the UI shows, not as paused.
func TestAPIStatusReportsAStalledCapture(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		IsPaused:     func() bool { return false },
		CaptureState: func() string { return "stalled" },
		Warnings:     func() []string { return []string{"existing warning"} },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	var resp struct {
		Paused       bool     `json:"paused"`
		CaptureState string   `json:"capture_state"`
		Warnings     []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, rr.Body.String())
	}
	if resp.Paused || resp.CaptureState != "stalled" {
		t.Fatalf("paused=%v capture_state=%q, want false and stalled", resp.Paused, resp.CaptureState)
	}
	if len(resp.Warnings) != 2 || resp.Warnings[0] != "existing warning" || !strings.Contains(resp.Warnings[1], "not resumed") {
		t.Fatalf("warnings = %q, want the existing one and a capture warning", resp.Warnings)
	}
}

// Raw recordings kept because they could not be saved are reported in the
// status, with a warning the UI shows once they take more than the warning
// size, so the user recovers or deletes them by hand: nothing removes them.
func TestAPIStatusReportsKeptRawRecordings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kept    KeptRawRecordings
		warning string // "" for no warning
	}{
		{name: "none", kept: KeptRawRecordings{WarnBytes: 1000}},
		{name: "under the warning size", kept: KeptRawRecordings{Count: 1, Bytes: 1000, WarnBytes: 1000}},
		{
			name:    "one over the warning size",
			kept:    KeptRawRecordings{Count: 1, Bytes: 1001, WarnBytes: 1000},
			warning: "1 recording could not be saved: its raw audio (0.0 MB) is kept in the audio directory. Recover or delete it by hand.",
		},
		{
			name:    "several over the warning size",
			kept:    KeptRawRecordings{Count: 3, Bytes: 432537600, WarnBytes: 107374182},
			warning: "3 recordings could not be saved: their raw audio (412.5 MB) is kept in the audio directory. Recover or delete them by hand.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := tc.kept
			h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
				sessionsByDate: map[string][]storage.Session{},
				sessions:       map[string]storage.Session{},
				segments:       map[string][]transcribe.Segment{},
			}, &ControlHooks{
				KeptRawRecordings: func() KeptRawRecordings { return kept },
			}, "", nil)
			if err != nil {
				t.Fatalf("Handler failed: %v", err)
			}

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))

			var resp struct {
				KeptRawRecordings int      `json:"kept_raw_recordings"`
				KeptRawBytes      int64    `json:"kept_raw_bytes"`
				Warnings          []string `json:"warnings"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode status: %v (body=%s)", err, rr.Body.String())
			}
			if resp.KeptRawRecordings != kept.Count || resp.KeptRawBytes != kept.Bytes {
				t.Errorf("kept_raw_recordings=%d kept_raw_bytes=%d, want %d and %d", resp.KeptRawRecordings, resp.KeptRawBytes, kept.Count, kept.Bytes)
			}
			var want []string
			if tc.warning != "" {
				want = []string{tc.warning}
			}
			if !slices.Equal(resp.Warnings, want) {
				t.Errorf("warnings = %q, want %q", resp.Warnings, want)
			}
		})
	}
}

func TestGetPresets(t *testing.T) {
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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

// A refused PATCH /api/config changes nothing: not the config, not its file,
// and no change is announced. The settings page sends all three GC fields in
// one body; a negative budget among them used to leave GC enabled with the new
// age limit saved, while the page reported the save as failed.
func TestAPI_PatchConfig_RefusedPatchChangesNothing(t *testing.T) {
	newTestConfigStore(t) // clears the environment
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte("gc_enabled: false\ngc_max_age_days: 30\ngc_max_audio_size_mb: 1024\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var announced atomic.Int32
	cfgStore.OnChange(func(config.Config) { announced.Add(1) })
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	body := `{"gc":{"enabled":true,"max_age_days":1,"max_audio_size_mb":-1}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PATCH %s: %d %s, want 400", body, rr.Code, rr.Body.String())
	}
	if cfg := cfgStore.Get(); cfg.GCEnabled || cfg.GCMaxAgeDays != 30 || cfg.GCMaxAudioSizeMB != 1024 {
		t.Errorf("a refused PATCH changed the config: gc_enabled %v, gc_max_age_days %d, gc_max_audio_size_mb %d; want false, 30, 1024",
			cfg.GCEnabled, cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("a refused PATCH rewrote the config file (%v):\n%s", err, after)
	}
	if n := announced.Load(); n != 0 {
		t.Errorf("a refused PATCH announced %d config changes", n)
	}
}

// 0 is each GC limit's "no limit", so PATCH /api/config accepts it. A
// negative limit is refused.
func TestAPI_PatchConfig_AcceptsNoGCLimit(t *testing.T) {
	cfgStore := newTestConfigStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := patch(`{"gc":{"max_age_days":0,"max_audio_size_mb":0}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("no limit: %d %s, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		GC struct {
			MaxAgeDays     *int `json:"max_age_days"`
			MaxAudioSizeMB *int `json:"max_audio_size_mb"`
		} `json:"gc"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.GC.MaxAgeDays == nil || *resp.GC.MaxAgeDays != 0 || resp.GC.MaxAudioSizeMB == nil || *resp.GC.MaxAudioSizeMB != 0 {
		t.Errorf("response gc = %+v, want max_age_days 0 and max_audio_size_mb 0", resp.GC)
	}
	if cfg := cfgStore.Get(); cfg.GCMaxAgeDays != 0 || cfg.GCMaxAudioSizeMB != 0 {
		t.Errorf("config gc_max_age_days %d, gc_max_audio_size_mb %d; want 0 and 0", cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}

	for _, body := range []string{`{"gc":{"max_age_days":-1}}`, `{"gc":{"max_audio_size_mb":-1}}`} {
		if rr := patch(body); rr.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s: %d %s, want 400", body, rr.Code, rr.Body.String())
		}
	}
	if cfg := cfgStore.Get(); cfg.GCMaxAgeDays != 0 || cfg.GCMaxAudioSizeMB != 0 {
		t.Errorf("a refused patch changed the config: gc_max_age_days %d, gc_max_audio_size_mb %d", cfg.GCMaxAgeDays, cfg.GCMaxAudioSizeMB)
	}
}

// dig returns the value at keys in a decoded JSON object.
func dig(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for i, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%v is not an object", keys[:i])
		}
		if v, ok = m[key]; !ok {
			t.Fatalf("no %v", keys[:i+1])
		}
	}
	return v
}

// The OpenAPI spec documents both GC limits in GET and PATCH /api/config,
// with 0 as "no limit".
func TestOpenAPISpecDocumentsTheGCLimits(t *testing.T) {
	raw, err := OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, schema := range [][]string{
		{"paths", "/api/config", "get", "responses", "200", "content", "application/json", "schema"},
		{"paths", "/api/config", "patch", "requestBody", "content", "application/json", "schema"},
	} {
		gc := dig(t, spec, append(schema, "properties", "gc", "properties")...)
		for _, limit := range []string{"max_age_days", "max_audio_size_mb"} {
			if minimum := dig(t, gc, limit, "minimum"); minimum != float64(0) {
				t.Errorf("%v gc.%s minimum %v, want 0", schema[:3], limit, minimum)
			}
			if description, _ := dig(t, gc, limit, "description").(string); !strings.Contains(description, "0 means no limit") {
				t.Errorf("%v gc.%s description %q does not say 0 means no limit", schema[:3], limit, description)
			}
		}
	}
}

func TestFaultInjectionDisabledWithoutTestMode(t *testing.T) {
	store := &apiStoreStub{
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

	store := &apiStoreStub{
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

	store := &apiStoreStub{
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
		t.Fatalf("expected 503 when Deepgram is not configured, got %d", rr.Code)
	}
}

// --- Manual Session Trigger Tests ---

func TestManualSessionStart_Success(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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

func TestManualSessionStart_CapturePaused(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}, &ControlHooks{
		StartSession: func(_ context.Context, titleHint string) (string, error) {
			return "", session.ErrCapturePaused
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
	if !strings.Contains(rr.Body.String(), "capture is paused") {
		t.Fatalf("expected the error to say capture is paused, got %s", rr.Body.String())
	}
}

func TestManualSessionStart_NotConfigured(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
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
	store := &apiStoreStub{
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

func TestCurrentTranscriptEndpoint_WithActiveSession(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Status: storage.SessionActive},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "hello", StartTime: 0, EndTime: 1, Timestamp: started},
				{Speaker: 1, Text: "world", StartTime: 1, EndTime: 2, Timestamp: started.Add(1 * time.Second)},
			},
		},
		dates: []string{"2026-03-23"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		ActiveSession: func() (string, time.Time) { return "s1", started },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/current/transcript", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Session struct {
			ID        string    `json:"id"`
			StartedAt time.Time `json:"started_at"`
			Status    string    `json:"status"`
		} `json:"session"`
		Segments []transcribe.Segment `json:"segments"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Session.ID != "s1" {
		t.Fatalf("expected session id 's1', got %q", resp.Session.ID)
	}
	if len(resp.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(resp.Segments))
	}
}

func TestCurrentTranscriptEndpoint_NoActiveSession(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
		dates:          nil,
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		ActiveSession: func() (string, time.Time) { return "", time.Time{} },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/current/transcript", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no active session") {
		t.Fatalf("expected error message about no active session, got %s", rr.Body.String())
	}
}

func TestCurrentTranscriptEndpoint_WithSinceFilter(t *testing.T) {
	started := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", StartedAt: started, Status: storage.SessionActive},
		},
		segments: map[string][]transcribe.Segment{
			"s1": {
				{Speaker: 0, Text: "first", StartTime: 0, EndTime: 1, Timestamp: started},
				{Speaker: 1, Text: "second", StartTime: 1, EndTime: 2, Timestamp: started.Add(1 * time.Second)},
				{Speaker: 0, Text: "third", StartTime: 2, EndTime: 3, Timestamp: started.Add(2 * time.Second)},
			},
		},
		dates: []string{"2026-03-23"},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		ActiveSession: func() (string, time.Time) { return "s1", started },
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	// Request with since filter - should only get segments after 1 second
	sinceTime := started.Add(500 * time.Millisecond).UnixMilli()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/sessions/current/transcript?since=%d", sinceTime), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Segments []transcribe.Segment `json:"segments"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(resp.Segments) != 2 {
		t.Fatalf("expected 2 segments after filter, got %d", len(resp.Segments))
	}
	if resp.Segments[0].Text != "second" {
		t.Fatalf("expected first segment to be 'second', got %q", resp.Segments[0].Text)
	}
}

func TestSearchEndpointWithFilters(t *testing.T) {
	mux := http.NewServeMux()
	var capturedOpts storage.SearchOptions
	store := &apiStoreStub{
		searchFunc: func(query string, opts storage.SearchOptions) ([]storage.SearchResult, error) {
			capturedOpts = opts
			return []storage.SearchResult{{SessionID: "sess-1", Title: "Meeting 1", Snippet: "test content", Rank: 0.5}}, nil
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	req := httptest.NewRequest("GET", "/api/search?q=test&date_from=2026-03-25T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if capturedOpts.DateFrom != "2026-03-25T00:00:00Z" {
		t.Fatalf("expected DateFrom=2026-03-25T00:00:00Z, got %q", capturedOpts.DateFrom)
	}
}

func TestSearchEndpointWithPresetFilter(t *testing.T) {
	mux := http.NewServeMux()
	var capturedOpts storage.SearchOptions
	store := &apiStoreStub{
		searchFunc: func(query string, opts storage.SearchOptions) ([]storage.SearchResult, error) {
			capturedOpts = opts
			return []storage.SearchResult{{SessionID: "sess-1", Title: "Meeting", Snippet: "test content", Rank: 0.5}}, nil
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	req := httptest.NewRequest("GET", "/api/search?q=test&preset=meeting", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if capturedOpts.Preset != "meeting" {
		t.Fatalf("expected Preset=meeting, got %q", capturedOpts.Preset)
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
	client := &embeddingClientStub{vectors: [][]float32{{1, 0}}, reportedModel: "provider-embedding-model"}
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, client)

	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=planning&limit=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var payload struct {
		Model   string `json:"model"`
		Results []struct {
			SessionID  string  `json:"session_id"`
			Title      string  `json:"title"`
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
	if payload.Model != client.reportedModel {
		t.Fatalf("response model = %q, want provider value %q", payload.Model, client.reportedModel)
	}
	if len(client.taskTypes) != 1 || client.taskTypes[0] != embedding.TaskTypeQuery {
		t.Fatalf("expected query task type, got %#v", client.taskTypes)
	}
}

func TestSemanticSearch_NoClient(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{}, &ControlHooks{}, "", nil)
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
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, &embeddingClientStub{vectors: [][]float32{{1, 0}}})

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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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

	store := &apiStoreStub{
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
	mux := http.NewServeMux()
	var capturedOpts storage.AggregateOptions
	store := &apiStoreStub{
		aggregateFunc: func(opts storage.AggregateOptions) (storage.AggregateResult, error) {
			capturedOpts = opts
			return storage.AggregateResult{
				SessionCount: 1,
				Groups: []storage.AggregateGroup{
					{Key: "meeting", Count: 1, Sessions: []storage.SessionSummary{{ID: "s1", Title: "Meeting", SummaryPreset: "meeting"}}},
				},
			}, nil
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/aggregate?group_by=preset&preset=meeting&date_from=2026-03-20T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}
	if capturedOpts.GroupBy != "preset" {
		t.Fatalf("expected GroupBy=preset, got %q", capturedOpts.GroupBy)
	}
	if capturedOpts.Preset != "meeting" {
		t.Fatalf("expected Preset=meeting, got %q", capturedOpts.Preset)
	}
	if capturedOpts.DateFrom != "2026-03-20T00:00:00Z" {
		t.Fatalf("expected DateFrom=2026-03-20T00:00:00Z, got %q", capturedOpts.DateFrom)
	}

	var result storage.AggregateResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.SessionCount != 1 {
		t.Fatalf("expected session_count 1, got %d", result.SessionCount)
	}
}

func TestAggregateEndpoint_InvalidGroupBy(t *testing.T) {
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
		"/api/sessions/{id}/segments/{segmentID}",
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Pause: func() error {
			called = true
			return nil
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
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions:       map[string]storage.Session{},
		segments:       map[string][]transcribe.Segment{},
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		Resume: func() error {
			called = true
			return nil
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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
	store := &apiStoreStub{
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

func TestSearchEndpointWithDateTo(t *testing.T) {
	mux := http.NewServeMux()
	var capturedOpts storage.SearchOptions
	store := &apiStoreStub{
		searchFunc: func(query string, opts storage.SearchOptions) ([]storage.SearchResult, error) {
			capturedOpts = opts
			return []storage.SearchResult{{SessionID: "s1", Title: "M1", Snippet: "test", Rank: 0.5}}, nil
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	req := httptest.NewRequest("GET", "/api/search?q=test&date_to=2026-03-26T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedOpts.DateTo != "2026-03-26T00:00:00Z" {
		t.Fatalf("expected DateTo=2026-03-26T00:00:00Z, got %q", capturedOpts.DateTo)
	}
}

func TestSemanticSearch_DateFilter(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{
		sessions: map[string]storage.Session{
			"sess-new": {ID: "sess-new", Title: "New Session", StartedAt: time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)},
			"sess-old": {ID: "sess-old", Title: "Old Session", StartedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)},
		},
		allEmbeddings: []storage.StoredEmbedding{
			{SessionID: "sess-new", ChunkIndex: 0, Vector: []float32{1, 0}},
			{SessionID: "sess-old", ChunkIndex: 0, Vector: []float32{1, 0}},
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, &embeddingClientStub{vectors: [][]float32{{1, 0}}})

	req := httptest.NewRequest("GET", "/api/search/semantic?q=test&date_from=2026-03-24T00:00:00Z&date_to=2026-03-26T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var payload struct {
		Results []struct {
			SessionID string `json:"session_id"`
		} `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected 1 result (only in-range session), got %d", len(payload.Results))
	}
	if payload.Results[0].SessionID != "sess-new" {
		t.Fatalf("expected sess-new, got %q", payload.Results[0].SessionID)
	}
}

func TestEventsEndpoint_CursorFollowUp(t *testing.T) {
	mux := http.NewServeMux()
	allEvents := []storage.StoredEvent{
		{ID: 1, EventType: "session_started", Payload: "{}", CreatedAt: time.Now()},
		{ID: 2, EventType: "session_ended", Payload: "{}", CreatedAt: time.Now()},
		{ID: 3, EventType: "summary_ready", Payload: "{}", CreatedAt: time.Now()},
	}
	store := &apiStoreStub{
		getEventsSinceFunc: func(cursor int64, limit int) ([]storage.StoredEvent, error) {
			var result []storage.StoredEvent
			for _, ev := range allEvents {
				if ev.ID > cursor {
					result = append(result, ev)
				}
				if len(result) >= limit {
					break
				}
			}
			return result, nil
		},
	}

	cfgStore := newTestConfigStore(t)
	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	// First request: get first 2 events
	req1 := httptest.NewRequest("GET", "/api/events?limit=2", nil)
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w1.Code)
	}
	var resp1 struct {
		Events     []map[string]interface{} `json:"events"`
		NextCursor float64                  `json:"next_cursor"`
		HasMore    bool                     `json:"has_more"`
	}
	if err := json.NewDecoder(w1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if len(resp1.Events) != 2 {
		t.Fatalf("first request: expected 2 events, got %d", len(resp1.Events))
	}
	if !resp1.HasMore {
		t.Fatal("first request: expected has_more=true")
	}

	// Second request: use cursor from first response
	req2 := httptest.NewRequest("GET", fmt.Sprintf("/api/events?cursor=%d&limit=2", int64(resp1.NextCursor)), nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d", w2.Code)
	}
	var resp2 struct {
		Events     []map[string]interface{} `json:"events"`
		NextCursor float64                  `json:"next_cursor"`
		HasMore    bool                     `json:"has_more"`
	}
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if len(resp2.Events) != 1 {
		t.Fatalf("second request: expected 1 event, got %d", len(resp2.Events))
	}
	if resp2.HasMore {
		t.Fatal("second request: expected has_more=false")
	}
}

func TestRepairSummariesEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{}
	cfgStore := newTestConfigStore(t)

	registerAPIRoutes(mux, store, &ControlHooks{
		RepairSummaries: func(ctx context.Context) (int, error) {
			return 22, nil
		},
	}, &healthCheckerStub{}, cfgStore, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/repair-summaries", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var payload map[string]int
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["repaired"] != 22 {
		t.Fatalf("expected repaired=22, got %+v", payload)
	}
}

func TestRepairSummariesEndpointUnavailableWithoutHook(t *testing.T) {
	mux := http.NewServeMux()
	store := &apiStoreStub{}
	cfgStore := newTestConfigStore(t)

	registerAPIRoutes(mux, store, &ControlHooks{}, &healthCheckerStub{}, cfgStore, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/repair-summaries", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestTTSSpeakReportsProviderModelAndVoiceInHeaders(t *testing.T) {
	const (
		configuredModel = "configured-tts-model"
		providerModel   = "provider-tts-model"
		providerVoice   = "provider-tts-voice"
	)
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{}, &ControlHooks{
		TTSSpeak: func(text, voice, provider string) (string, error) {
			if text == "" || voice != providerVoice || provider != "gemini" {
				return "", errors.New("unexpected speech request")
			}
			return "speech-request", nil
		},
		TTSStatus: func(id string) (map[string]any, bool) {
			if id != "speech-request" {
				return nil, false
			}
			return map[string]any{
				"id":     id,
				"status": "queued",
				"model":  providerModel,
				"voice":  providerVoice,
			}, true
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tts/speak", strings.NewReader(`{"text":"model acceptance","voice":"provider-tts-voice","provider":"gemini"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /api/tts/speak = %d, want 202: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Ghost-Wispr-TTS-Model"); got != providerModel {
		t.Fatalf("TTS model header = %q, want provider value %q, not configured %q", got, providerModel, configuredModel)
	}
	if got := rr.Header().Get("X-Ghost-Wispr-TTS-Voice"); got != providerVoice {
		t.Fatalf("TTS voice header = %q, want provider value %q", got, providerVoice)
	}
}

func TestSessionDetailReportsProviderModels(t *testing.T) {
	store := &apiStoreStub{
		sessions: map[string]storage.Session{
			"provider-models": {
				ID:              "provider-models",
				SummaryModel:    "provider-summary-model",
				RefinementModel: "provider-refinement-model",
			},
		},
		segments: map[string][]transcribe.Segment{"provider-models": {}},
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/provider-models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET session = %d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Session struct {
			SummaryModel    string `json:"summary_model"`
			RefinementModel string `json:"refinement_model"`
		} `json:"session"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Session.SummaryModel != "provider-summary-model" {
		t.Fatalf("summary_model = %q", payload.Session.SummaryModel)
	}
	if payload.Session.RefinementModel != "provider-refinement-model" {
		t.Fatalf("refinement_model = %q", payload.Session.RefinementModel)
	}
}

func TestTTSSpeakReturnsBadGatewayAfterSynthesisFailure(t *testing.T) {
	h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{}, &ControlHooks{
		TTSSpeak: func(string, string, string) (string, error) { return "failed-request", nil },
		TTSWaitForSynthesis: func(context.Context, string) (map[string]any, error) {
			return map[string]any{"status": tts.StatusFailed, "error": "provider refused synthesis"}, nil
		},
	}, "", nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tts/speak", strings.NewReader(`{"text":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("POST /api/tts/speak = %d, want 502: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provider refused synthesis") {
		t.Fatalf("failure response %q omits synthesis error", rr.Body.String())
	}
}

type deleteSessionErrorStore struct {
	*storage.SQLiteStore
	err error
}

func (s *deleteSessionErrorStore) DeleteSession(string, string) (string, error) {
	return "", s.err
}

type sourceRawPredeleteStore struct {
	*storage.SQLiteStore
	sourceRaw  []string
	stillNamed []string
}

func (s *sourceRawPredeleteStore) DeleteSession(id, audioDir string) (string, error) {
	for _, raw := range s.sourceRaw {
		if _, err := os.Stat(raw); err == nil {
			s.stillNamed = append(s.stillNamed, raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return s.SQLiteStore.DeleteSession(id, audioDir)
}

func newMergedDeleteStore(t *testing.T) (*storage.SQLiteStore, string, map[string][]byte) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"source", "later-source"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), id+".mp3"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("merged", []string{"source", "later-source"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}

	audioDir := t.TempDir()
	raw := map[string][]byte{
		"merged":       []byte("merged raw microphone capture"),
		"source":       []byte("source raw microphone capture"),
		"later-source": []byte("later source raw microphone capture"),
	}
	for id, contents := range raw {
		if err := os.WriteFile(filepath.Join(audioDir, id+".pcm"), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return store, audioDir, raw
}

func TestDeleteSessionHidesEveryMergedSourceRawPCM(t *testing.T) {
	store, audioDir, raw := newMergedDeleteStore(t)
	finish := finishRawErasing
	t.Cleanup(func() { finishRawErasing = finish })
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	finishRawErasing = func(store SessionStore, reservations []rawEraseReservation) {
		close(entered)
		<-release
		finish(store, reservations)
	}

	checkedStore := &sourceRawPredeleteStore{
		SQLiteStore: store,
		sourceRaw: []string{
			filepath.Join(audioDir, "source.pcm"),
			filepath.Join(audioDir, "later-source.pcm"),
		},
	}
	h, err := Handler(testStaticFS(t), NewHub(), checkedStore, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/merged", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete merged session = %d %s", rr.Code, rr.Body.String())
	}
	if len(checkedStore.stillNamed) != 0 {
		t.Errorf("merged source raw PCM still named when DeleteSession ran: %v", checkedStore.stillNamed)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the post-delete erasure did not begin")
	}
	for id := range raw {
		if _, err := os.Stat(filepath.Join(audioDir, id+".pcm")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("raw PCM for %s after merged delete = %v, want hidden before the answer", id, err)
		}
	}
	close(release)
	released = true
}

func TestDeleteSessionRestoresMergedSourcesWhenRowDeleteFails(t *testing.T) {
	store, audioDir, raw := newMergedDeleteStore(t)
	h, err := Handler(testStaticFS(t), NewHub(), &deleteSessionErrorStore{SQLiteStore: store, err: errors.New("database unavailable")}, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/merged", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete with database failure = %d %s, want 500", rr.Code, rr.Body.String())
	}
	for id, want := range raw {
		if got, err := os.ReadFile(filepath.Join(audioDir, id+".pcm")); err != nil || !bytes.Equal(got, want) {
			t.Errorf("raw PCM for %s after failed row delete = %q (%v), want restored source bytes", id, got, err)
		}
	}
}

func TestDeleteSessionRestoresEarlierRawPCMWhenALaterHideFails(t *testing.T) {
	store, audioDir, raw := newMergedDeleteStore(t)
	original := beginReservedErasing
	t.Cleanup(func() { beginReservedErasing = original })
	failedPath := filepath.Join(audioDir, "source.pcm")
	beginReservedErasing = func(reservation erase.Reservation) (erase.Pending, error) {
		if reservation.OriginalPath() == failedPath {
			return erase.Pending{}, errors.New("cannot hide source raw PCM")
		}
		return original(reservation)
	}

	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/merged", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete with later raw hide failure = %d %s, want 500", rr.Code, rr.Body.String())
	}
	for id, want := range raw {
		if got, err := os.ReadFile(filepath.Join(audioDir, id+".pcm")); err != nil || !bytes.Equal(got, want) {
			t.Errorf("raw PCM for %s after later hide failure = %q (%v), want restored bytes", id, got, err)
		}
	}
}

func TestDeleteSessionRefusesNestedMergeTarget(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"source", "middle-sibling", "outer-sibling"} {
		at := started.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MergeSessions("middle", []string{"source", "middle-sibling"}, started, started.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeSessions("outer", []string{"middle", "outer-sibling"}, started, started.Add(121*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/middle", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete of nested merge target = %d %s, want 409", rr.Code, rr.Body.String())
	}
	for _, id := range []string{"source", "middle-sibling", "middle", "outer", "outer-sibling"} {
		if _, err := store.GetSession(id); err != nil {
			t.Errorf("nested delete refusal removed %s: %v", id, err)
		}
	}
}

func TestDeleteSessionRefusesAnActiveMergedSource(t *testing.T) {
	store, audioDir, _ := newMergedDeleteStore(t)
	if _, err := store.DB().Exec(`UPDATE sessions SET status = ? WHERE id = 'source'`, storage.SessionActive); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, configStoreWithAudioDir(t, audioDir))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/merged", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete of merged session with active source = %d %s, want 409", rr.Code, rr.Body.String())
	}
	if sess, err := store.GetSession("source"); err != nil || sess.Status != storage.SessionActive {
		t.Fatalf("active source after refused delete = %+v (%v), want active", sess, err)
	}
}

func TestRetryRefinementWithoutLocalAudioStartsRestore(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementNoAudio},
		},
		segments: map[string][]transcribe.Segment{},
	}
	started := make(chan string, 1)
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		RetryRefinement: func(_ context.Context, sessionID string) error {
			started <- sessionID
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/retry-refinement", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("retry refinement without local audio = %d %s, want 202", response.Code, response.Body.String())
	}
	select {
	case sessionID := <-started:
		if sessionID != "s1" {
			t.Fatalf("restore refinement session = %q, want s1", sessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("retry refinement without local audio did not start restoration")
	}
}
