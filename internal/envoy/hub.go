package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const ghostWisprTopicPrefix = "notifications.ghostwispr."

// hubKeyNamespace scopes the idempotency keys of Ghost Wispr's events: each
// key is the name-based UUID of the event's delivery id in it.
var hubKeyNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/sjawhar/ghost-wispr#envoy-event"))

var ghostWisprTopicSegmentSanitizer = strings.NewReplacer(
	".", "_",
	" ", "_",
	"\n", "_",
	"\r", "_",
	"\t", "_",
	">", "_",
	"*", "_",
	"/", "_",
)

// HubClient publishes Ghost Wispr's events through the Envoy hub's listener
// (POST /v1/messages/publish), which takes a tailnet client's publish with no
// credential. NATS itself is no way in: the hub's credential-free user may
// publish only reply inboxes and agent stream frames (legion
// packages/envoy/infra/core/permissions.go SessionPermissions).
//
// Each publish carries what Envoy's Ghost Wispr webhook normalizer made from
// the same event: topic, source, summary and payload. The listener mints the
// envelope's source_event_id itself, so a dedupe_key of the webhook's shape
// ("ghostwispr.<delivery>") would name no event there: the stream would keep
// every retry, and no host would drop a repeat. The event's idempotency key
// instead is a UUID named by its delivery id, which the listener makes the
// dedupe key "publish.<uuid>": a key the stream and every host count as
// naming its message, so a retry of one event is stored once and delivered
// once, as the webhook's delivery id was.
type HubClient struct {
	listener    *Listener
	logger      *slog.Logger
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	retryDelays []time.Duration
}

// NewHubClient returns a client of the hub's listener at listenerURL, such as
// http://hub.example:9020. httpClient nil uses one with a 10 s timeout.
func NewHubClient(listenerURL string, httpClient *http.Client) (*HubClient, error) {
	return newHubClient(listenerURL, httpClient, collectOptions(nil))
}

func newHubClient(listenerURL string, httpClient *http.Client, opts options) (*HubClient, error) {
	listener, err := NewListener(listenerURL, httpClient)
	if err != nil {
		return nil, err
	}
	return &HubClient{
		listener:    listener,
		logger:      opts.logger,
		now:         func() time.Time { return time.Now().UTC() },
		sleep:       sleepWithContext,
		retryDelays: append([]time.Duration(nil), defaultRetryDelays...),
	}, nil
}

