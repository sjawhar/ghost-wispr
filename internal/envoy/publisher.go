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
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// KeyReader reads Envoy's webhook signing key: on the unit, from Secret
// Manager with the unit's Google credential.
type KeyReader func(ctx context.Context) (string, error)

// keyReadTimeout bounds one read of the signing key, so an event waits for a
// read that does not answer no longer than this.
const keyReadTimeout = 30 * time.Second

// Option configures a Client or a Publisher.
type Option func(*options)

type options struct {
	logger    *slog.Logger
	publicURL string
	keyStatus func(error)
}

// WithKeyStatus calls report after every read of the signing key, with nil
// when it read a key and the read's error when it did not; the unit shows the
// error in /api/status. report runs with the client's key lock held, so the
// last report always describes the read whose result the client keeps; it
// must not call the client.
func WithKeyStatus(report func(error)) Option {
	return func(o *options) {
		o.keyStatus = report
	}
}

// WithLogger sets the logger used for webhook request failures and retries.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		if logger != nil {
			o.logger = logging.WithModule(logger, "envoy")
		}
	}
}

// WithPublicURL sets the unit's own HTTPS address (config public_url): a
// Publisher's events link to each session's API resource under it.
func WithPublicURL(publicURL string) Option {
	return func(o *options) {
		o.publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	}
}

func collectOptions(opts []Option) options {
	collected := options{logger: logging.WithModule(slog.Default(), "envoy")}
	for _, option := range opts {
		if option != nil {
			option(&collected)
		}
	}
	return collected
}

// Client sends Ghost Wispr events to Envoy's webhook, signed with a key it
// reads once and keeps in memory.
type Client struct {
	url         string
	readKey     KeyReader
	keyStatus   func(error)
	httpClient  *http.Client
	logger      *slog.Logger
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	retryDelays []time.Duration

	keyMu         sync.Mutex
	key           []byte // the key the last working read returned; nil until a read works
	keyReadFailed bool   // the last read failed; key still holds the one before it, if any
}
type webhookStatusError struct {
	status int
}

func (err *webhookStatusError) Error() string {
	return fmt.Sprintf("envoy webhook responded with status %d", err.status)
}

// NewClient creates a webhook client. It reads the signing key with readKey
// at the first Send, not before.
func NewClient(webhookURL string, readKey KeyReader, opts ...Option) *Client {
	return newClient(webhookURL, readKey, collectOptions(opts))
}

func newClient(webhookURL string, readKey KeyReader, opts options) *Client {
	return &Client{
		url:       strings.TrimSpace(webhookURL),
		readKey:   readKey,
		keyStatus: opts.keyStatus,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger:      opts.logger,
		now:         func() time.Time { return time.Now().UTC() },
		sleep:       sleepWithContext,
		retryDelays: append([]time.Duration(nil), defaultRetryDelays...),
	}
}

// signingKey returns the key the client keeps, reading it first when it has
// none. With stale set it reads the key again, unless another send already
// replaced stale. The key is trimmed as Envoy trims
// ENVOY_GHOSTWISPR_SIGNING_SECRET, so both sides sign with the same bytes.
func (c *Client) signingKey(ctx context.Context, stale []byte) ([]byte, error) {
	c.keyMu.Lock()
	kept := c.key
	c.keyMu.Unlock()
	if kept != nil && (stale == nil || !bytes.Equal(kept, stale)) {
		return kept, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, keyReadTimeout)
	raw, err := c.readKey(readCtx)
	cancel()
	key := []byte(strings.TrimSpace(raw))
	if err == nil && len(key) == 0 {
		err = errors.New("the signing key is empty")
	}
	// Reported under keyMu, so the last report always describes the read
	// whose result keyReadFailed holds.
	c.keyMu.Lock()
	c.keyReadFailed = err != nil
	if err == nil {
		c.key = key
	}
	if c.keyStatus != nil {
		c.keyStatus(err)
	}
	c.keyMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("read envoy webhook signing key: %w", err)
	}
	return key, nil
}

