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
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	eventSessionStarted = "session_started"
	eventSessionEnded   = "session_ended"
	eventSummaryReady   = "summary_ready"
)

var defaultRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

type Store interface {
	GetSession(id string) (storage.Session, error)
	UpdateEnvoyPublishStatusAtVersion(sessionID string, summaryVersion int, status string) (bool, error)
}

// Event is one Ghost Wispr event sent to Envoy's webhook.
type Event struct {
	Type      string
	SessionID string
	Payload   map[string]any
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
	body, err := webhookEventBody(eventType, ev.SessionID, ev.Payload, c.now())
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

func (c *Client) sendOnce(ctx context.Context, eventType, delivery string, body []byte, signature string) (retry bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("create envoy webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GhostWispr-Event", strings.ReplaceAll(eventType, "_", "."))
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
	if !shouldPublish(&sess) {
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
		publishStatus := storage.EnvoyPublishFailed
		var responseErr *webhookStatusError
		if errors.As(err, &responseErr) && responseErr.status >= http.StatusBadRequest && responseErr.status < http.StatusInternalServerError {
			publishStatus = storage.EnvoyPublishRejected
		}
		if updated, statusErr := p.store.UpdateEnvoyPublishStatusAtVersion(sessionID, sess.SummaryVersion, publishStatus); statusErr != nil {
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

func (p *Publisher) publishSession(ctx context.Context, eventType, sessionID string) error {
	if p == nil || p.client == nil || p.store == nil {
		return nil
	}
	sess, err := p.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if err := p.sendSession(ctx, eventType, &sess); err != nil {
		return fmt.Errorf("publish %s event for session %s: %w", eventType, sessionID, err)
	}
	return nil
}

func (p *Publisher) sendSession(ctx context.Context, eventType string, sess *storage.Session) error {
	payload := map[string]any{
		"title":    sess.Title,
		"summary":  sess.Summary,
		"duration": sessionDuration(sess),
	}
	if eventType == eventSummaryReady {
		payload["summary_preset"] = sess.SummaryPreset
		payload["version"] = sess.SummaryVersion
	}
	return p.client.Send(ctx, Event{
		Type:      eventType,
		SessionID: sess.ID,
		Payload:   payload,
	})
}

func shouldPublish(sess *storage.Session) bool {
	return sess.Status == storage.SessionEnded && sess.SummaryStatus == storage.SummaryCompleted && sess.EndedAt != nil
}

func sessionDuration(sess *storage.Session) int64 {
	if sess.EndedAt == nil {
		return 0
	}
	return int64(sess.EndedAt.Sub(sess.StartedAt).Seconds())
}

func eventDelivery(ev Event) (eventType, delivery string, err error) {
	eventType = normalizeEventType(ev.Type)
	sessionID := strings.TrimSpace(ev.SessionID)
	if sessionID == "" {
		return "", "", errors.New("envoy webhook session ID is required")
	}

	switch eventType {
	case eventSessionStarted, eventSessionEnded:
		return eventType, sessionID + "." + eventType, nil
	case eventSummaryReady:
		version, ok := summaryVersion(ev.Payload["version"])
		if !ok {
			return "", "", errors.New("envoy webhook summary version must be a positive integer")
		}
		return eventType, sessionID + "." + eventType + "." + strconv.FormatInt(version, 10), nil
	default:
		return "", "", fmt.Errorf("unsupported envoy webhook event type %q", ev.Type)
	}
}

func webhookEventBody(eventType, sessionID string, payload map[string]any, createdAt time.Time) ([]byte, error) {
	bodyPayload := make(map[string]any, len(payload)+2)
	maps.Copy(bodyPayload, payload)
	bodyPayload["session_id"] = strings.TrimSpace(sessionID)
	bodyPayload["type"] = eventType

	return json.Marshal(struct {
		EventType string         `json:"event_type"`
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

func summaryVersion(value any) (int64, bool) {
	switch version := value.(type) {
	case int:
		return int64(version), version > 0
	case int8:
		return int64(version), version > 0
	case int16:
		return int64(version), version > 0
	case int32:
		return int64(version), version > 0
	case int64:
		return version, version > 0
	case uint:
		if uint64(version) > math.MaxInt64 {
			return 0, false
		}
		return int64(version), version > 0
	case uint8:
		return int64(version), version > 0
	case uint16:
		return int64(version), version > 0
	case uint32:
		return int64(version), version > 0
	case uint64:
		if version > math.MaxInt64 {
			return 0, false
		}
		return int64(version), version > 0
	case float64:
		// float64(math.MaxInt64) rounds up to 2^63, which int64 cannot hold.
		if version <= 0 || version != math.Trunc(version) || version >= math.MaxInt64 {
			return 0, false
		}
		return int64(version), true
	case json.Number:
		parsed, err := version.Int64()
		return parsed, err == nil && parsed > 0
	default:
		return 0, false
	}
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
