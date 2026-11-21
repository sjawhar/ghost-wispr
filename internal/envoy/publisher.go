package envoy

// This file publishes Ghost Wispr's events (session_started, session_ended,
// summary_ready) to Envoy through the hub's listener, POST /v1/messages/publish,
// which takes a tailnet client's publish with no credential. NATS itself is no
// way in: the hub's credential-free user may publish only reply inboxes and
// agent stream frames (legion packages/envoy/infra/core/permissions.go,
// SessionPermissions).
//
// Each publish carries the topic, source, summary and payload Envoy's Ghost
// Wispr webhook normalizer makes from the same event (legion
// packages/envoy/internal/contracts/normalize.go, GhostWisprEnvelope and
// ghostWisprPayload). The listener mints the envelope's source_event_id itself
// (contracts.MessageEnvelope), so a dedupe_key of the webhook's shape,
// "ghostwispr.<delivery>", would name no event there: the stream would store
// every resend, and no host would drop one. The event's idempotency key instead
// is a UUID named by its delivery id, which the listener makes the dedupe key
// "publish.<uuid>": a key the stream and every host count as naming its message
// (contracts.MintedDedupeKeyPattern), so a resend of one event is stored and
// delivered once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type EventType string

const (
	eventSessionStarted EventType = "session_started"
	eventSessionEnded   EventType = "session_ended"
	eventSummaryReady   EventType = "summary_ready"
)

// SessionEndedEvent is the session_ended event's type in the store's
// lifecycle queue, which a session's end queues it under in the same write
// (storage.SQLiteStore.EndSession, RecoverStaleSessions), and the sweep
// sends (PublishPendingLifecycleEvents).
const SessionEndedEvent = string(eventSessionEnded)

const ghostWisprTopicPrefix = "notifications.ghostwispr."

// publishPath is the hub listener's route for a publish to a topic.
const publishPath = "/v1/messages/publish"

// maxEnvelopeTextRunes caps each text the payload copies, as the webhook
// normalizer caps it (legion contracts.maxEnvelopeTextRunes).
const maxEnvelopeTextRunes = 2048

var defaultRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second}

// keyNamespace scopes the idempotency keys of Ghost Wispr's events: each key
// is the name-based UUID of the event's delivery id in it.
var keyNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/sjawhar/ghost-wispr#envoy-event"))

var topicSegmentSanitizer = strings.NewReplacer(
	".", "_",
	" ", "_",
	"\n", "_",
	"\r", "_",
	"\t", "_",
	">", "_",
	"*", "_",
	"/", "_",
)

type Store interface {
	GetSession(id string) (storage.Session, error)
	UpdateEnvoyPublishStatusAtVersion(sessionID string, summaryVersion int, status string) (bool, error)
	EnqueueEnvoyLifecycleEvent(eventType, sessionID string) error
	PendingEnvoyLifecycleEvents() ([]storage.EnvoyLifecycleEvent, error)
	MarkEnvoyLifecycleEventDelivered(eventType, sessionID string) error
}

// Event is one Ghost Wispr event sent to Envoy.
type Event struct {
	Type           EventType
	SessionID      string
	SummaryVersion int
	Payload        map[string]any
}

// Option configures a Client or a Publisher.
type Option func(*options)

type options struct {
	logger     *slog.Logger
	publicURL  string
	httpClient *http.Client
}

// WithHTTPClient sets the HTTP client the listener is called with; nil keeps
// the default, with a 10 s timeout.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) {
		o.httpClient = client
	}
}

// WithLogger sets the logger used for publish failures and retries.
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

// Client publishes Ghost Wispr's events through the hub's listener.
type Client struct {
	listener    *Listener
	logger      *slog.Logger
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	retryDelays []time.Duration
}

// NewClient returns a client of the hub's listener at listenerURL, such as
// http://hub.example:9020. It does not contact the listener until Send. A URL
// that is not http or https, such as the hub's NATS address, is refused: the
// listener's publish route is HTTP, and every send to it would fail.
func NewClient(listenerURL string, opts ...Option) (*Client, error) {
	return newClient(listenerURL, collectOptions(opts))
}

func newClient(listenerURL string, opts options) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(listenerURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("envoy: the hub URL %q is not an http or https URL of the hub's listener", listenerURL)
	}
	listener, err := NewListener(listenerURL, opts.httpClient)
	if err != nil {
		return nil, err
	}
	return &Client{
		listener:    listener,
		logger:      opts.logger,
		now:         func() time.Time { return time.Now().UTC() },
		sleep:       sleepWithContext,
		retryDelays: append([]time.Duration(nil), defaultRetryDelays...),
	}, nil
}

