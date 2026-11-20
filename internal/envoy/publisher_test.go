package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type storeStub struct {
	sessions map[string]storage.Session
	statuses map[string]string
	// afterStatus runs after each recorded status change, so a test can move
	// the session on (a regeneration) between two of the publisher's marks.
	afterStatus func(status string)
}

func (s *storeStub) GetSession(id string) (storage.Session, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return storage.Session{}, errors.New("not found")
	}
	return sess, nil
}

func (s *storeStub) UpdateEnvoyPublishStatusAtVersion(sessionID string, summaryVersion int, status string) (bool, error) {
	sess, ok := s.sessions[sessionID]
	if !ok || sess.SummaryVersion != summaryVersion {
		return false, nil
	}
	if s.statuses == nil {
		s.statuses = make(map[string]string)
	}
	s.statuses[sessionID] = status
	if s.afterStatus != nil {
		s.afterStatus(status)
	}
	return true, nil
}

func (*storeStub) EnqueueEnvoyLifecycleEvent(string, string) error { return nil }

func (*storeStub) PendingEnvoyLifecycleEvents() ([]storage.EnvoyLifecycleEvent, error) {
	return nil, nil
}

func (*storeStub) MarkEnvoyLifecycleEventDelivered(string, string) error { return nil }

func completedSession(version int) *storeStub {
	endedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	return &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			StartedAt:      endedAt.Add(-time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			Summary:        "A completed summary.",
			SummaryVersion: version,
		},
	}}
}

// mintedPublishKey is the listener's minted dedupe-key shape after its
// "publish." prefix (legion packages/contracts MINTED_DEDUPE_KEY_PATTERN): a
// key of this shape earns a JetStream MsgId and every host's dedupe.
var mintedPublishKey = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// hubListener stands in for the Envoy hub's listener. As its POST
// /v1/messages/publish does (legion packages/envoy/cmd/listener/api.go,
// publishHandler), it refuses with 400 a publish that is not JSON, names no
// topic or one outside notifications., has no message, names a source the
// listener does not take, or names both a dedupe_key and an idempotency_key,
// and 415 one that is not sent as JSON. It answers each accepted publish with
// the next of statuses (200 once they run out) and records it.
type hubListener struct {
	*httptest.Server
	mu        sync.Mutex
	statuses  []int
	published []map[string]any
}

var listenerSources = map[string]bool{"agent": true, "human": true, "envoy": true, "github": true, "slack": true, "whatsapp": true, "ghostwispr": true, "dispatch": true}

func newHubListener(t *testing.T, statuses ...int) *hubListener {
	t.Helper()
	l := &hubListener{statuses: statuses}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != publishPath {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
			http.Error(w, `{"error":"content type must be application/json"}`, http.StatusUnsupportedMediaType)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		topic, _ := body["topic"].(string)
		message, _ := body["message"].(string)
		source, _ := body["source"].(string)
		_, dedupe := body["dedupe_key"]
		_, idempotency := body["idempotency_key"]
		switch {
		case !strings.HasPrefix(topic, "notifications."):
			http.Error(w, `{"error":"topic must begin with notifications."}`, http.StatusBadRequest)
			return
		case strings.TrimSpace(message) == "":
			http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
			return
		case source != "" && !listenerSources[source]:
			http.Error(w, `{"error":"source must be one of: agent, human, envoy, github, slack, whatsapp, ghostwispr, dispatch"}`, http.StatusBadRequest)
			return
		case dedupe && idempotency:
			http.Error(w, `{"error":"dedupe_key and idempotency_key are mutually exclusive"}`, http.StatusBadRequest)
			return
		}
		l.mu.Lock()
		l.published = append(l.published, body)
		status := http.StatusOK
		if n := len(l.published) - 1; n < len(l.statuses) {
			status = l.statuses[n]
		}
		count := len(l.published)
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = fmt.Fprintf(w, `{"event_id":"evt-%d","topic":%q}`, count, topic)
			return
		}
		_, _ = w.Write([]byte(`{"error":"refused"}`))
	}))
	t.Cleanup(l.Close)
	return l
}

