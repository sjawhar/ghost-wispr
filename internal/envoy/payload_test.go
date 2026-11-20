package envoy

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

const (
	privateTitle   = "Private title words"
	privateSummary = "The summary repeats the private deleted words."
)

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
// removed the words it repeats. It also fails unless the links and times an
// agent reads instead reach the envelope's payload.
func TestEventsCarryNoSessionText(t *testing.T) {
	listener := newHubListener(t)
	publisher := testPublisher(t, listener, privateSession(), WithPublicURL("https://ghost-wispr.example.ts.net"))

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

	sent := listener.requests()
	if len(sent) != 3 {
		t.Fatalf("publishes = %d, want 3", len(sent))
	}
	for _, body := range sent {
		topic, _ := body["topic"].(string)
		for field, value := range body {
			text, _ := value.(string)
			for _, private := range []string{privateTitle, privateSummary, "private"} {
				if strings.Contains(text, private) {
					t.Errorf("%s %s carries session text %q: %s", topic, field, private, text)
				}
			}
		}
		payload := payloadOf(t, body)
		for _, key := range []string{"title", "summary"} {
			if _, ok := payload[key]; ok {
				t.Errorf("%s payload has a %q field: %#v", topic, key, payload)
			}
		}
		for key, want := range map[string]string{
			"session_id":       "session-1",
			"started_at":       "2026-11-10T11:45:45Z",
			"ended_at":         "2026-11-10T12:30:45Z",
			"drive_folder_url": "https://drive.google.com/drive/folders/folder-1",
			"session_url":      "https://ghost-wispr.example.ts.net/api/sessions/session-1",
		} {
			if got := payload[key]; got != want {
				t.Errorf("%s payload %s = %q, want %q", topic, key, got, want)
			}
		}
	}
}

// TestEventsLinkTheSessionAndItsTimes fails if an event stops saying which
// session it is, when it ran, or where an agent reads its current text, or
// says anything else.
func TestEventsLinkTheSessionAndItsTimes(t *testing.T) {
	listener := newHubListener(t)
	publisher := testPublisher(t, listener, privateSession(), WithPublicURL("https://ghost-wispr.example.ts.net/"))
	publisher.client.now = func() time.Time { return time.Date(2026, 11, 10, 12, 31, 0, 0, time.UTC) }
	if err := publisher.PublishSummaryReady(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSummaryReady: %v", err)
	}
	got := listener.requests()
	if len(got) != 1 {
		t.Fatalf("summary_ready publishes = %d, want 1", len(got))
	}
	want := map[string]string{
		"event_type":       "summary_ready",
		"payload_type":     "summary_ready",
		"session_id":       "session-1",
		"started_at":       "2026-11-10T11:45:45Z",
		"ended_at":         "2026-11-10T12:30:45Z",
		"duration":         "2700",
		"created_at":       "2026-11-10T12:31:00Z",
		"drive_folder_url": "https://drive.google.com/drive/folders/folder-1",
		"session_url":      "https://ghost-wispr.example.ts.net/api/sessions/session-1",
		"summary_preset":   "default",
		"summary_model":    "provider-summary-model",
		"version":          "3",
	}
	if payload := payloadOf(t, got[0]); !reflect.DeepEqual(payload, want) {
		t.Errorf("payload = %#v, want %#v", payload, want)
	}

	// A session not yet in Drive, on a unit with no public_url, still says
	// which session it is and when it ran.
	store := privateSession()
	session := store.sessions["session-1"]
	session.GDriveFolderID = ""
	store.sessions["session-1"] = session
	listener2 := newHubListener(t)
	publisher = testPublisher(t, listener2, store)
	if err := publisher.PublishSessionStarted(context.Background(), "session-1"); err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	started := listener2.requests()
	if len(started) != 1 {
		t.Fatalf("session_started publishes = %d, want 1", len(started))
	}
	payload := payloadOf(t, started[0])
	for _, key := range []string{"drive_folder_url", "session_url"} {
		if _, ok := payload[key]; ok {
			t.Errorf("session_started payload has %s with no folder or public_url: %#v", key, payload)
		}
	}
	if payload["started_at"] != "2026-11-10T11:45:45Z" {
		t.Errorf("session_started started_at = %q", payload["started_at"])
	}
}
