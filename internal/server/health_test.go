package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// MockHealthChecker implements HealthChecker for testing
type MockHealthChecker struct {
	deepgramConfigured bool
	deepgramConnected  bool
	dbHealthy          bool
	micOpen            bool
	micHold            string
	llmStatus          string
}

func (m *MockHealthChecker) IsDeepgramConfigured() bool {
	return m.deepgramConfigured
}

func (m *MockHealthChecker) IsDeepgramConnected() bool {
	return m.deepgramConnected
}

func (m *MockHealthChecker) IsDBHealthy(ctx context.Context) bool {
	return m.dbHealthy
}

func (m *MockHealthChecker) IsMicOpen() bool {
	return m.micOpen
}

func (m *MockHealthChecker) MicHold() string {
	return m.micHold
}

func (m *MockHealthChecker) IsLLMHealthy() string {
	if m.llmStatus == "" {
		return llmStatusUnchecked
	}
	return m.llmStatus
}

func TestHealthzLiveAlwaysReturns200(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: false,
		dbHealthy:         false,
		micOpen:           false,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzLive(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/live", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if status, ok := resp["status"].(string); !ok || status != "alive" {
		t.Errorf("expected status 'alive', got %v", resp["status"])
	}
}

func TestHealthzReadyReturns200WhenAllHealthy(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Deepgram != "connected" {
		t.Errorf("expected deepgram 'connected', got %q", resp.Deepgram)
	}
	if resp.DB != "ok" {
		t.Errorf("expected db 'ok', got %q", resp.DB)
	}
	if resp.Mic != "open" {
		t.Errorf("expected mic 'open', got %q", resp.Mic)
	}
	if resp.LLM != llmStatusUnchecked {
		t.Errorf("expected llm 'unchecked', got %q", resp.LLM)
	}
}

func TestHealthzReadyReturns200WhenLLMUnchecked(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
		llmStatus:         llmStatusUnchecked,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.LLM != llmStatusUnchecked {
		t.Errorf("expected llm 'unchecked', got %q", resp.LLM)
	}
}

func TestHealthzReadyReturns503WhenLLMUnhealthy(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
		llmStatus:         "error",
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.LLM != "error" {
		t.Errorf("expected llm 'error', got %q", resp.LLM)
	}
}

func TestHealthzReadyReturns503WhenDeepgramDisconnected(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: false,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Deepgram != "disconnected" {
		t.Errorf("expected deepgram 'disconnected', got %q", resp.Deepgram)
	}
}

func TestHealthzReadyReturns503WhenDBUnhealthy(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         false,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.DB != "error" {
		t.Errorf("expected db 'error', got %q", resp.DB)
	}
}

func TestHealthzReadyReturns503WhenMicClosed(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           false,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Mic != "closed" {
		t.Errorf("expected mic 'closed', got %q", resp.Mic)
	}
}

// A paused capture stops the mic on purpose, and a resume waits a moment for
// the paused session to be saved: readiness reports either state and stays
// 200. A resume that has not brought the mic back past the stall limit is a
// fault: 503 with mic "stalled", never "paused".
func TestHealthzReadyReportsAHeldMic(t *testing.T) {
	for _, tc := range []struct {
		hold     string
		wantCode int
		wantMic  string
	}{
		{hold: "paused", wantCode: http.StatusOK, wantMic: "paused"},
		{hold: "resuming", wantCode: http.StatusOK, wantMic: "resuming"},
		{hold: "stalled", wantCode: http.StatusServiceUnavailable, wantMic: "stalled"},
		{hold: "", wantCode: http.StatusServiceUnavailable, wantMic: "closed"},
	} {
		checker := &MockHealthChecker{deepgramConnected: true, dbHealthy: true, micHold: tc.hold}

		w := httptest.NewRecorder()
		handleHealthzReady(w, httptest.NewRequest("GET", "/healthz/ready", nil), checker)

		if w.Code != tc.wantCode {
			t.Errorf("hold %q: status %d, want %d", tc.hold, w.Code, tc.wantCode)
		}
		var resp HealthzReadyResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}
		if resp.Mic != tc.wantMic {
			t.Errorf("hold %q: mic %q, want %q", tc.hold, resp.Mic, tc.wantMic)
		}
	}
}

