package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeJSONLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	line := strings.TrimSpace(raw)
	if line == "" {
		t.Fatal("expected non-empty JSON log line")
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("unmarshal JSON log line: %v", err)
	}

	return payload
}

func TestLogger_JSONOutputIncludesRequiredFields(t *testing.T) {
	var out bytes.Buffer
	logger := WithModule(New(&out, "debug"), "session")

	logger.Info("session ended", "session_id", "sess-123", "operation", "end_current_session")

	payload := decodeJSONLine(t, out.String())
	requiredKeys := []string{"timestamp", "level", "module", "session_id", "operation", "message"}
	for _, key := range requiredKeys {
		if _, ok := payload[key]; !ok {
			t.Fatalf("expected key %q in payload: %#v", key, payload)
		}
	}

	if payload["level"] != "INFO" {
		t.Fatalf("expected level INFO, got %v", payload["level"])
	}
	if payload["module"] != "session" {
		t.Fatalf("expected module session, got %v", payload["module"])
	}
	if payload["session_id"] != "sess-123" {
		t.Fatalf("expected session_id sess-123, got %v", payload["session_id"])
	}
	if payload["operation"] != "end_current_session" {
		t.Fatalf("expected operation end_current_session, got %v", payload["operation"])
	}
	if payload["message"] != "session ended" {
		t.Fatalf("expected message 'session ended', got %v", payload["message"])
	}
}

func TestLogger_LevelFiltering(t *testing.T) {
	var out bytes.Buffer
	logger := WithModule(New(&out, "error"), "server")

	logger.Warn("warn should be filtered", "operation", "startup")
	logger.Error("error should pass", "operation", "startup")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one log line, got %d: %q", len(lines), out.String())
	}

	payload := decodeJSONLine(t, lines[0])
	if payload["level"] != "ERROR" {
		t.Fatalf("expected level ERROR, got %v", payload["level"])
	}
	if payload["message"] != "error should pass" {
		t.Fatalf("expected error message, got %v", payload["message"])
	}
}

func TestLogger_ContextFieldPropagation(t *testing.T) {
	var out bytes.Buffer
	base := WithModule(New(&out, "info"), "gdrive")
	ctx := ContextWithLogger(context.Background(), base.With("session_id", "sess-ctx"))

	logger := FromContext(ctx, base)
	logger.Info("sync started", "operation", "sync_session")

	payload := decodeJSONLine(t, out.String())
	if payload["module"] != "gdrive" {
		t.Fatalf("expected module gdrive, got %v", payload["module"])
	}
	if payload["session_id"] != "sess-ctx" {
		t.Fatalf("expected session_id sess-ctx, got %v", payload["session_id"])
	}
	if payload["operation"] != "sync_session" {
		t.Fatalf("expected operation sync_session, got %v", payload["operation"])
	}
}

func TestRequestIDMiddleware_AddsRequestIDToContextHeaderAndLogs(t *testing.T) {
	var out bytes.Buffer
	base := WithModule(New(&out, "info"), "server")
	mw := RequestIDMiddleware(base, func() string { return "req-abc-123" })

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := FromContext(r.Context(), slog.Default())
		logger.Info("request handled", "operation", "http_request")
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get(RequestIDHeader); got != "req-abc-123" {
		t.Fatalf("expected %s header to be req-abc-123, got %q", RequestIDHeader, got)
	}

	payload := decodeJSONLine(t, out.String())
	if payload["request_id"] != "req-abc-123" {
		t.Fatalf("expected request_id req-abc-123, got %v", payload["request_id"])
	}
	if payload["module"] != "server" {
		t.Fatalf("expected module server, got %v", payload["module"])
	}
	if payload["operation"] != "http_request" {
		t.Fatalf("expected operation http_request, got %v", payload["operation"])
	}
}
