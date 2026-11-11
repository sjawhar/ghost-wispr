package envoy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type EventType string

const (
	eventSessionStarted EventType = "session_started"
	eventSessionEnded   EventType = "session_ended"
	eventSummaryReady   EventType = "summary_ready"
)

var defaultRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

type Store interface {
	GetSession(id string) (storage.Session, error)
	UpdateEnvoyPublishStatusAtVersion(sessionID string, summaryVersion int, status string) (bool, error)
	EnqueueEnvoyLifecycleEvent(eventType, sessionID string) error
	PendingEnvoyLifecycleEvents() ([]storage.EnvoyLifecycleEvent, error)
	MarkEnvoyLifecycleEventDelivered(eventType, sessionID string) error
}

// Event is one Ghost Wispr event sent to Envoy's webhook.
type Event struct {
	Type           EventType
	SessionID      string
	SummaryVersion int
	Payload        map[string]any
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client used for webhook requests.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithLogger sets the logger used for webhook request failures and retries.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) {
		if logger != nil {
			c.logger = logging.WithModule(logger, "envoy")
		}
	}
}

// Client sends Ghost Wispr events to Envoy's webhook.
type Client struct {
	url         string
	secret      []byte
	httpClient  *http.Client
	logger      *slog.Logger
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	retryDelays []time.Duration
}
type webhookStatusError struct {
	status int
}

func (err *webhookStatusError) Error() string {
	return fmt.Sprintf("envoy webhook responded with status %d", err.status)
}

// NewClient creates a webhook client. It does not contact the relay until Send.
// It trims the URL and secret as Envoy trims ENVOY_GHOSTWISPR_SIGNING_SECRET,
// so both sides sign with the same key.
func NewClient(url, secret string, opts ...Option) *Client {
	client := &Client{
		url:    strings.TrimSpace(url),
		secret: []byte(strings.TrimSpace(secret)),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger:      logging.WithModule(slog.Default(), "envoy"),
		now:         func() time.Time { return time.Now().UTC() },
		sleep:       sleepWithContext,
		retryDelays: append([]time.Duration(nil), defaultRetryDelays...),
	}
	for _, option := range opts {
		if option != nil {
			option(client)
		}
	}
	return client
}

// Send posts ev to Envoy's webhook. It retries transient failures with the
// event's stable delivery ID and returns immediately for a 4xx response.
func (c *Client) Send(ctx context.Context, ev Event) error {
	if c == nil {
		return errors.New("envoy webhook client is nil")
	}
	if strings.TrimSpace(c.url) == "" {
		return errors.New("envoy webhook URL is required")
	}

	eventType, delivery, err := eventDelivery(ev)
	if err != nil {
		return err
	}
	body, err := webhookEventBody(eventType, ev.SessionID, ev.SummaryVersion, ev.Payload, c.now())
	if err != nil {
		return err
	}
	signature := webhookSignature(c.secret, body)

	for attempt := 0; ; attempt++ {
		retry, err := c.sendOnce(ctx, eventType, delivery, body, signature)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retry || attempt == len(c.retryDelays) {
			return err
		}

		delay := c.retryDelays[attempt]
		c.logger.Warn("envoy webhook retry scheduled",
			"event", eventType,
			"session_id", ev.SessionID,
			"delivery", delivery,
			"delay", delay.String(),
			"error", err,
		)
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) sendOnce(ctx context.Context, eventType EventType, delivery string, body []byte, signature string) (retry bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("create envoy webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GhostWispr-Event", strings.ReplaceAll(string(eventType), "_", "."))
	request.Header.Set("X-GhostWispr-Delivery", delivery)
	request.Header.Set("X-GhostWispr-Signature", signature)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return true, fmt.Errorf("post envoy webhook: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return false, nil
	}

	err = &webhookStatusError{status: response.StatusCode}
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests {
		return true, err
	}
	if response.StatusCode >= http.StatusBadRequest && response.StatusCode < http.StatusInternalServerError {
		c.logger.Warn("envoy webhook request rejected",
			"event", eventType,
			"delivery", delivery,
			"status", response.StatusCode,
		)
		return false, err
	}
	return true, err
}

// Publisher builds Ghost Wispr event payloads from stored sessions.
type Publisher struct {
	store  Store
	client *Client
}

// NewPublisher creates a session-aware Envoy webhook publisher.
func NewPublisher(url, secret string, store Store, opts ...Option) *Publisher {
	client := NewClient(url, secret, opts...)
	return &Publisher{
		store:  store,
		client: client,
	}
}

// PublishSessionStarted sends a session_started event.
func (p *Publisher) PublishSessionStarted(ctx context.Context, sessionID string) error {
	return p.publishSession(ctx, eventSessionStarted, sessionID)
}

// PublishSessionEnded sends a session_ended event.
func (p *Publisher) PublishSessionEnded(ctx context.Context, sessionID string) error {
	return p.publishSession(ctx, eventSessionEnded, sessionID)
}

// PublishSummaryReady sends a summary_ready event and tracks its delivery
// state so the existing sweep retries a completed summary after an outage.
func (p *Publisher) PublishSummaryReady(ctx context.Context, sessionID string) error {
	if p == nil || p.client == nil || p.store == nil {
		return nil
	}

	sess, err := p.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if !summaryCompleted(&sess) {
		return nil
	}
	if strings.TrimSpace(sess.Summary) == "" {
		// No summarizer made a summary, so there is nothing to announce.
		// Recorded as skipped, the publish sweep does not select the session
		// again; a summary made later moves it back to pending.
		if _, err := p.store.UpdateEnvoyPublishStatusAtVersion(sessionID, sess.SummaryVersion, storage.EnvoyPublishSkipped); err != nil {
			return fmt.Errorf("mark envoy publish skipped for session %s: %w", sessionID, err)
		}
		return nil
	}
	if updated, err := p.store.UpdateEnvoyPublishStatusAtVersion(sessionID, sess.SummaryVersion, storage.EnvoyPublishPending); err != nil {
		return fmt.Errorf("mark envoy publish pending for session %s: %w", sessionID, err)
	} else if !updated {
		return nil
	}

	if err := p.sendSession(ctx, eventSummaryReady, &sess); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if updated, statusErr := p.store.UpdateEnvoyPublishStatusAtVersion(sessionID, sess.SummaryVersion, storage.EnvoyPublishFailed); statusErr != nil {
			return fmt.Errorf("publish summary-ready event for session %s: %w (also failed to mark status: %v)", sessionID, err, statusErr)
		} else if !updated {
			return nil
		}
		return fmt.Errorf("publish summary-ready event for session %s: %w", sessionID, err)
	}

	if updated, err := p.store.UpdateEnvoyPublishStatusAtVersion(sessionID, sess.SummaryVersion, storage.EnvoyPublishPublished); err != nil {
		return fmt.Errorf("mark envoy publish published for session %s: %w", sessionID, err)
	} else if !updated {
		return nil
	}
	return nil
}

