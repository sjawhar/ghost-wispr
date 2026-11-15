package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		status   int
		check    func() bool
	}{
		{"Retry Summary", "/api/sessions/test-session/retry-summary", http.StatusOK, summaryCalled.Load},
		{"Retry Sync", "/api/sessions/test-session/retry-sync", http.StatusOK, syncCalled.Load},
		{"Retry Refinement", "/api/sessions/test-session/retry-refinement", http.StatusAccepted, refinementCalled.Load},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summaryCalled.Store(false)
			syncCalled.Store(false)
			refinementCalled.Store(false)
			req := httptest.NewRequest(http.MethodPost, tt.endpoint, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)

			if rr.Code != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, rr.Code)
			}

			// Give the goroutine a moment to run
			time.Sleep(10 * time.Millisecond)

			if !tt.check() {
				t.Errorf("expected %s to be called", tt.name)
			}
		})
	}
}

// Retry sync answers with the truth: when no sync can start for the session
// (sync is off, or its state refuses one, such as a Drive folder deleted by
// hand), it says so instead of 200 {"triggered":true} for a retry that did
// nothing.
func TestRetrySyncSaysWhenNoSyncCanStart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantCode  int
		wantError string // "" for none
	}{
		{name: "started", wantCode: http.StatusOK},
		{name: "state refuses a sync", err: errors.New("cannot sync session test-session now: invalid sync state transition: REMOTE_DELETED -> SYNCING"),
			wantCode: http.StatusConflict, wantError: "REMOTE_DELETED -> SYNCING"},
		{name: "sync off", err: ErrSyncNotConfigured, wantCode: http.StatusServiceUnavailable, wantError: ErrSyncNotConfigured.Error()},
		// Review of #39, round 3, note 9: Drive not answering whether a
		// deleted session's folder is back is no conflict with the session:
		// the retry may work once Drive answers.
		{name: "Drive not answering", err: fmt.Errorf("%w: %w", ErrDriveUnavailable,
			errors.New("cannot sync session test-session now: its Drive folder was deleted, and Drive did not say whether it is back: googleapi: Error 503")),
			wantCode: http.StatusServiceUnavailable, wantError: "Drive did not say whether it is back"},
		{name: "no such session", err: fmt.Errorf("get session test-session: query session test-session: %w", sql.ErrNoRows),
			wantCode: http.StatusNotFound, wantError: "no rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controls := &ControlHooks{RetrySync: func(context.Context, string) error { return tc.err }}
			mux := http.NewServeMux()
			registerAPIRoutes(mux, &apiStoreStub{}, controls, nil, nil, nil)

			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/test-session/retry-sync", nil))
			var body struct {
				Triggered bool   `json:"triggered"`
				Error     string `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rr.Body.String(), err)
			}
			if rr.Code != tc.wantCode || body.Triggered != (tc.err == nil) || !strings.Contains(body.Error, tc.wantError) {
				t.Errorf("retry sync = %d %s, want %d saying %q", rr.Code, rr.Body.String(), tc.wantCode, tc.wantError)
			}
		})
	}
}

// Retry refinement starts Drive restoration when a session has no local
// recording, including one GC removed to free space.
func TestRetryRefinementStartsRestoreForASessionWithNoRecording(t *testing.T) {
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
			if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), "triggered") {
				t.Fatalf("retry refinement with no recording = %d %s, want 202 and a restore retry", rr.Code, rr.Body.String())
			}
			time.Sleep(10 * time.Millisecond)
			if !called.Load() {
				t.Fatal("refinement restoration did not start for a session with no recording")
			}
		})
	}
}