// publishRequest is the body of the listener's POST /v1/messages/publish.
type publishRequest struct {
	Source         string `json:"source"`
	Topic          string `json:"topic"`
	Message        string `json:"message"`
	Payload        string `json:"payload"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Send publishes ev, retrying a failure a retry can fix (no answer, 408, 429
// or 5xx) after each of retryDelays with the same idempotency key. A refusal
// (any other 4xx) returns at once.
func (c *Client) Send(ctx context.Context, ev Event) error {
	if c == nil {
		return errors.New("envoy client is nil")
	}
	eventType, delivery, err := eventDelivery(ev)
	if err != nil {
		return err
	}
	body, err := publishBody(eventType, delivery, ev, c.now())
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		var answer struct {
			EventID string `json:"event_id"`
		}
		err := c.listener.do(ctx, http.MethodPost, publishPath, nil, body, &answer)
		if err == nil && answer.EventID == "" {
			err = errors.New("envoy: the publish answered no event id")
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) || attempt == len(c.retryDelays) {
			c.logger.Warn("envoy publish failed", "event", eventType, "session_id", ev.SessionID, "delivery", delivery, "error", err)
			return err
		}
		delay := c.retryDelays[attempt]
		c.logger.Warn("envoy publish retry scheduled",
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

// retryable reports whether a publish that failed with err may succeed if
// sent again: one the listener did not answer, or answered 408, 429 or 5xx.
func retryable(err error) bool {
	var refusal *ListenerError
	if !errors.As(err, &refusal) {
		return true
	}
	return refusal.Status == http.StatusRequestTimeout || refusal.Status == http.StatusTooManyRequests ||
		refusal.Status >= http.StatusInternalServerError
}

// publishBody builds the publish of ev: the topic, summary and payload the
// webhook normalizer made from the webhook body (eventBody) for the same event.
func publishBody(eventType EventType, delivery string, ev Event, now time.Time) (publishRequest, error) {
	raw, err := eventBody(eventType, ev.SessionID, ev.SummaryVersion, ev.Payload, now)
	if err != nil {
		return publishRequest{}, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return publishRequest{}, fmt.Errorf("decode Ghost Wispr event: %w", err)
	}
	event := string(eventType)
	return publishRequest{
		Source:         "ghostwispr",
		Topic:          ghostWisprTopicPrefix + topicSessionID(summarySessionID(body)) + "." + topicKind(eventType),
		Message:        eventSummary(event, body),
		Payload:        eventPayload(event, body),
		IdempotencyKey: uuid.NewSHA1(keyNamespace, []byte(delivery)).String(),
	}, nil
}

// eventBody is the body Ghost Wispr's webhook posted for the event, which the
// normalizer read; the payload is built from it so that every value (a
// duration's number, a version's) is written as the normalizer wrote it.
func eventBody(eventType EventType, sessionID string, summaryVersion int, payload map[string]any, createdAt time.Time) ([]byte, error) {
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

func topicKind(eventType EventType) string {
	return strings.ReplaceAll(string(eventType), "_", ".")
}

// eventSummary is the normalizer's ghostWisprSummary: the envelope's one-line
// payload_summary.
func eventSummary(eventType string, body map[string]any) string {
	summary := fmt.Sprintf("ghostwispr %s for session %s", eventType, summarySessionID(body))
	if title := nestedString(body, "payload", "title"); title != "" {
		summary += ": " + truncateWithEllipsis(firstNonEmptyLine(title), 80)
	}
	line := firstNonEmptyLine(summary)
	if utf8.RuneCountInString(line) <= 160 {
		return line
	}
	return string([]rune(line)[:159]) + "…"
}

// eventPayload is the normalizer's ghostWisprPayload: a JSON object of
// strings, without the fields the event did not carry.
func eventPayload(eventType string, body map[string]any) string {
	data := map[string]string{
		"event_type":       eventType,
		"session_id":       summarySessionID(body),
		"started_at":       nestedString(body, "payload", "started_at"),
		"ended_at":         nestedString(body, "payload", "ended_at"),
		"drive_folder_url": nestedString(body, "payload", "drive_folder_url"),
		"session_url":      nestedString(body, "payload", "session_url"),
		"title":            nestedString(body, "payload", "title"),
		"duration":         nestedNumberString(body, "payload", "duration"),
		"created_at":       stringValue(body["created_at"]),
	}
	if eventType == string(eventSummaryReady) {
		data["status"] = nestedString(body, "payload", "status")
		if summary := nestedString(body, "payload", "summary"); summary != "" {
			runes := []rune(summary)
			if len(runes) > maxEnvelopeTextRunes {
				summary = string(runes[:maxEnvelopeTextRunes])
				data["body_truncated"] = "true"
			}
			data["summary"] = summary
		}
		data["summary_preset"] = nestedString(body, "payload", "summary_preset")
		data["summary_model"] = nestedString(body, "payload", "summary_model")
		data["timestamp"] = nestedString(body, "payload", "timestamp")
		data["version"] = nestedNumberString(body, "payload", "version")
		data["payload_type"] = nestedString(body, "payload", "type")
	}
	for key, value := range data {
		if value == "" {
			delete(data, key)
			continue
		}
		data[key] = truncateWithEllipsis(value, maxEnvelopeTextRunes)
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

func summarySessionID(body map[string]any) string {
	return strings.TrimSpace(nestedString(body, "payload", "session_id"))
}

func topicSessionID(sessionID string) string {
	sessionID = strings.Trim(topicSegmentSanitizer.Replace(sessionID), "_")
	if sessionID == "" {
		return "unknown"
	}
	return sessionID
}

func nestedString(body map[string]any, keys ...string) string {
	return stringValue(nested(body, keys...))
}

// nestedNumberString writes a whole JSON number as an integer, so a large one
// is not written in scientific notation.
func nestedNumberString(body map[string]any, keys ...string) string {
	value := nested(body, keys...)
	if value == nil {
		return ""
	}
	if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) && number == math.Trunc(number) {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", value)
}

func nested(body map[string]any, keys ...string) any {
	var current any = body
	for _, key := range keys {
		item, _ := current.(map[string]any)
		if item == nil {
			return nil
		}
		current = item[key]
	}
	return current
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func firstNonEmptyLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimLeft(line, " \t\r")
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

func truncateWithEllipsis(text string, maxRunes int) string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	return string([]rune(text)[:maxRunes]) + "…"
}

// Publisher builds Ghost Wispr's events from stored sessions and publishes
// them through the hub (Client).
type Publisher struct {
	store     Store
	client    *Client
	publicURL string
}

// NewPublisher creates a session-aware publisher for the hub's listener at
// listenerURL.
func NewPublisher(listenerURL string, store Store, opts ...Option) (*Publisher, error) {
	collected := collectOptions(opts)
	client, err := newClient(listenerURL, collected)
	if err != nil {
		return nil, err
	}
	return &Publisher{store: store, client: client, publicURL: collected.publicURL}, nil
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
// state so the sweep sends a completed summary again after an outage.
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
	if _, err := p.store.UpdateEnvoyPublishStatusAtVersion(sessionID, sess.SummaryVersion, storage.EnvoyPublishPublished); err != nil {
		return fmt.Errorf("mark envoy publish published for session %s: %w", sessionID, err)
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
	return p.store.MarkEnvoyLifecycleEventDelivered(string(eventType), sessionID)
}

// PublishPendingLifecycleEvents sends again the lifecycle events no publish
// has delivered, an earlier process's included.
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

// eventDelivery normalizes ev's type and names the event: the session and the
// event, and the summary's version for summary_ready. Every send of one event
// has the same delivery id, and so the same idempotency key.
func eventDelivery(ev Event) (eventType EventType, delivery string, err error) {
	eventType = EventType(strings.ReplaceAll(strings.TrimSpace(strings.ToLower(string(ev.Type))), ".", "_"))
	sessionID := strings.TrimSpace(ev.SessionID)
	if sessionID == "" {
		return "", "", errors.New("envoy event session ID is required")
	}
	switch eventType {
	case eventSessionStarted, eventSessionEnded:
		return eventType, sessionID + "." + string(eventType), nil
	case eventSummaryReady:
		if ev.SummaryVersion <= 0 {
			return "", "", errors.New("envoy event summary version must be a positive integer")
		}
		return eventType, sessionID + "." + string(eventType) + "." + strconv.Itoa(ev.SummaryVersion), nil
	default:
		return "", "", fmt.Errorf("unsupported envoy event type %q", ev.Type)
	}
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