// Send posts ev to Envoy's webhook (sendWithRetries). A 401 is what a
// rotated key gives: then Send reads the key again, once, and when it changed
// sends again with it, with a new retry budget. It sends nothing when it has
// no key.
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
	key, err := c.signingKey(ctx, nil)
	if err != nil {
		return err
	}

	err = c.sendWithRetries(ctx, ev.SessionID, eventType, delivery, body, key)
	var status *webhookStatusError
	if !errors.As(err, &status) || status.status != http.StatusUnauthorized {
		return err
	}
	fresh, readErr := c.signingKey(ctx, key)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if bytes.Equal(fresh, key) {
		return err
	}
	return c.sendWithRetries(ctx, ev.SessionID, eventType, delivery, body, fresh)
}

// sendWithRetries posts body signed with key, retrying transient failures
// after each of retryDelays with the event's stable delivery ID; a 4xx
// response is returned at once.
func (c *Client) sendWithRetries(ctx context.Context, sessionID string, eventType EventType, delivery string, body, key []byte) error {
	signature := webhookSignature(key, body)
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
			"session_id", sessionID,
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
	store     Store
	client    *Client
	publicURL string
}

// NewPublisher creates a session-aware Envoy webhook publisher whose events
// are signed with the key readKey reads.
func NewPublisher(webhookURL string, readKey KeyReader, store Store, opts ...Option) *Publisher {
	collected := collectOptions(opts)
	return &Publisher{
		store:     store,
		client:    newClient(webhookURL, readKey, collected),
		publicURL: collected.publicURL,
	}
}

// CheckSigningKey reads the signing key when the publisher has none yet or
// its last read failed, so a key that cannot be read is reported
// (WithKeyStatus) before an event needs it, and a key that reads again clears
// the report without waiting for an event. While the last read worked it
// reads nothing.
func (p *Publisher) CheckSigningKey(ctx context.Context) error {
	c := p.client
	c.keyMu.Lock()
	kept, failed := c.key, c.keyReadFailed
	c.keyMu.Unlock()
	if kept != nil && !failed {
		return nil
	}
	// With kept as stale, signingKey reads again unless a send's read has
	// replaced kept meanwhile.
	_, err := c.signingKey(ctx, kept)
	return err
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

// sendSession sends an event about sess that carries no text of the session:
// no title and no summary. Envoy keeps every event for 72 hours and no
// deletion can recall it, so text sent there would outlive a segment
// deletion that removed the words it repeats. The event says which session
// it is, when it ran, and where an agent reads its current text: the
// session's API resource on the unit (session_url) and its Drive folder.
func (p *Publisher) sendSession(ctx context.Context, eventType EventType, sess *storage.Session) error {
	payload := map[string]any{
		"started_at": sess.StartedAt.UTC().Format(time.RFC3339),
		"duration":   sessionDuration(sess),
	}
	if sess.EndedAt != nil {
		payload["ended_at"] = sess.EndedAt.UTC().Format(time.RFC3339)
	}
	if sess.GDriveFolderID != "" {
		payload["drive_folder_url"] = driveFolderURL + url.PathEscape(sess.GDriveFolderID)
	}
	if p.publicURL != "" {
		payload["session_url"] = p.publicURL + "/api/sessions/" + url.PathEscape(sess.ID)
	}
	event := Event{Type: eventType, SessionID: sess.ID, Payload: payload}
	if eventType == eventSummaryReady {
		payload["summary_preset"] = sess.SummaryPreset
		payload["summary_model"] = sess.SummaryModel
		event.SummaryVersion = sess.SummaryVersion
	}
	return p.client.Send(ctx, event)
}

// driveFolderURL is where Google Drive shows a folder, by its id.
const driveFolderURL = "https://drive.google.com/drive/folders/"

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
