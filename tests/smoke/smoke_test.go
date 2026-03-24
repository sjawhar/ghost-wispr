// Package smoke provides end-to-end smoke tests for Ghost Whisper.
// These tests exercise the full pipeline against a running instance.
//
// Prerequisites:
//   - Ghost Whisper running on GHOST_WISPR_SMOKE_URL (default: http://localhost:8080)
//   - Set GHOST_WISPR_SMOKE=true to enable (skipped otherwise)
//
// Run: go test ./tests/smoke/ -v -timeout 300s
package smoke

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func baseURL() string {
	if u := os.Getenv("GHOST_WISPR_SMOKE_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:8080"
}

func skipIfNotEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("GHOST_WISPR_SMOKE") != "true" {
		t.Skip("smoke tests disabled: set GHOST_WISPR_SMOKE=true to enable")
	}
}

func getJSON(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(baseURL() + path)
	if err != nil {
		t.Fatalf("GET %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	_ = json.Unmarshal(body, &result)
	return resp.StatusCode, result
}

func getJSONArray(t *testing.T, path string) (int, []any) {
	t.Helper()
	resp, err := http.Get(baseURL() + path)
	if err != nil {
		t.Fatalf("GET %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result []any
	_ = json.Unmarshal(body, &result)
	return resp.StatusCode, result
}

func postJSON(t *testing.T, path string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(baseURL()+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	_ = json.Unmarshal(respBody, &result)
	return resp.StatusCode, result
}

// Test 1: Health check - liveness
func TestSmoke_HealthLive(t *testing.T) {
	skipIfNotEnabled(t)

	code, body := getJSON(t, "/healthz/live")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if body["status"] != "alive" {
		t.Fatalf("expected status alive, got %v", body["status"])
	}
}

// Test 2: Health check - readiness
func TestSmoke_HealthReady(t *testing.T) {
	skipIfNotEnabled(t)

	code, body := getJSON(t, "/healthz/ready")
	// May be 200 or 503 depending on component state
	if code != 200 && code != 503 {
		t.Fatalf("expected 200 or 503, got %d", code)
	}
	// Should have component fields
	for _, field := range []string{"deepgram", "db", "mic"} {
		if _, ok := body[field]; !ok {
			t.Errorf("missing field %q in readiness response", field)
		}
	}
}

// Test 3: Version endpoint
func TestSmoke_Version(t *testing.T) {
	skipIfNotEnabled(t)

	code, body := getJSON(t, "/api/version")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if body["version"] == nil {
		t.Fatal("missing version field")
	}
}

// Test 4: Session list
func TestSmoke_SessionList(t *testing.T) {
	skipIfNotEnabled(t)

	code, _ := getJSONArray(t, "/api/dates")
	if code != 200 {
		t.Fatalf("expected 200 for dates, got %d", code)
	}
}

// Test 5: Manual session start/stop
func TestSmoke_ManualSessionStartStop(t *testing.T) {
	skipIfNotEnabled(t)

	// Start a session
	code, body := postJSON(t, "/api/sessions/start", `{"title_hint": "Smoke Test Session"}`)
	if code != 200 {
		t.Fatalf("expected 200 for session start, got %d: %v", code, body)
	}
	sessionID, ok := body["session_id"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("expected session_id in response, got %v", body)
	}

	// Wait a moment
	time.Sleep(500 * time.Millisecond)

	// Stop the session
	code, body = postJSON(t, "/api/sessions/current/stop", "")
	if code != 200 {
		t.Fatalf("expected 200 for session stop, got %d: %v", code, body)
	}

	// Verify session exists
	time.Sleep(500 * time.Millisecond)
	code, sessBody := getJSON(t, fmt.Sprintf("/api/sessions/%s", sessionID))
	if code != 200 {
		t.Fatalf("expected 200 for session detail, got %d", code)
	}
	if sessBody["id"] != sessionID {
		t.Fatalf("expected session ID %s, got %v", sessionID, sessBody["id"])
	}
}

// Test 6: Search endpoint
func TestSmoke_Search(t *testing.T) {
	skipIfNotEnabled(t)

	code, results := getJSONArray(t, "/api/search?q=test")
	if code != 200 {
		t.Fatalf("expected 200 for search, got %d", code)
	}
	// Results may be empty if no matching sessions
	_ = results
}

// Test 7: Search with empty query
func TestSmoke_SearchEmpty(t *testing.T) {
	skipIfNotEnabled(t)

	code, results := getJSONArray(t, "/api/search?q=")
	if code != 200 {
		t.Fatalf("expected 200 for empty search, got %d", code)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty results for empty query, got %d", len(results))
	}
}

// Test 8: Web UI serves index.html
func TestSmoke_WebUI(t *testing.T) {
	skipIfNotEnabled(t)

	resp, err := http.Get(baseURL() + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for web UI, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<!DOCTYPE html>") && !strings.Contains(string(body), "<html") {
		t.Fatal("expected HTML response for web UI")
	}
}

// Test 9: Mic diagnostic endpoint
func TestSmoke_MicDiagnostic(t *testing.T) {
	skipIfNotEnabled(t)

	code, body := postJSON(t, "/api/diagnostic/mic", "")
	// May be 200 (mic available) or 503 (mic unavailable)
	if code != 200 && code != 500 {
		t.Fatalf("expected 200 or 500 for mic diagnostic, got %d: %v", code, body)
	}
}

// Test 10: Logs endpoint
func TestSmoke_Logs(t *testing.T) {
	skipIfNotEnabled(t)

	code, _ := getJSONArray(t, "/api/logs?limit=10")
	if code != 200 {
		t.Fatalf("expected 200 for logs, got %d", code)
	}
}

// Test 11: Error propagation - fault injection (requires test mode)
func TestSmoke_FaultInjection(t *testing.T) {
	skipIfNotEnabled(t)

	if os.Getenv("GHOST_WISPR_TEST_MODE") != "true" {
		t.Skip("fault injection requires GHOST_WISPR_TEST_MODE=true")
	}

	code, body := postJSON(t, "/api/test/fault/deepgram-disconnect", "")
	if code == 403 {
		t.Skip("test mode not enabled on server")
	}
	if code != 200 && code != 503 {
		t.Fatalf("expected 200 or 503 for fault injection, got %d: %v", code, body)
	}
}

// Test 12: No session has empty title
func TestSmoke_NoEmptyTitles(t *testing.T) {
	skipIfNotEnabled(t)

	code, dates := getJSONArray(t, "/api/dates")
	if code != 200 {
		t.Fatalf("expected 200 for dates, got %d", code)
	}

	for _, d := range dates {
		date, ok := d.(string)
		if !ok {
			continue
		}
		code, sessions := getJSONArray(t, fmt.Sprintf("/api/sessions?date=%s", date))
		if code != 200 {
			continue
		}
		for _, s := range sessions {
			sess, ok := s.(map[string]any)
			if !ok {
				continue
			}
			title, _ := sess["title"].(string)
			if title == "" {
				t.Errorf("session %v has empty title", sess["id"])
			}
		}
	}
}
