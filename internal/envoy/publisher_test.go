package envoy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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

type webhookBody struct {
	EventType string         `json:"event_type"`
	CreatedAt string         `json:"created_at"`
	Payload   map[string]any `json:"payload"`
}

type webhookRequest struct {
	method      string
	event       string
	delivery    string
	contentType string
	body        webhookBody
}

func TestSendSignsTheBody(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret")
	defer server.Close()

	client := NewClient(server.URL, "shared-secret")
	client.retryDelays = nil

	if err := client.Send(context.Background(), Event{
		Type:           eventSummaryReady,
		SessionID:      "session-1",
		SummaryVersion: 1,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := len(requests()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestNewClientSetsRequestTimeout(t *testing.T) {
	client := NewClient("https://relay.example.test/webhook/ghostwispr", "shared-secret")
	if client.httpClient.Timeout != 10*time.Second {
		t.Fatalf("default HTTP timeout = %s, want 10s", client.httpClient.Timeout)
	}
}

// Envoy trims ENVOY_GHOSTWISPR_SIGNING_SECRET (internal/webhook/config.go in
// the listener), so a secret pasted with a trailing newline must sign the
// same bytes on both sides. The listener stand-in holds the trimmed secret.
func TestNewClientTrimsTheSecretLikeEnvoy(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret")
	defer server.Close()

	client := NewClient(server.URL, "  shared-secret\n\n")
	client.retryDelays = nil

	if err := client.Send(context.Background(), Event{
		Type:      "session_started",
		SessionID: "session-1",
	}); err != nil {
		t.Fatalf("Send with an untrimmed secret: %v", err)
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestSendStopsWaitingWhenTheContextEnds(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret", http.StatusServiceUnavailable)
	defer server.Close()

	client := NewClient(server.URL, "shared-secret")
	client.retryDelays = []time.Duration{time.Hour}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.Send(ctx, Event{
		Type:      "session_ended",
		SessionID: "session-1",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send error = %v, want deadline exceeded", err)
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestSendCarriesTheHeadersAndBodyTheListenerNeeds(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret")
	defer server.Close()

	now := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	client := NewClient(server.URL, "shared-secret")
	client.now = func() time.Time { return now }
	client.retryDelays = nil

	if err := client.Send(context.Background(), Event{
		Type:           eventSummaryReady,
		SessionID:      "session-1",
		SummaryVersion: 3,
		Payload: map[string]any{
			"title":          "Team sync",
			"summary":        "Reviewed the webhook cutover.",
			"duration":       int64(2700),
			"summary_preset": "default",
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	request := got[0]
	if request.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", request.method)
	}
	if request.event != "summary.ready" {
		t.Fatalf("event header = %q, want summary.ready", request.event)
	}
	if request.delivery != "session-1.summary_ready.3" {
		t.Fatalf("delivery header = %q, want session-1.summary_ready.3", request.delivery)
	}
	if request.contentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", request.contentType)
	}
	if request.body.EventType != "summary_ready" {
		t.Fatalf("body event_type = %q, want summary_ready", request.body.EventType)
	}
	if request.body.CreatedAt != now.Format(time.RFC3339) {
		t.Fatalf("created_at = %q, want %q", request.body.CreatedAt, now.Format(time.RFC3339))
	}
	if sessionID, _ := request.body.Payload["session_id"].(string); sessionID != "session-1" {
		t.Fatalf("payload session_id = %q, want session-1", sessionID)
	}
	if eventType, _ := request.body.Payload["type"].(string); eventType != "summary_ready" {
		t.Fatalf("payload type = %q, want summary_ready", eventType)
	}
	if title, _ := request.body.Payload["title"].(string); title != "Team sync" {
		t.Fatalf("payload title = %q, want Team sync", title)
	}
	if summary, _ := request.body.Payload["summary"].(string); summary != "Reviewed the webhook cutover." {
		t.Fatalf("payload summary = %q, want webhook summary", summary)
	}
	if duration, _ := request.body.Payload["duration"].(float64); duration != 2700 {
		t.Fatalf("payload duration = %v, want 2700", request.body.Payload["duration"])
	}
	if version, _ := request.body.Payload["version"].(float64); version != 3 {
		t.Fatalf("payload version = %v, want 3", request.body.Payload["version"])
	}
}

func TestSendRetriesWithTheSameDelivery(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret", http.StatusServiceUnavailable, http.StatusOK)
	defer server.Close()

	client := NewClient(server.URL, "shared-secret")
	client.retryDelays = []time.Duration{0}

	if err := client.Send(context.Background(), Event{
		Type:      "session_ended",
		SessionID: "session-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[0].delivery != "session-1.session_ended" {
		t.Fatalf("first delivery = %q, want session-1.session_ended", got[0].delivery)
	}
	if got[1].delivery != got[0].delivery {
		t.Fatalf("retry delivery = %q, want %q", got[1].delivery, got[0].delivery)
	}
	if !reflect.DeepEqual(got[1].body, got[0].body) {
		t.Fatalf("retry body = %#v, want %#v", got[1].body, got[0].body)
	}
}

func TestSendGivesUpOnA4xx(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret", http.StatusUnauthorized)
	defer server.Close()

	var logs bytes.Buffer
	client := NewClient(server.URL, "shared-secret", WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	client.retryDelays = []time.Duration{0, 0, 0}

	err := client.Send(context.Background(), Event{
		Type:      "session_started",
		SessionID: "session-1",
	})
	if err == nil {
		t.Fatal("Send succeeded, want 401 error")
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "status=401") {
		t.Fatalf("expected 401 status in log, got %q", logs.String())
	}
}

func TestPublishSummaryReadyUsesSummaryVersionAndMarksStatus(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret")
	defer server.Close()

	endedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			Title:          "Team Sync",
			Summary:        "Reviewed the webhook cutover.",
			StartedAt:      endedAt.Add(-45 * time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			SummaryPreset:  "default",
			SummaryVersion: 3,
		},
	}}
	publisher := NewPublisher(server.URL, "shared-secret", store)
	publisher.client.retryDelays = nil

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishPublished)
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	if got[0].delivery != "session-1.summary_ready.3" {
		t.Fatalf("delivery = %q, want session-1.summary_ready.3", got[0].delivery)
	}
}

// A regeneration that starts while version 1 is in flight moves the session
// to version 2 and resets its delivery state. The late completion of version
// 1 must not mark version 2 as published.
func TestPublishSummaryReadyLeavesANewerSummaryAlone(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret")
	defer server.Close()

	endedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			StartedAt:      endedAt.Add(-time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			Summary:        "A completed summary.",
			SummaryVersion: 1,
		},
	}}
	store.afterStatus = func(status string) {
		if status != storage.EnvoyPublishPending {
			return
		}
		sess := store.sessions["session-1"]
		sess.SummaryVersion = 2
		store.sessions["session-1"] = sess
	}
	publisher := NewPublisher(server.URL, "shared-secret", store)
	publisher.client.retryDelays = nil

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	got := requests()
	if len(got) != 1 || got[0].delivery != "session-1.summary_ready.1" {
		t.Fatalf("requests = %#v, want one delivery of version 1", got)
	}
	if status := store.statuses["session-1"]; status != storage.EnvoyPublishPending {
		t.Fatalf("publish status after a stale completion = %q, want %q left for version 2", status, storage.EnvoyPublishPending)
	}
}

func TestPublishSummaryReadyRecordsFailed4xx(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret", http.StatusUnauthorized)
	defer server.Close()

	endedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			StartedAt:      endedAt.Add(-time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			Summary:        "A completed summary.",
			SummaryVersion: 1,
		},
	}}
	publisher := NewPublisher(server.URL, "shared-secret", store)
	publisher.client.retryDelays = []time.Duration{0, 0}

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSummaryReady succeeded, want 401 error")
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishFailed {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishFailed)
	}
}

func TestPublishSummaryReadyRecordsFailed5xx(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret", http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	defer server.Close()

	endedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			StartedAt:      endedAt.Add(-time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			Summary:        "A completed summary.",
			SummaryVersion: 1,
		},
	}}
	publisher := NewPublisher(server.URL, "shared-secret", store)
	publisher.client.retryDelays = []time.Duration{0, 0}

	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSummaryReady succeeded, want 503 error")
	}
	if got := len(requests()); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishFailed {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishFailed)
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

	down, _ := newListenerTestServer(t, "shared-secret", http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	publisher := NewPublisher(down.URL, "shared-secret", store)
	publisher.client.retryDelays = nil
	if err := publisher.PublishSessionStarted(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSessionStarted succeeded while relay was down")
	}
	if err := store.EndSession("session-1", startedAt.Add(time.Minute), ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := publisher.PublishSessionEnded(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSessionEnded succeeded while relay was down")
	}
	down.Close()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	up, requests := newListenerTestServer(t, "shared-secret")
	defer up.Close()
	publisher = NewPublisher(up.URL, "shared-secret", reopened)
	publisher.client.retryDelays = nil
	if err := publisher.PublishPendingLifecycleEvents(context.Background()); err != nil {
		t.Fatalf("PublishPendingLifecycleEvents: %v", err)
	}
	got := requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[0].delivery != "session-1.session_started" || got[1].delivery != "session-1.session_ended" {
		t.Fatalf("deliveries = %#v, want stable start then end", got)
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

	server, requests := newListenerTestServer(t, "shared-secret", http.StatusServiceUnavailable, http.StatusOK)
	defer server.Close()
	publisher := NewPublisher(server.URL, "shared-secret", store)
	publisher.client.retryDelays = nil
	if err := publisher.PublishPendingLifecycleEvents(context.Background()); err == nil {
		t.Fatal("PublishPendingLifecycleEvents succeeded, want the first event error")
	}
	if got := len(requests()); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	pending, err := store.PendingEnvoyLifecycleEvents()
	if err != nil {
		t.Fatalf("PendingEnvoyLifecycleEvents: %v", err)
	}
	if len(pending) != 1 || pending[0].Type != string(eventSessionStarted) {
		t.Fatalf("pending events = %#v, want the failed start only", pending)
	}
}

func TestSummaryReadyRetries429(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret", http.StatusTooManyRequests, http.StatusOK)
	defer server.Close()
	endedAt := time.Date(2026, 11, 11, 7, 20, 0, 0, time.UTC)
	store := &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			StartedAt:      endedAt.Add(-time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			Summary:        "A completed summary.",
			SummaryVersion: 1,
		},
	}}
	publisher := NewPublisher(server.URL, "shared-secret", store)
	publisher.client.retryDelays = []time.Duration{0}
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	if got := len(requests()); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := store.statuses["session-1"]; got != storage.EnvoyPublishPublished {
		t.Fatalf("publish status = %q, want %q", got, storage.EnvoyPublishPublished)
	}
}

func TestFailedSummaryDeliversAfterSecretIsFixed(t *testing.T) {
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
	if err := store.UpdateSummary("session-1", "Title", "Summary", storage.SummaryCompleted, "default", "{}"); err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}

	listener, requests := newListenerTestServer(t, "fixed-secret")
	defer listener.Close()
	publisher := NewPublisher(listener.URL, "wrong-secret", store)
	publisher.client.retryDelays = nil
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err == nil {
		t.Fatal("PublishSummaryReady succeeded with the wrong secret")
	}
	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("GetSessionsNeedingEnvoyPublish: %v", err)
	}
	if len(ids) != 1 || ids[0] != "session-1" {
		t.Fatalf("failed summary candidates = %v, want [session-1]", ids)
	}

	publisher = NewPublisher(listener.URL, "fixed-secret", store)
	publisher.client.retryDelays = nil
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady after secret fix: %v", err)
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("successful requests = %d, want 1", got)
	}
}

