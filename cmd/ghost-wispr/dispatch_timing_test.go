package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// TestDispatchCallsAreTimedByRouteOnly fails if a Dispatch call of the
// Concierge's client is not logged with its route and time, or the log holds
// the issue key or the search's words, which are the room's.
func TestDispatchCallsAreTimedByRouteOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			_, _ = w.Write([]byte(`{"issue":{"key":"ABC-12","title":"t","status":"in_progress"}}`))
		}
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client, err := dispatch.NewClient(server.URL, strings.Repeat("k", 8), newDispatchHTTP(logger))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), "purple elephant budget", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Issue(context.Background(), "ABC-12"); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	for _, want := range []string{`msg="dispatch call" route=search status=200`, `msg="dispatch call" route=issue status=200`} {
		if !strings.Contains(got, want) {
			t.Errorf("the log lacks %q:\n%s", want, got)
		}
	}
	for _, room := range []string{"purple", "elephant", "ABC-12", "abc-12"} {
		if strings.Contains(got, room) {
			t.Errorf("the log holds %q, the room's words:\n%s", room, got)
		}
	}
}

// TestDispatchRoutes fails if a path is named by anything but its route.
func TestDispatchRoutes(t *testing.T) {
	for path, want := range map[string]string{
		"/api/v1/projects":                       "projects",
		"/api/v1/search":                         "search",
		"/api/v1/search/results":                 "search_keywords",
		"/api/v1/issues/ABC-12":                  "issue",
		"/api/v1/issues/ABC-12/events":           "events",
		"/api/v1/issues/ABC-12/artifacts/s/text": "document",
		"/api/v1/issues/ABC-12/claims":           "other",
		"/health":                                "other",
	} {
		if got := dispatchRoute(path); got != want {
			t.Errorf("dispatchRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestEnvoyRoutes fails if an Envoy call the Concierge waits on is not named,
// or the unit's own registration is logged every 90 s.
func TestEnvoyRoutes(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/registry/agent-7":    "registry",
		"/v1/messages/send":       "send",
		"/v1/interests/subscribe": "",
		"/v1/sessions/unit-1":     "",
	} {
		if got := envoyRoute(path); got != want {
			t.Errorf("envoyRoute(%q) = %q, want %q", path, got, want)
		}
	}
}