// TestHealthzReadyExcusesDeepgramOnlyWhileCaptureIsHeld fails if readiness
// answers 503 for a paused unit only because Deepgram is not connected: a unit
// that starts paused connects to Deepgram only when it resumes, and a paused
// unit sends Deepgram nothing. Deepgram then reports the hold and readiness
// stays 200. Once audio flows (no hold), or past the stall limit, or with no
// Deepgram configured at all, Deepgram not connected is a fault: 503.
func TestHealthzReadyExcusesDeepgramOnlyWhileCaptureIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name         string
		configured   bool
		connected    bool
		hold         string
		micOpen      bool
		wantCode     int
		wantDeepgram string
	}{
		{name: "paused start", configured: true, hold: "paused", wantCode: http.StatusOK, wantDeepgram: "paused"},
		{name: "resuming", configured: true, hold: "resuming", wantCode: http.StatusOK, wantDeepgram: "resuming"},
		{name: "paused and connected", configured: true, connected: true, hold: "paused", wantCode: http.StatusOK, wantDeepgram: "connected"},
		{name: "after resume", configured: true, micOpen: true, wantCode: http.StatusServiceUnavailable, wantDeepgram: "disconnected"},
		{name: "stalled", configured: true, hold: "stalled", wantCode: http.StatusServiceUnavailable, wantDeepgram: "disconnected"},
		{name: "paused with no Deepgram", hold: "paused", wantCode: http.StatusServiceUnavailable, wantDeepgram: "disconnected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := &MockHealthChecker{deepgramConfigured: tc.configured, deepgramConnected: tc.connected, dbHealthy: true, micOpen: tc.micOpen, micHold: tc.hold}
			w := httptest.NewRecorder()
			handleHealthzReady(w, httptest.NewRequest("GET", "/healthz/ready", nil), checker)
			var resp HealthzReadyResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			if w.Code != tc.wantCode || resp.Deepgram != tc.wantDeepgram {
				t.Fatalf("readiness = %d with deepgram %q, want %d with %q (body %s)", w.Code, resp.Deepgram, tc.wantCode, tc.wantDeepgram, w.Body.String())
			}
		})
	}
}

type holdState string

func (h holdState) MicHold() string { return string(h) }

func TestDefaultHealthCheckerReportsMicHold(t *testing.T) {
	if got := NewDefaultHealthChecker(nil, nil, nil, holdState("stalled"), nil).MicHold(); got != "stalled" {
		t.Fatalf("MicHold = %q, want the capture's %q", got, "stalled")
	}
	if got := NewDefaultHealthChecker(nil, nil, nil, nil, nil).MicHold(); got != "" {
		t.Fatalf("MicHold with no capture switch = %q, want none", got)
	}
}

// While a long query holds the database's one connection (the state a capture
// stalled on the database is in), readiness still answers, with 503 and the
// database as the fault, instead of waiting for the connection.
func TestHealthzReadyAnswersWhileTheDatabaseIsBusy(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	conn, err := store.DB().Conn(context.Background())
	if err != nil {
		t.Fatalf("take the database connection: %v", err)
	}
	defer func() { _ = conn.Close() }()
	checker := NewDefaultHealthChecker(nil, store, nil, holdState("stalled"), nil)

	w := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		handleHealthzReady(w, httptest.NewRequest("GET", "/healthz/ready", nil), checker)
	}()
	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("readiness still waiting for the database after 10s")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.DB != storage.ComponentStatusError {
		t.Errorf("db %q, want %q", resp.DB, storage.ComponentStatusError)
	}
}

func TestDefaultHealthCheckerHandlesTypedNilPointers(t *testing.T) {
	var resilient *transcribe.ResilientClient
	var mic *audio.Mic

	checker := NewDefaultHealthChecker(resilient, nil, mic, nil, nil)

	if checker.IsDeepgramConnected() {
		t.Fatal("expected typed-nil resilient client to be treated as disconnected")
	}
	if checker.IsDeepgramConfigured() {
		t.Fatal("expected typed-nil resilient client to be treated as no Deepgram configured")
	}
	if checker.IsMicOpen() {
		t.Fatal("expected typed-nil mic to be treated as closed")
	}
	if checker.IsLLMHealthy() != llmStatusUnchecked {
		t.Fatalf("expected unchecked llm status for nil tracker")
	}
}

type connectedState bool

func (c connectedState) IsConnected() bool { return bool(c) }

