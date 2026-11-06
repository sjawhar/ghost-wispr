package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

func TestGetLogs(t *testing.T) {
	now := time.Now()
	mockLogs := []logging.LogEntry{
		{Timestamp: now.Add(-2 * time.Minute), Level: "INFO", Message: "test info"},
		{Timestamp: now.Add(-1 * time.Minute), Level: "ERROR", Message: "test error"},
	}

	controls := &ControlHooks{
		GetLogs: func(level string, limit int, since time.Time) []logging.LogEntry {
			var result []logging.LogEntry
			for _, l := range mockLogs {
				if level != "" && level != "ALL" && l.Level != level {
					continue
				}
				if !since.IsZero() && l.Timestamp.Before(since) {
					continue
				}
				result = append(result, l)
			}
			if limit > 0 && len(result) > limit {
				result = result[:limit]
			}
			return result
		},
	}

	mux := http.NewServeMux()
	registerLogRoutes(mux, controls)

	tests := []struct {
		name           string
		query          string
		expectedStatus int
		expectedCount  int
	}{
		{"All logs", "", http.StatusOK, 2},
		{"Filter by level", "?level=ERROR", http.StatusOK, 1},
		{"Filter by limit", "?limit=1", http.StatusOK, 1},
		{"Filter by since", "?since=" + now.Add(-90*time.Second).Format(time.RFC3339), http.StatusOK, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/logs"+tt.query, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}

			if tt.expectedStatus == http.StatusOK {
				var logs []logging.LogEntry
				if err := json.NewDecoder(rr.Body).Decode(&logs); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if len(logs) != tt.expectedCount {
					t.Errorf("expected %d logs, got %d", tt.expectedCount, len(logs))
				}
			}
		})
	}
}

func TestRetryEndpoints(t *testing.T) {
	var summaryCalled, syncCalled, refinementCalled atomic.Bool

	controls := &ControlHooks{
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			summaryCalled.Store(true)
			return nil
		},
		RetrySync: func(ctx context.Context, sessionID string) error {
			syncCalled.Store(true)
			return nil
		},
		RetryRefinement: func(ctx context.Context, sessionID string) error {
			refinementCalled.Store(true)
			return nil
		},
	}

	mux := http.NewServeMux()
	registerAPIRoutes(mux, &apiStoreStub{}, controls, nil, nil, nil)

	tests := []struct {
		name     string
		endpoint string
		check    func() bool
	}{
		{"Retry Summary", "/api/sessions/test-session/retry-summary", summaryCalled.Load},
		{"Retry Sync", "/api/sessions/test-session/retry-sync", syncCalled.Load},
		{"Retry Refinement", "/api/sessions/test-session/retry-refinement", refinementCalled.Load},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summaryCalled.Store(false)
			syncCalled.Store(false)
			refinementCalled.Store(false)
			req := httptest.NewRequest(http.MethodPost, tt.endpoint, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("expected status 200, got %d", rr.Code)
			}

			// Give the goroutine a moment to run
			time.Sleep(10 * time.Millisecond)

			if !tt.check() {
				t.Errorf("expected %s to be called", tt.name)
			}
		})
	}
}

// Retry refinement on a session with no saved recording (refinement status
// no_audio, or audio that disk pressure removed) is refused with the reason,
// instead of reporting a retry that can only fail.
func TestRetryRefinementRefusesASessionWithNoRecording(t *testing.T) {
	cases := map[string]storage.Session{
		"no recording saved": {ID: "20261106140930", Status: storage.SessionEnded, RefinementStatus: storage.RefinementNoAudio},
		"audio removed to free space": {ID: "20261106140930", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted,
			AudioRemovedAt: "2026-11-07T09:30:00Z"},
	}
	for name := range cases {
		sess := cases[name]
		t.Run(name, func(t *testing.T) {
			var called atomic.Bool
			controls := &ControlHooks{RetryRefinement: func(context.Context, string) error {
				called.Store(true)
				return nil
			}}
			store := &apiStoreStub{sessions: map[string]storage.Session{sess.ID: sess}}
			mux := http.NewServeMux()
			registerAPIRoutes(mux, store, controls, nil, nil, nil)

			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/20261106140930/retry-refinement", nil))
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "no saved recording") {
				t.Fatalf("retry refinement with no recording = %d %s, want 409 saying there is no saved recording", rr.Code, rr.Body.String())
			}
			time.Sleep(10 * time.Millisecond)
			if called.Load() {
				t.Fatal("refinement retried for a session with no recording")
			}
		})
	}
}