// TestEmptySummaryIsNotPublished fails if the summary-ready path, which the
// startup sweep also uses, sends Envoy a completed summary with no text: the
// one a session gets when no summarizer is configured.
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
	generation, err := store.Canonicalize("session-1")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if err := store.UpdateSummaryAtGeneration("session-1", "", "", storage.SummaryCompleted, "", "{}", generation); err != nil {
		t.Fatalf("complete empty summary: %v", err)
	}

	listener, requests := newListenerTestServer(t, "shared-secret")
	defer listener.Close()
	publisher := NewPublisher(listener.URL, "shared-secret", store)
	publisher.client.retryDelays = nil
	ids, err := store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatalf("GetSessionsNeedingEnvoyPublish: %v", err)
	}
	for _, id := range ids {
		if err := publisher.PublishSummaryReady(context.Background(), id); err != nil {
			t.Fatalf("PublishSummaryReady(%s): %v", id, err)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("summary_ready requests for an empty summary = %d, want none", len(got))
	}
}

func TestCopiedListenerChecksCanRejectInvalidRequests(t *testing.T) {
	const secret = "shared-secret"
	server, _ := newListenerTestServer(t, secret)
	defer server.Close()

	const validBody = `{"event_type":"summary_ready","payload":{"session_id":"session-1","type":"summary_ready"}}`
	cases := []struct {
		name          string
		body          string
		event         string
		signingSecret string
		want          int
	}{
		{name: "wrong secret", body: validBody, event: "summary.ready", signingSecret: "other-secret", want: http.StatusUnauthorized},
		{name: "missing session id", body: `{"event_type":"summary_ready","payload":{"type":"summary_ready"}}`, event: "summary.ready", signingSecret: secret, want: http.StatusBadRequest},
		{name: "header body mismatch", body: validBody, event: "session.ended", signingSecret: secret, want: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			request.Header.Set("X-GhostWispr-Delivery", "session-1.summary_ready.1")
			request.Header.Set("X-GhostWispr-Event", tc.event)
			request.Header.Set("X-GhostWispr-Signature", signWebhook(tc.signingSecret, []byte(tc.body)))

			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			_ = response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.want)
			}
		})
	}
}