// TestDefaultHealthCheckerReportsDeepgramConfigured fails if the checker does
// not tell a Deepgram client source that is not connected (a paused start
// connects only on resume) from none at all (no API key).
func TestDefaultHealthCheckerReportsDeepgramConfigured(t *testing.T) {
	checker := NewDefaultHealthChecker(connectedState(false), nil, nil, holdState("paused"), nil)
	if !checker.IsDeepgramConfigured() || checker.IsDeepgramConnected() {
		t.Fatalf("with a client source that is not connected: configured=%t connected=%t, want true and false", checker.IsDeepgramConfigured(), checker.IsDeepgramConnected())
	}
	if NewDefaultHealthChecker(nil, nil, nil, holdState("paused"), nil).IsDeepgramConfigured() {
		t.Fatal("with no Deepgram client source: configured = true, want false")
	}
}

func TestHealthzReadyReturns503WhenMultipleComponentsUnhealthy(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: false,
		dbHealthy:         false,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Deepgram != "disconnected" {
		t.Errorf("expected deepgram 'disconnected', got %q", resp.Deepgram)
	}
	if resp.DB != "error" {
		t.Errorf("expected db 'error', got %q", resp.DB)
	}
}

func TestHealthzLiveContentType(t *testing.T) {
	checker := &MockHealthChecker{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzLive(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/live", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

func TestHealthzReadyContentType(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

func TestHealthzReadyJSONStructure(t *testing.T) {
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	// Verify all fields are present
	if resp.Deepgram == "" {
		t.Error("deepgram field is empty")
	}
	if resp.DB == "" {
		t.Error("db field is empty")
	}
	if resp.Mic == "" {
		t.Error("mic field is empty")
	}
	if resp.LLM == "" {
		t.Error("llm field is empty")
	}
}

func TestHealthzLiveWithLogging(t *testing.T) {
	// Test that health check works with structured logging
	buf := &bytes.Buffer{}
	logger := logging.New(buf, "info")

	checker := &MockHealthChecker{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.ContextWithLogger(r.Context(), logger)
		handleHealthzLive(w, r.WithContext(ctx), checker)
	})

	req := httptest.NewRequest("GET", "/healthz/live", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestHealthzReadyWithLogging(t *testing.T) {
	// Test that health check works with structured logging
	buf := &bytes.Buffer{}
	logger := logging.New(buf, "info")

	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.ContextWithLogger(r.Context(), logger)
		handleHealthzReady(w, r.WithContext(ctx), checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestHealthzReadyConcurrentAccess(t *testing.T) {
	// Test that health check is thread-safe
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/healthz/ready", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected status 200, got %d", w.Code)
			}
		}()
	}
	wg.Wait()
}

func TestHealthzLiveWithResilientClient(t *testing.T) {
	// Test integration with actual ResilientClient state
	checker := &MockHealthChecker{
		deepgramConnected: true,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzLive(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/live", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("liveness should always return 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if status, ok := resp["status"].(string); !ok || status != "alive" {
		t.Errorf("expected status 'alive', got %v", resp["status"])
	}
}

func TestHealthzReadyWithResilientClientDisconnected(t *testing.T) {
	// Test that readiness reflects ResilientClient disconnected state
	checker := &MockHealthChecker{
		deepgramConnected: false,
		dbHealthy:         true,
		micOpen:           true,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleHealthzReady(w, r, checker)
	})

	req := httptest.NewRequest("GET", "/healthz/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness should return 503 when Deepgram disconnected, got %d", w.Code)
	}

	var resp HealthzReadyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Deepgram != "disconnected" {
		t.Errorf("expected deepgram 'disconnected', got %q", resp.Deepgram)
	}
}

func TestLLMHealthTrackerTransitions(t *testing.T) {
	tracker := NewLLMHealthTracker()

	if got := tracker.Status(); got != llmStatusUnchecked {
		t.Fatalf("expected initial status %q, got %q", llmStatusUnchecked, got)
	}
	if !tracker.IsHealthy() {
		t.Fatal("expected unchecked tracker to be healthy")
	}

	tracker.SetError("timeout")
	if got := tracker.Status(); got != "error" {
		t.Fatalf("expected error status, got %q", got)
	}
	if tracker.IsHealthy() {
		t.Fatal("expected error tracker to be unhealthy")
	}

	tracker.SetOK()
	if got := tracker.Status(); got != "ok" {
		t.Fatalf("expected ok status, got %q", got)
	}
	if !tracker.IsHealthy() {
		t.Fatal("expected ok tracker to be healthy")
	}
}
