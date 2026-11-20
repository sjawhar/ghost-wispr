package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// No record the server writes holds a key twice. The static assets check, the
// health checks and the WebSocket route log with loggers that already name a
// module, and naming one again must not add a second "module" key.
func TestNoServerLogRecordRepeatsAKey(t *testing.T) {
	var out bytes.Buffer
	logger := logging.New(&out, slog.LevelDebug)
	hub := NewHub(logger)

	// Built without index.html, the handler logs why and panics.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a handler built without index.html did not panic")
			}
		}()
		_, _ = HandlerWithLogger(fstest.MapFS{}, hub, nil, &ControlHooks{}, "", nil, logger, nil)
	}()

	checker := &MockHealthChecker{}
	h, err := HandlerWithLogger(testStaticFS(t), hub, nil, &ControlHooks{}, "", checker, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(r *http.Request) { h.ServeHTTP(httptest.NewRecorder(), r) }
	serve(httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
	*checker = MockHealthChecker{deepgramConfigured: true, deepgramConnected: true, dbHealthy: true, micOpen: true}
	serve(httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
	serve(httptest.NewRequest(http.MethodGet, "/ws", nil)) // no upgrade headers
	foreign := httptest.NewRequest(http.MethodPost, "/api/pause", nil)
	foreign.Header.Set("Origin", "https://evil.example")
	serve(foreign)
	hub.BroadcastVoiceEvent("no_such_event", "")

	// Each message is a record the check must read, so a check that read none
	// of them cannot pass, and the module that record names.
	modules := map[string]string{
		"failed to read index.html from static assets": "server",
		"readiness check failed":                       "health",
		"readiness check passed":                       "health",
		"websocket upgrade failed":                     "server",
		"refused a request from another origin":        "server",
		"dropped an unknown voice event":               "server",
	}
	read := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var record struct {
			Message string `json:"message"`
			Module  string `json:"module"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("a record is not JSON: %v: %s", err, line)
		}
		if want, ok := modules[record.Message]; ok {
			read[record.Message] = true
			if record.Module != want {
				t.Errorf("record %q names module %q, want %q: %s", record.Message, record.Module, want, line)
			}
		}
		if keys := repeatedKeys(t, line); len(keys) > 0 {
			t.Errorf("record %q holds %q more than once: %s", record.Message, keys, line)
		}
	}
	for message := range modules {
		if !read[message] {
			t.Errorf("the server wrote no record %q, so the check did not read it", message)
		}
	}
}

// repeatedKeys lists each key that an object in the JSON value line holds
// more than once, at any depth.
func repeatedKeys(t *testing.T, line []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	token := func() json.Token {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read %s: %v", line, err)
		}
		return tok
	}
	var repeated []string
	var value func()
	value = func() {
		switch token() {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				key, _ := token().(string)
				if seen[key] {
					repeated = append(repeated, key)
				}
				seen[key] = true
				value()
			}
			token() // }
		case json.Delim('['):
			for dec.More() {
				value()
			}
			token() // ]
		}
	}
	value()
	return repeated
}