func newListenerTestServer(t *testing.T, secret string, statuses ...int) (*httptest.Server, func() []webhookRequest) {
	t.Helper()

	var mu sync.Mutex
	var requests []webhookRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		event := normalizeListenerEventType(request.Header.Get("X-GhostWispr-Event"))
		delivery := strings.TrimSpace(request.Header.Get("X-GhostWispr-Delivery"))
		signature := strings.TrimSpace(request.Header.Get("X-GhostWispr-Signature"))
		if delivery == "" || event == "" {
			http.Error(w, "missing ghostwispr headers", http.StatusBadRequest)
			return
		}
		if !hmac.Equal([]byte(signWebhook(secret, body)), []byte(signature)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		var payload webhookBody
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if bodyEvent := normalizeListenerEventType(payload.EventType); bodyEvent != "" && bodyEvent != event {
			http.Error(w, "event mismatch", http.StatusBadRequest)
			return
		}
		sessionID, _ := payload.Payload["session_id"].(string)
		if strings.TrimSpace(sessionID) == "" {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}
		payloadType, _ := payload.Payload["type"].(string)
		if payloadEvent := normalizeListenerEventType(payloadType); payloadEvent != "" && payloadEvent != event {
			http.Error(w, "event mismatch", http.StatusBadRequest)
			return
		}

		mu.Lock()
		requests = append(requests, webhookRequest{
			method:      request.Method,
			event:       request.Header.Get("X-GhostWispr-Event"),
			delivery:    delivery,
			contentType: request.Header.Get("Content-Type"),
			body:        payload,
		})
		status := http.StatusOK
		if index := len(requests) - 1; index < len(statuses) {
			status = statuses[index]
		}
		mu.Unlock()
		w.WriteHeader(status)
	}))

	return server, func() []webhookRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]webhookRequest(nil), requests...)
	}
}

func signWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func normalizeListenerEventType(event string) string {
	event = strings.TrimSpace(strings.ToLower(event))
	return strings.ReplaceAll(event, ".", "_")
}
