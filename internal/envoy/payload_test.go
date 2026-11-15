package envoy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	privateTitle   = "Private title words"
	privateSummary = "The summary repeats the private deleted words."
)

// rawWebhookServer records each event's body exactly as it was sent.
func rawWebhookServer(t *testing.T) (*httptest.Server, func() map[string][]byte) {
	t.Helper()
	var mu sync.Mutex
	bodies := make(map[string][]byte)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies[normalizeListenerEventType(r.Header.Get("X-GhostWispr-Event"))] = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, func() map[string][]byte {
		mu.Lock()
		defer mu.Unlock()
		copied := make(map[string][]byte, len(bodies))
		for event, body := range bodies {
			copied[event] = body
		}
		return copied
	}
}

func privateSession() *storeStub {
	endedAt := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	return &storeStub{sessions: map[string]storage.Session{
		"session-1": {
			ID:             "session-1",
			Title:          privateTitle,
			Summary:        privateSummary,
			StartedAt:      endedAt.Add(-45 * time.Minute),
			EndedAt:        &endedAt,
			Status:         storage.SessionEnded,
			SummaryStatus:  storage.SummaryCompleted,
			SummaryPreset:  "default",
			SummaryModel:   "provider-summary-model",
			SummaryVersion: 3,
			GDriveFolderID: "folder-1",
		},
	}}
}

// TestEventsCarryNoSessionText fails if any event sends a session's title or
// summary to Envoy. Envoy keeps every event for 72 hours and no deletion can
// recall it, so a summary sent there would outlive a segment deletion that
// removed the words it repeats.
func TestEventsCarryNoSessionText(t *testing.T) {
	server, bodies := rawWebhookServer(t)
	publisher := NewPublisher(server.URL, staticKey("shared-secret"), privateSession())
	publisher.client.retryDelays = nil

	ctx := context.Background()
	if err := publisher.PublishSessionStarted(ctx, "session-1"); err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	if err := publisher.PublishSessionEnded(ctx, "session-1"); err != nil {
		t.Fatalf("PublishSessionEnded: %v", err)
	}
	if err := publisher.PublishSummaryReady(ctx, "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}

	sent := bodies()
	for _, event := range []string{"session_started", "session_ended", "summary_ready"} {
		body, ok := sent[event]
		if !ok {
			t.Fatalf("no %s event was sent; sent %d events", event, len(sent))
		}
		for _, text := range []string{privateTitle, privateSummary, "private"} {
			if strings.Contains(string(body), text) {
				t.Errorf("%s body carries session text %q: %s", event, text, body)
			}
		}
		var decoded webhookBody
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("%s body is not JSON: %v", event, err)
		}
		for _, key := range []string{"title", "summary"} {
			if _, ok := decoded.Payload[key]; ok {
				t.Errorf("%s payload has a %q field: %s", event, key, body)
			}
		}
	}
}

// TestEventsLinkTheSessionAndItsTimes fails if an event stops saying which
// session it is, when it ran, or where an agent reads its current text, or
// if the Envoy listener stand-in (newListenerTestServer, which copies its
// checks) would refuse it.
func TestEventsLinkTheSessionAndItsTimes(t *testing.T) {
	server, requests := newListenerTestServer(t, "shared-secret")
	defer server.Close()
	publisher := NewPublisher(server.URL, staticKey("shared-secret"), privateSession(), WithPublicURL("https://ghost-wispr.example.ts.net/"))
	publisher.client.retryDelays = nil
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("summary_ready requests = %d, want 1", len(got))
	}
	want := map[string]any{
		"session_id":       "session-1",
		"type":             "summary_ready",
		"started_at":       "2026-11-10T11:45:45Z",
		"ended_at":         "2026-11-10T12:30:45Z",
		"duration":         float64(2700),
		"drive_folder_url": "https://drive.google.com/drive/folders/folder-1",
		"session_url":      "https://ghost-wispr.example.ts.net/api/sessions/session-1",
		"summary_preset":   "default",
		"summary_model":    "provider-summary-model",
		"version":          float64(3),
	}
	payload := got[0].body.Payload
	for key, value := range want {
		if payload[key] != value {
			t.Errorf("payload %s = %#v, want %#v", key, payload[key], value)
		}
	}
	if len(payload) != len(want) {
		t.Errorf("payload has %d fields, want exactly %d: %#v", len(payload), len(want), payload)
	}

	// A session not yet in Drive, on a unit with no public_url, still says
	// which session it is and when it ran.
	store := privateSession()
	session := store.sessions["session-1"]
	session.GDriveFolderID = ""
	store.sessions["session-1"] = session
	server2, requests2 := newListenerTestServer(t, "shared-secret")
	defer server2.Close()
	publisher = NewPublisher(server2.URL, staticKey("shared-secret"), store)
	publisher.client.retryDelays = nil
	if err := publisher.PublishSessionStarted(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	started := requests2()
	if len(started) != 1 {
		t.Fatalf("session_started requests = %d, want 1", len(started))
	}
	for _, key := range []string{"drive_folder_url", "session_url"} {
		if _, ok := started[0].body.Payload[key]; ok {
			t.Errorf("session_started payload has %s with no folder or public_url: %#v", key, started[0].body.Payload)
		}
	}
	if started[0].body.Payload["started_at"] != "2026-11-10T11:45:45Z" {
		t.Errorf("session_started started_at = %#v", started[0].body.Payload["started_at"])
	}
}
