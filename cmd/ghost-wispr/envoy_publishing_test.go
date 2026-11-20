package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// envoyHub stands in for the Envoy hub's listener, POST /v1/messages/publish.
// Like the hub, whose stream stores a publish whose idempotency key it already
// holds no second time (legion packages/envoy/internal/bus/nats.go,
// publishJetStream), it counts each idempotency key once: Ghost Wispr sends an
// event at least once, and every send of one event carries the same key.
type envoyHub struct {
	*httptest.Server
	mu         sync.Mutex
	posted     int
	keys       map[string]bool
	authorized int
	hold, held chan struct{} // see holdFirstPost
}

func newEnvoyHub(t *testing.T) *envoyHub {
	t.Helper()
	h := &envoyHub{keys: map[string]bool{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var body struct {
			Topic          string `json:"topic"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Topic == "" || body.IdempotencyKey == "" {
			http.Error(w, `{"error":"invalid publish"}`, http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.posted++
		first := h.posted == 1
		h.keys[body.IdempotencyKey] = true
		if r.Header.Get("Authorization") != "" {
			h.authorized++
		}
		hold, held := h.hold, h.held
		count := h.posted
		h.mu.Unlock()
		if first && hold != nil {
			close(held)
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"event_id":"evt-%d"}`, count)
	}))
	t.Cleanup(h.Close)
	return h
}

// stored is the number of events the hub stored: one per idempotency key.
func (h *envoyHub) stored() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.keys)
}

// withCredential is the number of publishes that carried an Authorization
// header.
func (h *envoyHub) withCredential() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authorized
}

// posts is the number of publishes the hub answered.
func (h *envoyHub) posts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.posted
}

// holdFirstPost makes the hub hold the first publish, unanswered, until
// release runs. arrived is closed once that publish has come. Call it before
// anything publishes.
func (h *envoyHub) holdFirstPost(t *testing.T) (arrived <-chan struct{}, release func()) {
	t.Helper()
	h.mu.Lock()
	h.hold, h.held = make(chan struct{}), make(chan struct{})
	hold, held := h.hold, h.held
	h.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release) // before the server's Close, which waits for the held publish
	return held, release
}

func testEnvoyPublishing(t *testing.T) (*envoyPublishing, *warningsRegistry) {
	t.Helper()
	store, manager := storeSessions(t)
	warnings := newWarningsRegistry()
	return &envoyPublishing{store: store, manager: manager, warnings: warnings}, warnings
}

func envoyConfig(hubURL string) *config.Config {
	return &config.Config{EnvoyEnabled: true, EnvoyHubURL: hubURL}
}

// TestEnvoyEventsReachTheHubWithNoCredential fails if a recorder configured
// with envoy_hub_url does not publish its events there, or sends any
// credential: the hub takes a tailnet client's publish with none.
func TestEnvoyEventsReachTheHubWithNoCredential(t *testing.T) {
	hub := newEnvoyHub(t)
	publishing, warnings := testEnvoyPublishing(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishing.configure(ctx, envoyConfig(hub.URL), discardLogger)
	if err := publishing.store.CreateSession("session-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := publishing.current.Load().PublishSessionStarted(ctx, "session-1"); err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	if hub.stored() != 1 {
		t.Fatalf("hub stored %d events, want 1", hub.stored())
	}
	if n := hub.withCredential(); n != 0 {
		t.Fatalf("%d publishes carried an Authorization header, want none", n)
	}
	if list := warnings.List(); len(list) != 0 {
		t.Fatalf("warnings = %q, want none", list)
	}
}

// TestASweepDuringAPublishSendsTheEventOnce fails if a session_started that a
// sweep publishes while the direct publish of it is still waiting reaches
// Envoy as two events. The event stays pending until its publish returns, so
// a sweep in between, such as the one configure starts, publishes it again.
// The control checks that the sweep did publish it a second time.
func TestASweepDuringAPublishSendsTheEventOnce(t *testing.T) {
	hub := newEnvoyHub(t)
	arrived, release := hub.holdFirstPost(t)
	publishing, _ := testEnvoyPublishing(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishing.configure(ctx, envoyConfig(hub.URL), discardLogger)
	if err := publishing.store.CreateSession("session-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() { published <- publishing.current.Load().PublishSessionStarted(ctx, "session-1") }()
	select {
	case <-arrived:
	case <-time.After(testWait):
		t.Fatal("control: no session_started publish came")
	}
	publishing.sweep(ctx) // the event is still pending: its first publish has not returned
	release()
	if err := <-published; err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	if posts := hub.posts(); posts < 2 {
		t.Fatalf("control: the session_started was published %d times, want a second publish from the sweep", posts)
	}
	if got := hub.stored(); got != 1 {
		t.Fatalf("hub stored %d events, want 1", got)
	}
}

// TestEnvoyWithNoHubURLSendsNothing fails if Envoy enabled with no
// envoy_hub_url builds a publisher, or if turning Envoy off leaves the one a
// settings change built in place.
func TestEnvoyWithNoHubURLSendsNothing(t *testing.T) {
	hub := newEnvoyHub(t)
	publishing, _ := testEnvoyPublishing(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publishing.configure(ctx, envoyConfig(hub.URL), discardLogger)
	if publishing.current.Load() == nil {
		t.Fatal("control: a config with envoy_hub_url built no publisher")
	}
	publishing.configure(ctx, &config.Config{EnvoyEnabled: true}, discardLogger)
	if publishing.current.Load() != nil {
		t.Fatal("a config with no envoy_hub_url kept an Envoy publisher")
	}
	publishing.configure(ctx, envoyConfig(hub.URL), discardLogger)
	publishing.configure(ctx, &config.Config{EnvoyHubURL: hub.URL}, discardLogger)
	if publishing.current.Load() != nil {
		t.Fatal("turning Envoy off kept an Envoy publisher")
	}
	if hub.posts() != 0 {
		t.Fatalf("hub received %d publishes with nothing to send, want none", hub.posts())
	}
}

// TestANonHTTPHubURLWarnsAndSendsNothing fails if a hub URL the listener's
// HTTP publish route cannot answer, such as the hub's NATS address, builds a
// publisher that reports itself enabled while every send fails, instead of a
// /api/status warning.
func TestANonHTTPHubURLWarnsAndSendsNothing(t *testing.T) {
	publishing, warnings := testEnvoyPublishing(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publishing.configure(ctx, envoyConfig("nats://hub.example.test:4222"), discardLogger)
	if publishing.current.Load() != nil {
		t.Fatal("a NATS hub URL built an Envoy publisher")
	}
	list := warnings.List()
	if len(list) != 1 || !strings.Contains(list[0], "nats://hub.example.test:4222") {
		t.Fatalf("warnings = %q, want one naming the URL", list)
	}

	hub := newEnvoyHub(t)
	publishing.configure(ctx, envoyConfig(hub.URL), discardLogger)
	if publishing.current.Load() == nil || len(warnings.List()) != 0 {
		t.Fatalf("control: an HTTP hub URL left publisher %v and warnings %q", publishing.current.Load(), warnings.List())
	}
}