func (p *Publisher) publishSession(ctx context.Context, eventType EventType, sessionID string) error {
	if p == nil || p.client == nil || p.store == nil {
		return nil
	}
	if err := p.store.EnqueueEnvoyLifecycleEvent(string(eventType), sessionID); err != nil {
		return err
	}
	sess, err := p.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if err := p.sendSession(ctx, eventType, &sess); err != nil {
		return fmt.Errorf("publish %s event for session %s: %w", eventType, sessionID, err)
	}
	if err := p.store.MarkEnvoyLifecycleEventDelivered(string(eventType), sessionID); err != nil {
		return err
	}
	return nil
}

func (p *Publisher) PublishPendingLifecycleEvents(ctx context.Context) error {
	events, err := p.store.PendingEnvoyLifecycleEvents()
	if err != nil {
		return err
	}

	var firstErr error
	for _, event := range events {
		var publishErr error
		switch EventType(event.Type) {
		case eventSessionStarted:
			publishErr = p.PublishSessionStarted(ctx, event.SessionID)
		case eventSessionEnded:
			publishErr = p.PublishSessionEnded(ctx, event.SessionID)
		default:
			publishErr = fmt.Errorf("unknown pending Envoy lifecycle event type %q", event.Type)
		}
		if publishErr != nil && firstErr == nil {
			firstErr = publishErr
		}
	}
	return firstErr
}

func (p *Publisher) sendSession(ctx context.Context, eventType EventType, sess *storage.Session) error {
	payload := map[string]any{
		"title":    sess.Title,
		"summary":  sess.Summary,
		"duration": sessionDuration(sess),
	}
	event := Event{Type: eventType, SessionID: sess.ID, Payload: payload}
	if eventType == eventSummaryReady {
		payload["summary_preset"] = sess.SummaryPreset
		event.SummaryVersion = sess.SummaryVersion
	}
	return p.client.Send(ctx, event)
}

// summaryCompleted reports whether sess is an ended session whose summary
// completed. Only one with summary text is announced to Envoy.
func summaryCompleted(sess *storage.Session) bool {
	return sess.Status == storage.SessionEnded && sess.SummaryStatus == storage.SummaryCompleted && sess.EndedAt != nil
}

func sessionDuration(sess *storage.Session) int64 {
	if sess.EndedAt == nil {
		return 0
	}
	return int64(sess.EndedAt.Sub(sess.StartedAt).Seconds())
}

func eventDelivery(ev Event) (eventType EventType, delivery string, err error) {
	eventType = EventType(normalizeEventType(string(ev.Type)))
	sessionID := strings.TrimSpace(ev.SessionID)
	if sessionID == "" {
		return "", "", errors.New("envoy webhook session ID is required")
	}

	switch eventType {
	case eventSessionStarted, eventSessionEnded:
		return eventType, sessionID + "." + string(eventType), nil
	case eventSummaryReady:
		if ev.SummaryVersion <= 0 {
			return "", "", errors.New("envoy webhook summary version must be a positive integer")
		}
		return eventType, sessionID + "." + string(eventType) + "." + strconv.Itoa(ev.SummaryVersion), nil
	default:
		return "", "", fmt.Errorf("unsupported envoy webhook event type %q", ev.Type)
	}
}

func webhookEventBody(eventType EventType, sessionID string, summaryVersion int, payload map[string]any, createdAt time.Time) ([]byte, error) {
	bodyPayload := make(map[string]any, len(payload)+3)
	maps.Copy(bodyPayload, payload)
	bodyPayload["session_id"] = strings.TrimSpace(sessionID)
	bodyPayload["type"] = eventType
	if eventType == eventSummaryReady {
		bodyPayload["version"] = summaryVersion
	}

	return json.Marshal(struct {
		EventType EventType      `json:"event_type"`
		CreatedAt string         `json:"created_at"`
		Payload   map[string]any `json:"payload"`
	}{
		EventType: eventType,
		CreatedAt: createdAt.UTC().Format(time.RFC3339),
		Payload:   bodyPayload,
	})
}

func webhookSignature(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func normalizeEventType(event string) string {
	event = strings.TrimSpace(strings.ToLower(event))
	return strings.ReplaceAll(event, ".", "_")
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
