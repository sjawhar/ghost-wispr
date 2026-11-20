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
	logger := WithModule(New(&out, slog.LevelDebug), "session")

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
	logger := WithModule(New(&out, ParseLevel("error")), "server")

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
	base := WithModule(New(&out, slog.LevelInfo), "gdrive")
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

// WithModule on a logger that names a module names the new one in its place,
// with what was added between kept: a record holds one "module" key.
func TestWithModule_OnANamedLoggerReplacesTheModule(t *testing.T) {
	var out bytes.Buffer
	logger := WithModule(WithModule(New(&out, slog.LevelInfo), "server").With("request_id", "req-1"), "health")

	logger.Info("readiness check failed", "operation", "ready")

	if n := strings.Count(out.String(), `"module"`); n != 1 {
		t.Fatalf("record holds %d module keys, want 1: %s", n, out.String())
	}
	payload := decodeJSONLine(t, out.String())
	if payload["module"] != "health" {
		t.Fatalf("expected module health, got %v", payload["module"])
	}
	if payload["request_id"] != "req-1" {
		t.Fatalf("expected request_id req-1, got %v", payload["request_id"])
	}
	if payload["operation"] != "ready" {
		t.Fatalf("expected operation ready, got %v", payload["operation"])
	}
}

// A group opened on a logger that names a module leaves the module at the top
// of each record, outside the group.
func TestWithModule_ModuleStaysOutsideAGroup(t *testing.T) {
	var out bytes.Buffer
	logger := WithModule(New(&out, slog.LevelInfo), "server").WithGroup("request")

	logger.Info("request handled", "path", "/api/status")

	payload := decodeJSONLine(t, out.String())
	if payload["module"] != "server" {
		t.Fatalf("expected module server at the top level, got %v: %s", payload["module"], out.String())
	}
	group, _ := payload["request"].(map[string]any)
	if group["path"] != "/api/status" {
		t.Fatalf("expected request.path /api/status, got %v: %s", group["path"], out.String())
	}
	if _, ok := group["module"]; ok {
		t.Fatalf("module is inside the group: %s", out.String())
	}
}

// Naming another module on a logger with a group open keeps one "module" key
// in each record, the new one, and each bound attribute in its group; the
// logger it derived from keeps its own module.
func TestWithModule_RenamedInAGroupHoldsOneModule(t *testing.T) {
	var out bytes.Buffer
	base := WithModule(New(&out, slog.LevelInfo).With("root_id", "root-1"), "server").
		WithGroup("request").With("request_id", "req-1")
	WithModule(base, "health").Info("renamed logger", "operation", "ready")
	base.Info("original logger", "operation", "serve")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two records, got %d: %s", len(lines), out.String())
	}
	for i, want := range []struct{ module, operation string }{{"health", "ready"}, {"server", "serve"}} {
		line := lines[i]
		if n := strings.Count(line, `"module"`); n != 1 {
			t.Errorf("record holds %d module keys, want 1: %s", n, line)
		}
		if !strings.Contains(line, `"module":"`+want.module+`"`) {
			t.Errorf("record does not name module %q: %s", want.module, line)
		}
		payload := decodeJSONLine(t, line)
		if payload["root_id"] != "root-1" {
			t.Errorf("root attribute lost or grouped: %s", line)
		}
		request, _ := payload["request"].(map[string]any)
		if request["request_id"] != "req-1" || request["operation"] != want.operation {
			t.Errorf("grouped attributes lost or misplaced: %s", line)
		}
	}
}

// A bound value is rendered once, when it is bound, as a plain JSON logger
// renders it: the logger and the one that renames its module log the same
// value, a value's own MarshalJSON and a LogValuer each run once, and a map
// changed after it was bound shows as it was.
func TestWithModule_RenamingKeepsBoundValuesAsBound(t *testing.T) {
	var out bytes.Buffer
	marshals, evaluations := 0, 0
	counter := map[string]int{"counter": 1}
	logger := WithModule(New(&out, slog.LevelInfo), "server").
		With("state", counter, "marshaled", countingMarshaler{&marshals}, slog.Group("detail", slog.Any("value", logValueFunc(func() slog.Value {
			evaluations++
			return slog.IntValue(evaluations)
		}))))
	counter["counter"] = 2
	logger.Info("original logger")
	WithModule(logger, "health").Info("renamed logger")

	if marshals != 1 || evaluations != 1 {
		t.Errorf("binding ran MarshalJSON %d times and LogValue %d times, want once each", marshals, evaluations)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two records, got %d: %s", len(lines), out.String())
	}
	for _, line := range lines {
		payload := decodeJSONLine(t, line)
		state, _ := payload["state"].(map[string]any)
		detail, _ := payload["detail"].(map[string]any)
		if state["counter"] != float64(1) || payload["marshaled"] != float64(1) || detail["value"] != float64(1) {
			t.Errorf("a bound value is not as it was bound (state.counter 1, marshaled 1, detail.value 1): %s", line)
		}
	}
}

type logValueFunc func() slog.Value

func (f logValueFunc) LogValue() slog.Value { return f() }

// countingMarshaler renders as the number of times it was rendered.
type countingMarshaler struct{ calls *int }

func (m countingMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls++
	return json.Marshal(*m.calls)
}

func TestRequestIDMiddleware_AddsRequestIDToContextHeaderAndLogs(t *testing.T) {
	var out bytes.Buffer
	base := WithModule(New(&out, slog.LevelInfo), "server")
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
