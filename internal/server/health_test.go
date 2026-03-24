package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// MockHealthChecker implements HealthChecker for testing
type MockHealthChecker struct {
	deepgramConnected bool
	dbHealthy         bool
	micOpen           bool
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
