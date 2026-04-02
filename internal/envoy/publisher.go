package envoy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	Source       = "ghost-wispr"
	DefaultTopic = "notifications.ghost-wispr.summary-ready"
)

var defaultRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

type Store interface {
	GetSession(id string) (storage.Session, error)
	UpdateEnvoyPublishStatus(sessionID, status string) error
}

type SummaryReadyPayload struct {
	SessionID       string    `json:"session_id"`
	Title           string    `json:"title"`
	SummaryPreset   string    `json:"summary_preset"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	DurationSeconds int64     `json:"duration_seconds"`
}

type Envelope struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	SourceEventID  string `json:"source_event_id"`
	Topic          string `json:"topic"`
	DedupeKey      string `json:"dedupe_key"`
	IssuedAt       int64  `json:"issued_at"`
	ExpiresAt      *int64 `json:"expires_at,omitempty"`
	PayloadSummary string `json:"payload_summary"`
	PayloadRef     string `json:"payload_ref,omitempty"`
	TraceID        string `json:"trace_id"`
}

func (e Envelope) Validate() error {
	if strings.TrimSpace(e.EventID) == "" {
		return fmt.Errorf("event_id is required")
	}
	if strings.TrimSpace(e.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(e.SourceEventID) == "" {
		return fmt.Errorf("source_event_id is required")
	}
	if strings.TrimSpace(e.Topic) == "" {
		return fmt.Errorf("topic is required")
	}
	if strings.TrimSpace(e.DedupeKey) == "" {
		return fmt.Errorf("dedupe_key is required")
	}
	if e.IssuedAt == 0 {
		return fmt.Errorf("issued_at must be set")
	}
	if strings.TrimSpace(e.PayloadSummary) == "" {
		return fmt.Errorf("payload_summary is required")
	}
	if strings.TrimSpace(e.TraceID) == "" {
		return fmt.Errorf("trace_id is required")
	}
	switch e.Source {
	case "agent", "github", "ghost-wispr", "slack", "whatsapp":
		return nil
	default:
		return fmt.Errorf("source must be one of: agent, github, ghost-wispr, slack, whatsapp")
	}
}

type busPublisher interface {
	Publish(item Envelope) error
	Close() error
}

type Publisher struct {
	store       Store
	client      busPublisher
	topic       string
	logger      *slog.Logger
	now         func() time.Time
	id          func() string
	sleep       func(context.Context, time.Duration) error
	retryDelays []time.Duration
}

func NewPublisher(natsURL, topic string, store Store, logger ...*slog.Logger) (*Publisher, error) {
	urls := splitURLs(natsURL)
	if len(urls) == 0 {
		return nil, fmt.Errorf("nats url is required")
	}
	client, err := connectNATS(urls, logger...)
	if err != nil {
		return nil, err
	}
	return NewPublisherWithClient(store, client, topic, logger...), nil
}

func NewPublisherWithClient(store Store, client busPublisher, topic string, logger ...*slog.Logger) *Publisher {
	l := logging.WithModule(slog.Default(), "envoy")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "envoy")
	}
	if strings.TrimSpace(topic) == "" {
		topic = DefaultTopic
	}
	return &Publisher{
		store:  store,
		client: client,
		topic:  strings.TrimSpace(topic),
		logger: l,
		now: func() time.Time {
			return time.Now().UTC()
		},
		id: func() string {
			return uuid.NewString()
		},
		sleep:       sleepWithContext,
		retryDelays: append([]time.Duration(nil), defaultRetryDelays...),
	}
}

func (p *Publisher) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	return p.client.Close()
}

func (p *Publisher) PublishSummaryReady(ctx context.Context, sessionID string) error {
	if p == nil || p.client == nil || p.store == nil {
		return nil
	}

	sess, err := p.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if !shouldPublish(sess) {
		return nil
	}

	item, err := p.buildEnvelope(sess)
	if err != nil {
		return fmt.Errorf("build envelope for session %s: %w", sessionID, err)
	}
	if err := p.store.UpdateEnvoyPublishStatus(sessionID, storage.EnvoyPublishPending); err != nil {
		return fmt.Errorf("mark envoy publish pending for session %s: %w", sessionID, err)
	}

	p.logger.Info("publishing summary-ready event", "operation", "publish_summary_ready", "session_id", sessionID, "topic", item.Topic)
	if err := p.publishWithRetry(ctx, item); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if statusErr := p.store.UpdateEnvoyPublishStatus(sessionID, storage.EnvoyPublishFailed); statusErr != nil {
			return fmt.Errorf("publish summary-ready event for session %s: %w (also failed to mark status: %v)", sessionID, err, statusErr)
		}
		return fmt.Errorf("publish summary-ready event for session %s: %w", sessionID, err)
	}

	if err := p.store.UpdateEnvoyPublishStatus(sessionID, storage.EnvoyPublishPublished); err != nil {
		return fmt.Errorf("mark envoy publish published for session %s: %w", sessionID, err)
	}
	p.logger.Info("summary-ready event published", "operation", "publish_summary_ready", "session_id", sessionID, "topic", item.Topic)
	return nil
}

func (p *Publisher) publishWithRetry(ctx context.Context, item Envelope) error {
	if err := p.client.Publish(item); err == nil {
		return nil
	} else {
		lastErr := err
		for _, delay := range p.retryDelays {
			p.logger.Warn("summary-ready publish retry scheduled", "operation", "publish_summary_ready", "topic", item.Topic, "delay", delay.String(), "error", lastErr)
			if err := p.sleep(ctx, delay); err != nil {
				return err
			}
			if err := p.client.Publish(item); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		return lastErr
	}
}

func (p *Publisher) buildEnvelope(sess storage.Session) (Envelope, error) {
	if sess.EndedAt == nil {
		return Envelope{}, fmt.Errorf("ended_at is required")
	}
	payloadBytes, err := json.Marshal(SummaryReadyPayload{
		SessionID:       sess.ID,
		Title:           sess.Title,
		SummaryPreset:   sess.SummaryPreset,
		StartedAt:       sess.StartedAt.UTC(),
		EndedAt:         sess.EndedAt.UTC(),
		DurationSeconds: int64(sess.EndedAt.Sub(sess.StartedAt).Seconds()),
	})
	if err != nil {
		return Envelope{}, err
	}

	item := Envelope{
		EventID:        p.id(),
		Source:         Source,
		SourceEventID:  sess.ID,
		Topic:          p.topic,
		DedupeKey:      sess.ID,
		IssuedAt:       p.now().UnixMilli(),
		PayloadSummary: string(payloadBytes),
		TraceID:        p.id(),
	}
	if err := item.Validate(); err != nil {
		return Envelope{}, err
	}
	return item, nil
}

func shouldPublish(sess storage.Session) bool {
	return sess.Status == storage.SessionEnded && sess.SummaryStatus == storage.SummaryCompleted && sess.EndedAt != nil
}

func splitURLs(raw string) []string {
	parts := strings.Split(raw, ",")
	urls := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	return urls
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type natsClient struct {
	logger *slog.Logger
	conn   *nats.Conn
	js     nats.JetStreamContext
	urls   []string
	mu     sync.Mutex
}

func connectNATS(urls []string, logger ...*slog.Logger) (*natsClient, error) {
	l := logging.WithModule(slog.Default(), "envoy")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "envoy")
	}
	nc, err := connect(urls, l)
	if err != nil {
		return nil, err
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, err
	}
	return &natsClient{logger: l, conn: nc, js: js, urls: urls}, nil
}

func (c *natsClient) Publish(item Envelope) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err := c.ensureConn(); err != nil {
		return err
	}
	_, err = c.js.Publish(item.Topic, data)
	if err == nil {
		return nil
	}
	if err == nats.ErrConnectionClosed {
		if ensureErr := c.ensureConn(); ensureErr != nil {
			return ensureErr
		}
		_, err = c.js.Publish(item.Topic, data)
	}
	return err
}

func (c *natsClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.conn.Close()
	return nil
}

func (c *natsClient) ensureConn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.conn.Status() != nats.CLOSED {
		return nil
	}
	nc, err := connect(c.urls, c.logger)
	if err != nil {
		return err
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return err
	}
	c.conn = nc
	c.js = js
	return nil
}

func connect(urls []string, logger *slog.Logger) (*nats.Conn, error) {
	var nc *nats.Conn
	var err error
	for range 10 {
		next := options(urls, logger)
		nc, err = next.Connect()
		if err == nil {
			return nc, nil
		}
		time.Sleep(time.Second)
	}
	return nil, err
}

func options(urls []string, logger *slog.Logger) nats.Options {
	return nats.Options{
		Servers:       urls,
		Name:          "ghost-wispr",
		MaxReconnect:  -1,
		ReconnectWait: 2 * nats.DefaultReconnectWait,
		DisconnectedErrCB: func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("envoy nats disconnected", "error", err)
				return
			}
			logger.Warn("envoy nats disconnected")
		},
		ReconnectedCB: func(nc *nats.Conn) {
			logger.Info("envoy nats reconnected", "url", nc.ConnectedUrl())
		},
		ClosedCB: func(_ *nats.Conn) {
			logger.Warn("envoy nats connection closed")
		},
		AsyncErrorCB: func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if sub != nil {
				logger.Warn("envoy nats async error", "subject", sub.Subject, "error", err)
				return
			}
			logger.Warn("envoy nats async error", "error", err)
		},
	}
}