// hubPublish is the body of the listener's POST /v1/messages/publish.
type hubPublish struct {
	Source         string `json:"source"`
	Topic          string `json:"topic"`
	Message        string `json:"message"`
	Payload        string `json:"payload"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Send publishes ev, retrying a failure a retry can fix (no answer, 408, 429
// or 5xx) after each of retryDelays with the same idempotency key. A refusal
// (any other 4xx) returns at once.
func (c *HubClient) Send(ctx context.Context, ev Event) error {
	if c == nil {
		return errors.New("envoy hub client is nil")
	}
	eventType, delivery, err := eventDelivery(ev)
	if err != nil {
		return err
	}
	body, err := hubPublishBody(eventType, delivery, ev, c.now())
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		var answer struct {
			EventID string `json:"event_id"`
		}
		err := c.listener.do(ctx, http.MethodPost, "/v1/messages/publish", nil, body, &answer)
		if err == nil && answer.EventID == "" {
			err = errors.New("envoy: the publish answered no event id")
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !hubRetryable(err) || attempt == len(c.retryDelays) {
			return err
		}
		delay := c.retryDelays[attempt]
		c.logger.Warn("envoy hub publish retry scheduled",
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

// hubRetryable reports whether a publish that failed with err may succeed if
// sent again: one the listener did not answer, or answered 408, 429 or 5xx.
func hubRetryable(err error) bool {
	var refusal *ListenerError
	if !errors.As(err, &refusal) {
		return true
	}
	switch {
	case refusal.Status == http.StatusRequestTimeout, refusal.Status == http.StatusTooManyRequests:
		return true
	case refusal.Status >= http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

// hubPublishBody builds the publish of ev: the topic, summary and payload
// Envoy's webhook normalizer made from the webhook body Ghost Wispr sent
// (legion packages/envoy/internal/contracts/normalize.go GhostWisprEnvelope).
func hubPublishBody(eventType EventType, delivery string, ev Event, now time.Time) (hubPublish, error) {
	body, err := webhookEventBody(eventType, ev.SessionID, ev.SummaryVersion, ev.Payload, now)
	if err != nil {
		return hubPublish{}, err
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return hubPublish{}, fmt.Errorf("decode Ghost Wispr event: %w", err)
	}
	event := normalizeEventType(string(eventType))
	return hubPublish{
		Source:         "ghostwispr",
		Topic:          ghostWisprTopicPrefix + hubTopicSessionID(hubSummarySessionID(parsed)) + "." + hubKind(event),
		Message:        hubSummary(event, parsed),
		Payload:        hubPayload(event, parsed),
		IdempotencyKey: uuid.NewSHA1(hubKeyNamespace, []byte(delivery)).String(),
	}, nil
}

func hubKind(eventType string) string {
	switch normalizeEventType(eventType) {
	case "session_started":
		return "session.started"
	case "session_ended":
		return "session.ended"
	case "summary_ready":
		return "summary.ready"
	default:
		return normalizeEventType(eventType)
	}
}

func hubSummary(eventType string, body map[string]any) string {
	summary := fmt.Sprintf("ghostwispr %s for session %s", eventType, hubSummarySessionID(body))
	if title := hubNestedString(body, "payload", "title"); title != "" {
		summary += ": " + hubFirst(title, 80)
	}
	return hubOneLineSummary(summary)
}

func hubPayload(eventType string, body map[string]any) string {
	data := map[string]string{
		"event_type": eventType,
		"session_id": hubSummarySessionID(body),
		"title":      hubNestedString(body, "payload", "title"),
		"duration":   hubNestedNumberString(body, "payload", "duration"),
		"created_at": hubString(body["created_at"]),
	}
	if eventType == "summary_ready" {
		data["status"] = hubNestedString(body, "payload", "status")
		if summary := hubNestedString(body, "payload", "summary"); summary != "" {
			data["summary"] = hubTruncate(summary, 2048)
			if utf8.RuneCountInString(summary) > 2048 {
				data["body_truncated"] = "true"
			}
		}
		data["summary_preset"] = hubNestedString(body, "payload", "summary_preset")
		data["timestamp"] = hubNestedString(body, "payload", "timestamp")
		data["version"] = hubNestedNumberString(body, "payload", "version")
		data["payload_type"] = hubNestedString(body, "payload", "type")
	}
	for key, value := range data {
		if value == "" {
			delete(data, key)
		} else if key != "changed_paths" {
			data[key] = hubTruncate(value, 2048)
		}
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

func hubSummarySessionID(body map[string]any) string {
	return strings.TrimSpace(hubNestedString(body, "payload", "session_id"))
}

func hubTopicSessionID(sessionID string) string {
	if sessionID == "" {
		return "unknown"
	}
	sessionID = ghostWisprTopicSegmentSanitizer.Replace(sessionID)
	sessionID = strings.Trim(sessionID, "_")
	if sessionID == "" {
		return "unknown"
	}
	return sessionID
}

func hubNestedString(body map[string]any, keys ...string) string {
	return hubString(hubNested(body, keys...))
}

func hubNestedNumberString(body map[string]any, keys ...string) string {
	value := hubNested(body, keys...)
	if value == nil {
		return ""
	}
	if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) && number == math.Trunc(number) {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", value)
}

func hubNested(body map[string]any, keys ...string) any {
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

func hubString(value any) string {
	text, _ := value.(string)
	return text
}

func hubFirst(value string, maxRunes int) string {
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.TrimLeft(line, " \t\r")
		if strings.TrimSpace(line) != "" {
			return hubTruncate(line, maxRunes)
		}
	}
	return ""
}

func hubOneLineSummary(value string) string {
	summary := hubFirst(value, 160)
	if utf8.RuneCountInString(summary) <= 160 {
		return summary
	}
	return string([]rune(summary)[:159]) + "…"
}

func hubTruncate(value string, maxRunes int) string {
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	if maxRunes <= 0 {
		return "…"
	}
	return string([]rune(value)[:maxRunes]) + "…"
}

// HubPublisher builds Ghost Wispr's events from stored sessions and publishes
// them through the Envoy hub (HubClient).
type HubPublisher struct {
	store     Store
	client    *HubClient
	publicURL string
}

// NewHubPublisher creates a session-aware publisher for the hub's listener at
// listenerURL.
func NewHubPublisher(listenerURL string, store Store, opts ...Option) (*HubPublisher, error) {
	collected := collectOptions(opts)
	client, err := newHubClient(listenerURL, collected.httpClient, collected)
	if err != nil {
		return nil, err
	}
	return &HubPublisher{
		store:     store,
		client:    client,
		publicURL: collected.publicURL,
	}, nil
}

// PublishSessionStarted sends a session_started event.
func (p *HubPublisher) PublishSessionStarted(ctx context.Context, sessionID string) error {
	return p.publishSession(ctx, eventSessionStarted, sessionID)
}

// PublishSessionEnded sends a session_ended event.
func (p *HubPublisher) PublishSessionEnded(ctx context.Context, sessionID string) error {
	return p.publishSession(ctx, eventSessionEnded, sessionID)
}

// PublishSummaryReady sends a summary_ready event and tracks its delivery
// state so the existing sweep retries a completed summary after an outage.
func (p *HubPublisher) PublishSummaryReady(ctx context.Context, sessionID string) error {
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

func (p *HubPublisher) publishSession(ctx context.Context, eventType EventType, sessionID string) error {
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

// PublishPendingLifecycleEvents retries the lifecycle events that a prior
// process did not mark delivered.
func (p *HubPublisher) PublishPendingLifecycleEvents(ctx context.Context) error {
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

func (p *HubPublisher) sendSession(ctx context.Context, eventType EventType, sess *storage.Session) error {
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