// requests returns every publish the listener answered, in order.
func (l *hubListener) requests() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.published...)
}

// payloadOf decodes a publish's payload, the JSON object of strings the
// webhook normalizer wrote.
func payloadOf(t *testing.T, body map[string]any) map[string]string {
	t.Helper()
	raw, _ := body["payload"].(string)
	var payload map[string]string
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload %q is not a JSON object of strings: %v", raw, err)
	}
	return payload
}

func testClient(t *testing.T, listener *hubListener) *Client {
	t.Helper()
	client, err := NewClient(listener.URL, WithHTTPClient(listener.Client()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.retryDelays = nil
	return client
}

func testPublisher(t *testing.T, listener *hubListener, store Store, opts ...Option) *Publisher {
	t.Helper()
	publisher, err := NewPublisher(listener.URL, store, append([]Option{WithHTTPClient(listener.Client())}, opts...)...)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	publisher.client.retryDelays = nil
	return publisher
}

func summaryReadyEvent(version int) Event {
	return Event{
		Type:           eventSummaryReady,
		SessionID:      "session-1",
		SummaryVersion: version,
		Payload: map[string]any{
			"title":            "Team Sync",
			"duration":         int64(2700),
			"started_at":       "2026-11-19T11:45:45Z",
			"ended_at":         "2026-11-19T12:30:45Z",
			"drive_folder_url": "https://drive.google.com/drive/folders/folder-1",
			"session_url":      "https://ghost-wispr.example.ts.net/api/sessions/session-1",
			"status":           "completed",
			"summary":          "Reviewed the hub cutover.",
			"summary_preset":   "default",
			"summary_model":    "provider-summary-model",
			"timestamp":        "2026-11-19T12:30:00Z",
		},
	}
}

// TestClientPublishesTheWebhookEnvelope fails if a publish through the hub's
// listener gives consumers another topic, source, summary or payload than
// Envoy's Ghost Wispr webhook normalizer made from the same event (legion
// packages/envoy/internal/contracts/normalize.go GhostWisprEnvelope and
// ghostWisprPayload, at 9a8fe5f9), or carries a field the listener would turn
// into another envelope: a chosen dedupe_key, which names no event once the
// listener mints source_event_id, or a source_session, which stamps a sender.
func TestClientPublishesTheWebhookEnvelope(t *testing.T) {
	listener := newHubListener(t)
	client := testClient(t, listener)
	client.now = func() time.Time { return time.Date(2026, 11, 19, 12, 30, 45, 0, time.UTC) }

	if err := client.Send(context.Background(), summaryReadyEvent(3)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := listener.requests()
	if len(got) != 1 {
		t.Fatalf("publishes = %d, want 1", len(got))
	}
	body := got[0]
	for field, want := range map[string]string{
		"topic":   "notifications.ghostwispr.session-1.summary.ready",
		"source":  "ghostwispr",
		"message": "ghostwispr summary_ready for session session-1: Team Sync",
	} {
		if value, _ := body[field].(string); value != want {
			t.Errorf("%s = %q, want %q", field, value, want)
		}
	}
	for _, field := range []string{"dedupe_key", "source_session"} {
		if _, present := body[field]; present {
			t.Errorf("publish carries %s = %v, want it absent", field, body[field])
		}
	}
	if key, _ := body["idempotency_key"].(string); !mintedPublishKey.MatchString(key) {
		t.Errorf("idempotency_key = %q, want a lowercase UUID the listener counts as minted", key)
	}
	want := map[string]string{
		"event_type":       "summary_ready",
		"session_id":       "session-1",
		"started_at":       "2026-11-19T11:45:45Z",
		"ended_at":         "2026-11-19T12:30:45Z",
		"drive_folder_url": "https://drive.google.com/drive/folders/folder-1",
		"session_url":      "https://ghost-wispr.example.ts.net/api/sessions/session-1",
		"title":            "Team Sync",
		"duration":         "2700",
		"created_at":       "2026-11-19T12:30:45Z",
		"status":           "completed",
		"summary":          "Reviewed the hub cutover.",
		"summary_preset":   "default",
		"summary_model":    "provider-summary-model",
		"timestamp":        "2026-11-19T12:30:00Z",
		"version":          "3",
		"payload_type":     "summary_ready",
	}
	if payload := payloadOf(t, body); !reflect.DeepEqual(payload, want) {
		t.Errorf("payload = %#v, want %#v", payload, want)
	}
}

// TestClientWritesSessionIDsAsTheNormalizerDoes fails if a session id that
// is not one NATS token gives another topic than the webhook's, or a long
// first title line another summary.
func TestClientWritesSessionIDsAsTheNormalizerDoes(t *testing.T) {
	listener := newHubListener(t)
	client := testClient(t, listener)
	long := strings.Repeat("t", 90)
	if err := client.Send(context.Background(), Event{
		Type:      "Session.Started",
		SessionID: " a.b c>d*e/f ",
		Payload:   map[string]any{"title": "\n  " + long + "\nsecond line"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := listener.requests()[0]
	if got, want := body["topic"], "notifications.ghostwispr.a_b_c_d_e_f.session.started"; got != want {
		t.Errorf("topic = %v, want %q", got, want)
	}
	if got, want := body["message"], "ghostwispr session_started for session a.b c>d*e/f: "+strings.Repeat("t", 80)+"…"; got != want {
		t.Errorf("message = %v, want %q", got, want)
	}
}

// TestClientKeysEachEventOnce fails if two sends of one event carry
// different idempotency keys (the stream would keep both and hosts would
// deliver both), or if two different events share one (the stream would drop
// the second).
func TestClientKeysEachEventOnce(t *testing.T) {
	listener := newHubListener(t)
	client := testClient(t, listener)
	for _, ev := range []Event{summaryReadyEvent(3), summaryReadyEvent(3), summaryReadyEvent(4), {Type: eventSessionEnded, SessionID: "session-1"}} {
		if err := client.Send(context.Background(), ev); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := listener.requests()
	keys := make([]any, len(got))
	for i, body := range got {
		keys[i] = body["idempotency_key"]
	}
	if len(keys) != 4 || keys[0] != keys[1] {
		t.Fatalf("one event sent twice carried keys %v, want one", keys)
	}
	if keys[0] == keys[2] || keys[0] == keys[3] || keys[2] == keys[3] {
		t.Errorf("distinct events shared a key: %v", keys)
	}
}

// TestClientRetriesWithTheSameKey fails if a publish the listener could not
// store (a 503 from a JetStream timeout) is not sent again, or is sent again
// under another key, which would store the event twice if the first attempt
// landed after all.
func TestClientRetriesWithTheSameKey(t *testing.T) {
	listener := newHubListener(t, http.StatusServiceUnavailable, http.StatusOK)
	client := testClient(t, listener)
	client.retryDelays = []time.Duration{0}

	if err := client.Send(context.Background(), Event{Type: eventSessionEnded, SessionID: "session-1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := listener.requests()
	if len(got) != 2 {
		t.Fatalf("publishes = %d, want 2", len(got))
	}
	if !reflect.DeepEqual(got[1], got[0]) {
		t.Errorf("retry = %#v, want the first attempt %#v", got[1], got[0])
	}
}

// TestClientDoesNotRetryARefusal fails if a publish the listener refuses is
// sent again (a retry cannot change its verdict), or if the refusal is not
// logged with its status.
func TestClientDoesNotRetryARefusal(t *testing.T) {
	listener := newHubListener(t, http.StatusBadRequest)
	var logs bytes.Buffer
	client, err := NewClient(listener.URL, WithHTTPClient(listener.Client()), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	client.retryDelays = []time.Duration{0, 0}

	if err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-1"}); err == nil {
		t.Fatal("Send succeeded, want the listener's refusal")
	}
	if got := len(listener.requests()); got != 1 {
		t.Fatalf("publishes = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "answered 400") {
		t.Errorf("log %q does not name the 400", logs.String())
	}
}

func TestClientStopsWaitingWhenTheContextEnds(t *testing.T) {
	listener := newHubListener(t, http.StatusServiceUnavailable)
	client := testClient(t, listener)
	client.retryDelays = []time.Duration{time.Hour}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.Send(ctx, Event{Type: eventSessionEnded, SessionID: "session-1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send error = %v, want deadline exceeded", err)
	}
	if got := len(listener.requests()); got != 1 {
		t.Fatalf("publishes = %d, want 1", got)
	}
}

// TestHubListenerRefusesWhatTheListenerRefuses fails if the stand-in takes a
// publish the real listener refuses, so the tests above would pass for a
// client the hub turns away. The control is a publish it takes.
func TestHubListenerRefusesWhatTheListenerRefuses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		contentType string
		want        int
	}{
		{"control", `{"source":"ghostwispr","topic":"notifications.ghostwispr.s.session.started","message":"m"}`, "application/json", http.StatusOK},
		{"not JSON", `{"source":"ghostwispr","topic":"notifications.ghostwispr.s.session.started","message":"m"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"no message", `{"source":"ghostwispr","topic":"notifications.ghostwispr.s.session.started"}`, "application/json", http.StatusBadRequest},
		{"topic outside notifications", `{"source":"ghostwispr","topic":"ghostwispr.s","message":"m"}`, "application/json", http.StatusBadRequest},
		{"unknown source", `{"source":"recorder","topic":"notifications.ghostwispr.s.session.started","message":"m"}`, "application/json", http.StatusBadRequest},
		{"both keys", `{"topic":"notifications.ghostwispr.s.session.started","message":"m","dedupe_key":"a","idempotency_key":"b"}`, "application/json", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := newHubListener(t)
			response, err := listener.Client().Post(listener.URL+publishPath, tc.contentType, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.want)
			}
		})
	}
}

func TestPublishSummaryReadyUsesSummaryVersionAndMarksStatus(t *testing.T) {
	listener := newHubListener(t)
	store := completedSession(3)
	sess := store.sessions["session-1"]
	sess.SummaryModel = "provider-summary-model"
	store.sessions["session-1"] = sess
	publisher := testPublisher(t, listener, store)

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishPublished)
	}
	got := listener.requests()
	if len(got) != 1 {
		t.Fatalf("publishes = %d, want 1", len(got))
	}
	payload := payloadOf(t, got[0])
	if payload["version"] != "3" || payload["summary_model"] != "provider-summary-model" {
		t.Fatalf("payload = %#v, want version 3 and the summary's model", payload)
	}
}

// A regeneration that starts while version 1 is in flight moves the session
// to version 2 and resets its delivery state. The late completion of version
// 1 must not mark version 2 as published.
func TestPublishSummaryReadyLeavesANewerSummaryAlone(t *testing.T) {
	listener := newHubListener(t)
	store := completedSession(1)
	store.afterStatus = func(status string) {
		if status != storage.EnvoyPublishPending {
			return
		}
		sess := store.sessions["session-1"]
		sess.SummaryVersion = 2
		store.sessions["session-1"] = sess
	}
	publisher := testPublisher(t, listener, store)

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	got := listener.requests()
	if len(got) != 1 || payloadOf(t, got[0])["version"] != "1" {
		t.Fatalf("publishes = %#v, want one of version 1", got)
	}
	if status := store.statuses["session-1"]; status != storage.EnvoyPublishPending {
		t.Fatalf("publish status after a stale completion = %q, want %q left for version 2", status, storage.EnvoyPublishPending)
	}
}

func TestPublishSummaryReadyRecordsARefusalFailed(t *testing.T) {
	listener := newHubListener(t, http.StatusBadRequest)
	store := completedSession(1)
	publisher := testPublisher(t, listener, store)
	publisher.client.retryDelays = []time.Duration{0, 0}

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSummaryReady succeeded, want the 400")
	}
	if got := len(listener.requests()); got != 1 {
		t.Fatalf("publishes = %d, want 1", got)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishFailed {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishFailed)
	}
}

func TestPublishSummaryReadyRecordsFailed5xx(t *testing.T) {
	listener := newHubListener(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	store := completedSession(1)
	publisher := testPublisher(t, listener, store)
	publisher.client.retryDelays = []time.Duration{0, 0}

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSummaryReady succeeded, want 503 error")
	}
	if got := len(listener.requests()); got != 3 {
		t.Fatalf("publishes = %d, want 3", got)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishFailed {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishFailed)
	}
}

func TestSummaryReadyRetries429(t *testing.T) {
	listener := newHubListener(t, http.StatusTooManyRequests, http.StatusOK)
	store := completedSession(1)
	publisher := testPublisher(t, listener, store)
	publisher.client.retryDelays = []time.Duration{0}
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	if got := len(listener.requests()); got != 2 {
		t.Fatalf("publishes = %d, want 2", got)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishPublished)
	}
}

func TestLifecycleEventsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	startedAt := time.Date(2026, 11, 11, 7, 20, 0, 0, time.UTC)
	if err := store.CreateSession("session-1", startedAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	down := newHubListener(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	publisher := testPublisher(t, down, store)
	if err := publisher.PublishSessionStarted(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSessionStarted succeeded while the hub was down")
	}
	if err := store.EndSession("session-1", startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := publisher.PublishSessionEnded(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSessionEnded succeeded while the hub was down")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	up := newHubListener(t)
	publisher = testPublisher(t, up, reopened)
	if err := publisher.PublishPendingLifecycleEvents(context.Background()); err != nil {
		t.Fatalf("PublishPendingLifecycleEvents: %v", err)
	}
	got := up.requests()
	if len(got) != 2 {
		t.Fatalf("publishes = %d, want 2", len(got))
	}
	if got[0]["topic"] != "notifications.ghostwispr.session-1.session.started" || got[1]["topic"] != "notifications.ghostwispr.session-1.session.ended" {
		t.Fatalf("topics = %v then %v, want start then end", got[0]["topic"], got[1]["topic"])
	}
	// The restart's publishes are the ones the hub refused: same keys.
	refused := down.requests()
	if got[0]["idempotency_key"] != refused[0]["idempotency_key"] || got[1]["idempotency_key"] != refused[1]["idempotency_key"] {
		t.Fatalf("the restart's keys differ from the first attempts': %v, %v", got, refused)
	}
	events, err := reopened.PendingEnvoyLifecycleEvents()
	if err != nil {
		t.Fatalf("PendingEnvoyLifecycleEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("pending lifecycle events = %#v, want none", events)
	}
}

func TestPendingLifecycleEventsContinueAfterFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	startedAt := time.Date(2026, 11, 11, 7, 20, 0, 0, time.UTC)
	if err := store.CreateSession("session-1", startedAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession("session-1", startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.EnqueueEnvoyLifecycleEvent(string(eventSessionStarted), "session-1"); err != nil {
		t.Fatalf("enqueue start: %v", err)
	}
	if err := store.EnqueueEnvoyLifecycleEvent(string(eventSessionEnded), "session-1"); err != nil {
		t.Fatalf("enqueue end: %v", err)
	}

	listener := newHubListener(t, http.StatusServiceUnavailable, http.StatusOK)
	publisher := testPublisher(t, listener, store)
	if err := publisher.PublishPendingLifecycleEvents(context.Background()); err == nil {
		t.Fatal("PublishPendingLifecycleEvents succeeded, want the first event error")
	}
	if got := len(listener.requests()); got != 2 {
		t.Fatalf("publishes = %d, want 2", got)
	}
	pending, err := store.PendingEnvoyLifecycleEvents()
	if err != nil {
		t.Fatalf("PendingEnvoyLifecycleEvents: %v", err)
	}
	if len(pending) != 1 || pending[0].Type != string(eventSessionStarted) {
		t.Fatalf("pending events = %#v, want the failed start only", pending)
	}
}

// TestAFailedSummaryIsSentAgainLater fails if a summary_ready the hub could
// not take is not left for the sweep, or is not sent once the hub takes it.
func TestAFailedSummaryIsSentAgainLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	startedAt := time.Date(2026, 11, 11, 7, 20, 0, 0, time.UTC)
	if err := store.CreateSession("session-1", startedAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession("session-1", startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.UpdateSummaryAtGeneration("session-1", "Title", "Summary", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
		t.Fatalf("UpdateSummaryAtGeneration: %v", err)
	}

	listener := newHubListener(t, http.StatusServiceUnavailable)
	publisher := testPublisher(t, listener, store)
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSummaryReady succeeded while the hub answered 503")
	}
	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("GetSessionsNeedingEnvoyPublish: %v", err)
	}
	if len(ids) != 1 || ids[0] != "session-1" {
		t.Fatalf("failed summary candidates = %v, want [session-1]", ids)
	}
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady once the hub takes it: %v", err)
	}
	if ids, err = store.GetSessionsNeedingEnvoyPublish(); err != nil || len(ids) != 0 {
		t.Fatalf("sessions needing an Envoy publish after delivery = %v, %v; want none", ids, err)
	}
}

// TestEmptySummaryIsNotPublished fails if the summary-ready path, which the
// startup sweep also uses, sends Envoy a completed summary with no text (the
// one a session gets when no summarizer is configured), or if the sweep then
// selects that session again on every run instead of recording it as
// skipped. A summary made later is still published.
func TestEmptySummaryIsNotPublished(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	startedAt := time.Date(2026, 11, 11, 7, 20, 0, 0, time.UTC)
	if err := store.CreateSession("session-1", startedAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.EndSession("session-1", startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := store.StartSummary("session-1"); err != nil {
		t.Fatalf("StartSummary: %v", err)
	}
	_, generation, err := store.Canonicalize("session-1")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if err := store.UpdateSummaryAtGeneration("session-1", "", "", storage.SummaryCompleted, "", "{}", "", generation); err != nil {
		t.Fatalf("complete empty summary: %v", err)
	}

	listener := newHubListener(t)
	publisher := testPublisher(t, listener, store)
	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("GetSessionsNeedingEnvoyPublish: %v", err)
	}
	for _, id := range ids {
		if err := publisher.PublishSummaryReady(context.Background(), id); err != nil {
			t.Fatalf("PublishSummaryReady(%s): %v", id, err)
		}
	}
	if got := listener.requests(); len(got) != 0 {
		t.Fatalf("summary_ready publishes for an empty summary = %d, want none", len(got))
	}
	if ids, err = store.GetSessionsNeedingEnvoyPublish(); err != nil || len(ids) != 0 {
		t.Fatalf("sessions needing an Envoy publish after the skip = %v, %v; want none", ids, err)
	}
	if sess, err := store.GetSession("session-1"); err != nil || sess.EnvoyPublishStatus != storage.EnvoyPublishSkipped {
		t.Fatalf("envoy_publish_status after the skip = %q, %v; want %q", sess.EnvoyPublishStatus, err, storage.EnvoyPublishSkipped)
	}

	if err := store.StartSummary("session-1"); err != nil {
		t.Fatalf("StartSummary: %v", err)
	}
	if err := store.UpdateSummaryAtGeneration("session-1", "Title", "A real summary.", storage.SummaryCompleted, "default", "{}", "", generation); err != nil {
		t.Fatalf("store a real summary: %v", err)
	}
	if ids, err = store.GetSessionsNeedingEnvoyPublish(); err != nil || len(ids) != 1 {
		t.Fatalf("sessions needing an Envoy publish after a real summary = %v, %v; want session-1", ids, err)
	}
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady after a real summary: %v", err)
	}
	if got := listener.requests(); len(got) != 1 {
		t.Fatalf("summary_ready publishes after a real summary = %d, want 1", len(got))
	}
}
