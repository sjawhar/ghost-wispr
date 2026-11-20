package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// newDispatchHTTP is the Concierge's HTTP client for Dispatch: the dispatch
// package's 15 s bound, with each call timed (timedCalls).
func newDispatchHTTP(logger *slog.Logger) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: timedCalls{msg: "dispatch call", route: dispatchRoute, next: http.DefaultTransport, logger: logger}}
}

// newEnvoyHTTP is the Envoy listener's HTTP client: the envoy package's 10 s
// bound, with the Concierge's registry reads and sends timed (timedCalls).
func newEnvoyHTTP(logger *slog.Logger) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: timedCalls{msg: "envoy call", route: envoyRoute, next: http.DefaultTransport, logger: logger}}
}

// timedCalls logs each call's route, status and time to the answer's
// headers, for the fast answer's 8 s bound (R6). It names the route only: a
// path holds an issue key heard in the room or a session id, and a search's
// query holds the room's words. A route named "" is not logged.
type timedCalls struct {
	msg    string
	route  func(path string) string
	next   http.RoundTripper
	logger *slog.Logger
}

func (t timedCalls) RoundTrip(request *http.Request) (*http.Response, error) {
	start := time.Now()
	response, err := t.next.RoundTrip(request)
	route := t.route(request.URL.Path)
	if route == "" {
		return response, err
	}
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	t.logger.Info(t.msg, "route", route, "status", status, "ms", time.Since(start).Milliseconds(), "failed", err != nil)
	return response, err
}

// envoyRoute names the listener's routes the Concierge waits on: a session's
// liveness (registry) and a message (send). The unit's own registration and
// close are not logged.
func envoyRoute(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/registry/"):
		return "registry"
	case path == "/v1/messages/send":
		return "send"
	}
	return ""
}

// dispatchRoute names a Dispatch API path by its route, as dispatch.APIError
// does: projects, search, search_keywords, issue, events, document, else
// other.
func dispatchRoute(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "v1" {
		return "other"
	}
	switch {
	case len(parts) == 3 && parts[2] == "projects":
		return "projects"
	case len(parts) == 3 && parts[2] == "search":
		return "search"
	case len(parts) == 4 && parts[2] == "search" && parts[3] == "results":
		return "search_keywords" // the keyword search (keywords_only)
	case parts[2] != "issues":
		return "other"
	case len(parts) == 4:
		return "issue"
	case len(parts) == 5 && parts[4] == "events":
		return "events"
	case len(parts) == 7 && parts[4] == "artifacts" && parts[6] == "text":
		return "document"
	}
	return "other"
}
