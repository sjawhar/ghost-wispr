package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type storeStub struct {
	sessions map[string]storage.Session
	statuses map[string]string
}

func (s *storeStub) GetSession(id string) (storage.Session, error) {
	sess, ok := s.sessions[id]
	if !ok {
		return storage.Session{}, errors.New("not found")
	}
	return sess, nil
}

func (s *storeStub) UpdateEnvoyPublishStatus(sessionID, status string) error {
	if s.statuses == nil {
		s.statuses = make(map[string]string)
	}
	s.statuses[sessionID] = status
	if sess, ok := s.sessions[sessionID]; ok {
		sess.EnvoyPublishStatus = status
		s.sessions[sessionID] = sess
	}
	return nil
}

type busStub struct {
	publishErrs []error
	published   []Envelope
}

func (b *busStub) Publish(item Envelope) error {
	if len(b.publishErrs) > 0 {
		err := b.publishErrs[0]
		b.publishErrs = b.publishErrs[1:]
		if err != nil {
			return err
		}
	}
	b.published = append(b.published, item)
	return nil
}

func (b *busStub) Close() error { return nil }

func TestPublishSummaryReadyPublishesEnvelopeAndMarksStatus(t *testing.T) {
	endedAt := time.Date(2026, 4, 1, 15, 30, 0, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:            "session-1",
			Title:         "Team Sync",
			StartedAt:     endedAt.Add(-45 * time.Minute),
			EndedAt:       &endedAt,
			Status:        storage.SessionEnded,
			SummaryStatus: storage.SummaryCompleted,
			SummaryPreset: "default",
		},
	}}
	bus := &busStub{}
	publisher := NewPublisherWithClient(store, bus, DefaultTopic)
	publisher.id = func() string { return "fixed-id" }
	publisher.now = func() time.Time { return endedAt }
	publisher.retryDelays = nil

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady failed: %v", err)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("expected published status, got %q", got)
	}
	if len(bus.published) != 1 {
		t.Fatalf("expected 1 published envelope, got %d", len(bus.published))
	}
	item := bus.published[0]
	if item.Source != Source {
		t.Fatalf("expected source %q, got %q", Source, item.Source)
	}
	if item.Topic != DefaultTopic {
		t.Fatalf("expected topic %q, got %q", DefaultTopic, item.Topic)
	}
	if item.SourceEventID != "session-1" {
		t.Fatalf("expected source_event_id session-1, got %q", item.SourceEventID)
	}
	if item.DedupeKey != "session-1" {
		t.Fatalf("expected dedupe_key session-1, got %q", item.DedupeKey)
	}

	var payload SummaryReadyPayload
	if err := json.Unmarshal([]byte(item.PayloadSummary), &payload); err != nil {
		t.Fatalf("unmarshal payload_summary: %v", err)
	}
	if payload.SessionID != "session-1" {
		t.Fatalf("expected payload session_id session-1, got %q", payload.SessionID)
	}
	if payload.Title != "Team Sync" {
		t.Fatalf("expected payload title %q, got %q", "Team Sync", payload.Title)
	}
	if payload.DurationSeconds != 2700 {
		t.Fatalf("expected duration_seconds 2700, got %d", payload.DurationSeconds)
	}
}

func TestPublishSummaryReadyRetriesAndFails(t *testing.T) {
	endedAt := time.Date(2026, 4, 1, 15, 30, 0, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:            "session-1",
			StartedAt:     endedAt.Add(-time.Minute),
			EndedAt:       &endedAt,
			Status:        storage.SessionEnded,
			SummaryStatus: storage.SummaryCompleted,
		},
	}}
	bus := &busStub{publishErrs: []error{errors.New("boom-1"), errors.New("boom-2"), errors.New("boom-3"), errors.New("boom-4")}}
	publisher := NewPublisherWithClient(store, bus, DefaultTopic)
	publisher.retryDelays = []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	var delays []time.Duration
	publisher.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	err := publisher.PublishSummaryReady(context.Background(), "session-1")
	if err == nil {
		t.Fatal("expected PublishSummaryReady to fail")
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishFailed {
		t.Fatalf("expected failed status, got %q", got)
	}
	if len(delays) != 3 {
		t.Fatalf("expected 3 retry delays, got %d", len(delays))
	}
}

func TestPublishSummaryReadySkipsDiscardedSessions(t *testing.T) {
	endedAt := time.Date(2026, 4, 1, 15, 30, 0, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:            "session-1",
			StartedAt:     endedAt.Add(-time.Minute),
			EndedAt:       &endedAt,
			Status:        storage.SessionDiscarded,
			SummaryStatus: storage.SummaryCompleted,
		},
	}}
	bus := &busStub{}
	publisher := NewPublisherWithClient(store, bus, DefaultTopic)

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady failed: %v", err)
	}
	if len(bus.published) != 0 {
		t.Fatalf("expected no publishes, got %d", len(bus.published))
	}
}

func TestPublishSummaryReadyIntegrationWithNATS(t *testing.T) {
	server, url := runNATSServer(t)
	defer server.Shutdown()

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	defer conn.Close()
	sub, err := conn.SubscribeSync(DefaultTopic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	endedAt := time.Date(2026, 4, 1, 15, 30, 0, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:            "session-1",
			Title:         "Integration Meeting",
			StartedAt:     endedAt.Add(-10 * time.Minute),
			EndedAt:       &endedAt,
			Status:        storage.SessionEnded,
			SummaryStatus: storage.SummaryCompleted,
			SummaryPreset: "default",
		},
	}}
	publisher, err := NewPublisher(url, DefaultTopic, store)
	if err != nil {
		t.Fatalf("NewPublisher failed: %v", err)
	}
	defer func() { _ = publisher.Close() }()
	publisher.retryDelays = nil

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady failed: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("receive message: %v", err)
	}
	var item Envelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if item.Source != Source {
		t.Fatalf("expected source %q, got %q", Source, item.Source)
	}
	if item.Topic != DefaultTopic {
		t.Fatalf("expected topic %q, got %q", DefaultTopic, item.Topic)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("expected published status, got %q", got)
	}
}

func runNATSServer(t *testing.T) (*natsserver.Server, string) {
	t.Helper()
	server, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	conn, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatalf("connect admin client: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("AddStream: %v", err)
	}
	return server, server.ClientURL()
}
