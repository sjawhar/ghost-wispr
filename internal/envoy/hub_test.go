package envoy

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const hubPublishPath = "/v1/messages/publish"

// mintedPublishKey is the listener's minted dedupe-key shape, after its
// "publish." prefix (legion packages/contracts MINTED_DEDUPE_KEY_PATTERN): a
// key of this shape earns a JetStream MsgId and every host's dedupe.
var mintedPublishKey = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func testHubClient(t *testing.T) (*HubClient, *fakeListener) {
	t.Helper()
	fake, server := newFakeListener(t)
	fake.answer(hubPublishPath, http.StatusOK, `{"event_id":"evt-1"}`)
	client, err := NewHubClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewHubClient: %v", err)
	}
	client.retryDelays = nil
	return client, fake
}

func summaryReadyEvent(version int) Event {
	return Event{
		Type:           eventSummaryReady,
		SessionID:      "session-1",
		SummaryVersion: version,
		Payload: map[string]any{
			"title":          "Team Sync",
			"duration":       int64(2700),
			"status":         "completed",
			"summary":        "Reviewed the hub cutover.",
			"summary_preset": "default",
			"timestamp":      "2026-11-19T12:30:00Z",
		},
	}
}

// TestHubClientPublishesTheWebhookEnvelope fails if a publish through the
// hub's listener gives consumers another topic, source, summary or payload
// than Envoy's Ghost Wispr webhook normalizer made from the same event
// (legion packages/envoy/internal/contracts/normalize.go GhostWisprEnvelope),
// or carries a field the listener would turn into another envelope: a chosen
// dedupe_key (which names no event once the listener mints source_event_id)
// or a source_session (which stamps a sender).
func TestHubClientPublishesTheWebhookEnvelope(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 30, 45, 0, time.UTC)
	client, fake := testHubClient(t)
	client.now = func() time.Time { return now }

	if err := client.Send(context.Background(), summaryReadyEvent(3)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := fake.lastBody(t, hubPublishPath)

	for field, want := range map[string]string{
		"topic":   "notifications.ghostwispr.session-1.summary.ready",
		"source":  "ghostwispr",
		"message": "ghostwispr summary_ready for session session-1: Team Sync",
	} {
		if got, _ := body[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	for _, field := range []string{"dedupe_key", "source_session"} {
		if _, present := body[field]; present {
			t.Errorf("publish carries %s = %v, want it absent", field, body[field])
		}
	}
	key, _ := body["idempotency_key"].(string)
	if !mintedPublishKey.MatchString(key) {
		t.Errorf("idempotency_key = %q, want a lowercase UUID the listener counts as minted", key)
	}

	raw, _ := body["payload"].(string)
	var payload map[string]string
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload %q is not the webhook's JSON object of strings: %v", raw, err)
	}
	want := map[string]string{
		"event_type":     "summary_ready",
		"session_id":     "session-1",
		"title":          "Team Sync",
		"duration":       "2700",
		"created_at":     "2026-11-19T12:30:45Z",
		"status":         "completed",
		"summary":        "Reviewed the hub cutover.",
		"summary_preset": "default",
		"timestamp":      "2026-11-19T12:30:00Z",
		"version":        "3",
		"payload_type":   "summary_ready",
	}
	if !reflect.DeepEqual(payload, want) {
		t.Errorf("payload = %#v, want %#v", payload, want)
	}
}

// TestHubClientKeysEachEventOnce fails if two sends of one event carry
// different idempotency keys (the stream would keep both and hosts would
// deliver both), or if two different events share one (the stream would drop
// the second). The webhook deduped by the event's delivery id the same way.
func TestHubClientKeysEachEventOnce(t *testing.T) {
	client, fake := testHubClient(t)
	ctx := context.Background()
	for _, ev := range []Event{summaryReadyEvent(3), summaryReadyEvent(3), summaryReadyEvent(4), {Type: eventSessionEnded, SessionID: "session-1"}} {
		if err := client.Send(ctx, ev); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	fake.mu.Lock()
	bodies := fake.bodies[hubPublishPath]
	fake.mu.Unlock()
	if len(bodies) != 4 {
		t.Fatalf("publishes = %d, want 4", len(bodies))
	}
	keys := make([]string, len(bodies))
	for i, body := range bodies {
		keys[i], _ = body["idempotency_key"].(string)
	}
	if keys[0] != keys[1] {
		t.Errorf("one event sent twice carried keys %q and %q, want one", keys[0], keys[1])
	}
	if keys[0] == keys[2] || keys[0] == keys[3] || keys[2] == keys[3] {
		t.Errorf("distinct events shared a key: %q", keys)
	}
}

// TestHubClientRetriesWithTheSameKey fails if a publish the listener could
// not store (a 503 from a JetStream timeout) is not sent again, or is sent
// again under another key, which would store the event twice if the first
// attempt landed after all.
func TestHubClientRetriesWithTheSameKey(t *testing.T) {
	client, fake := testHubClient(t)
	client.retryDelays = []time.Duration{0}
	attempts := 0
	fake.mu.Lock()
	fake.answers[hubPublishPath] = func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"nats: timeout"}`))
			return
		}
		_, _ = w.Write([]byte(`{"event_id":"evt-1"}`))
	}
	fake.mu.Unlock()

	if err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fake.mu.Lock()
	bodies := fake.bodies[hubPublishPath]
	fake.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("publishes = %d, want 2", len(bodies))
	}
	if bodies[0]["idempotency_key"] != bodies[1]["idempotency_key"] {
		t.Errorf("retry key %v, want the first attempt's %v", bodies[1]["idempotency_key"], bodies[0]["idempotency_key"])
	}
}

// TestHubClientDoesNotRetryARefusal fails if a publish the listener refuses
// as malformed is sent again: a retry cannot change the listener's verdict.
func TestHubClientDoesNotRetryARefusal(t *testing.T) {
	client, fake := testHubClient(t)
	client.retryDelays = []time.Duration{0, 0}
	fake.answer(hubPublishPath, http.StatusBadRequest, `{"error":"source must be one of: agent"}`)

	if err := client.Send(context.Background(), Event{Type: eventSessionStarted, SessionID: "session-1"}); err == nil {
		t.Fatal("Send succeeded, want the listener's refusal")
	}
	fake.mu.Lock()
	sent := len(fake.bodies[hubPublishPath])
	fake.mu.Unlock()
	if sent != 1 {
		t.Fatalf("publishes = %d, want 1", sent)
	}
}

// TestHubPublisherPublishesCompletedSummary fails if the publisher the unit
// uses for the hub does not publish a completed summary or does not record it
// published, so the sweep would send it again forever.
func TestHubPublisherPublishesCompletedSummary(t *testing.T) {
	endedAt := time.Date(2026, 11, 19, 12, 30, 45, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			StartedAt:      endedAt.Add(-45 * time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			Summary:        "Completed outside the event body.",
			SummaryStatus:  storage.SummaryCompleted,
			SummaryVersion: 3,
		},
	}}
	fake, server := newFakeListener(t)
	fake.answer(hubPublishPath, http.StatusOK, `{"event_id":"evt-1"}`)
	publisher, err := NewHubPublisher(server.URL, store, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewHubPublisher: %v", err)
	}

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishPublished)
	}
	body := fake.lastBody(t, hubPublishPath)
	if got, want := body["topic"], "notifications.ghostwispr.session-1.summary.ready"; got != want {
		t.Fatalf("topic = %v, want %q", got, want)
	}
	raw, _ := body["payload"].(string)
	var payload map[string]string
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, leaked := payload["summary"]; leaked {
		t.Fatalf("payload carries the session's summary text: %q", raw)
	}
}
