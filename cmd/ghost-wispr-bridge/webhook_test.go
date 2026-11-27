package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// webhookFixture is one file of testdata/webhooks: a webhook as Attendee
// v1.100.6 posts it, signed by Attendee's own sign_payload (see the README
// there for the command that made each).
type webhookFixture struct {
	Secret    string `json:"secret"`
	Signature string `json:"signature"`
	Body      string `json:"body"`
}

func webhookReadFixture(t *testing.T, name string) webhookFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "webhooks", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var f webhookFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	if f.Secret == "" || f.Signature == "" || f.Body == "" {
		t.Fatalf("fixture %s lacks its secret, signature or body", name)
	}
	return f
}

// webhookTestSecret is the secret every fixture was signed with.
const webhookTestSecret = "TzM+bJBu4w5rygTVnBniZjN3vTSYa2dVUPKXhCrCtH4="

func webhookNewReceiver(t *testing.T) (*webhookReceiver, chan webhookEvent) {
	t.Helper()
	events := make(chan webhookEvent, 64)
	r, err := newWebhookReceiver(webhookTestSecret, events, newLogger(io.Discard, "bridge", slog.LevelDebug))
	if err != nil {
		t.Fatalf("newWebhookReceiver: %v", err)
	}
	return r, events
}

// webhookPost posts body to h as Attendee does, with the signature header
// when signature is not empty, and returns the status.
func webhookPost(t *testing.T, h http.Handler, body []byte, signature string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Attendee-Webhook/1.0")
	if signature != "" {
		req.Header.Set("X-Webhook-Signature", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// webhookTaken returns the events the receiver has sent so far.
func webhookTaken(events chan webhookEvent) []webhookEvent {
	var got []webhookEvent
	for {
		select {
		case e := <-events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func webhookShow(e webhookEvent) string {
	s := fmt.Sprintf("{Trigger:%q BotID:%q", e.Trigger, e.BotID)
	if e.State != nil {
		s += fmt.Sprintf(" State:%+v", *e.State)
	}
	if e.Chat != nil {
		s += fmt.Sprintf(" Chat:{ID:%q SenderID:%q Text:%q At:%s}", e.Chat.ID, e.Chat.SenderID, e.Chat.Text, e.Chat.At.Format(time.RFC3339Nano))
	}
	if e.Participant != nil {
		s += fmt.Sprintf(" Participant:{ID:%q Name:%q Join:%t At:%s}", e.Participant.ID, e.Participant.Name, e.Participant.Join, e.Participant.At.Format(time.RFC3339Nano))
	}
	return s + "}"
}

const webhookFixtureBot = "bot_Ab12Cd34Ef56Gh78"

// webhookFixtureEvents is what each fixture must decode to, written by hand
// from the payload in the README's command.
var webhookFixtureEvents = map[string]webhookEvent{
	"state_change_joined.json": {
		Trigger: "bot.state_change", BotID: webhookFixtureBot,
		State: &stateChange{New: "joined_recording", Old: "joining", EventType: "joined_meeting",
			At: time.Date(2026, 12, 1, 17, 0, 3, 123_456_000, time.UTC)},
	},
	"state_change_auto_leave.json": {
		Trigger: "bot.state_change", BotID: webhookFixtureBot,
		State: &stateChange{New: "leaving", Old: "joined_recording", EventType: "leave_requested", EventSubType: "auto_leave_silence",
			At: time.Date(2026, 12, 1, 21, 0, 41, 7_000, time.UTC)},
	},
	"state_change_ended.json": {
		Trigger: "bot.state_change", BotID: webhookFixtureBot,
		State: &stateChange{New: "ended", Old: "post_processing", EventType: "post_processing_completed",
			At: time.Date(2026, 12, 1, 18, 2, 11, 500_000_000, time.UTC)},
	},
	"chat_message.json": {
		Trigger: "chat_messages.update", BotID: webhookFixtureBot,
		Chat: &chatMessage{
			ID:       "msg_Zx98Yw76Vu54Ts32",
			SenderID: "spaces/AAQAbc123/devices/42",
			Text:     "Zo\u00eb: <b>wispr pause</b> & \"now\" \\ \u2028second\u2029third\nfourth\tfifth \u007f \U0001F600 \u674e",
			At:       time.Date(2025, 12, 1, 17, 2, 3, 0, time.UTC),
		},
	},
	"participant_join.json": {
		Trigger: "participant_events.join_leave", BotID: webhookFixtureBot,
		Participant: &participantEvent{
			ID:   "spaces/AAQAbc123/devices/42",
			Name: "S\u00f8ren \u00d8deg\u00e5rd <host> & co",
			Join: true,
			At:   time.Date(2025, 12, 1, 17, 0, 5, 123_000_000, time.UTC),
		},
	},
	"participant_leave.json": {
		Trigger: "participant_events.join_leave", BotID: webhookFixtureBot,
		Participant: &participantEvent{
			ID:   "spaces/AAQAbc123/devices/77",
			Name: "\u00c9lodie \u2028Bront\u00eb",
			Join: false,
			At:   time.Date(2025, 12, 1, 17, 59, 59, 870_000_000, time.UTC),
		},
	},
	"bot_logs.json": {Trigger: "bot_logs.update", BotID: webhookFixtureBot},
}

// TestAWebhookSignedAsAttendeeSignsIsTaken posts every fixture, each the
// bytes Attendee sends with the signature Attendee's own sign_payload made,
// and holds that each is taken and decoded. An encoder that writes Go's JSON
// (escaping <, >, & and U+2028, writing 1.0 as 1) or keeps the body's key
// order or separators fails it.
func TestAWebhookSignedAsAttendeeSignsIsTaken(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "webhooks", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(webhookFixtureEvents) {
		t.Fatalf("found %d fixtures, want the %d this test names: %v", len(names), len(webhookFixtureEvents), names)
	}
	for _, path := range names {
		name := filepath.Base(path)
		t.Run(strings.TrimSuffix(name, ".json"), func(t *testing.T) {
			want, ok := webhookFixtureEvents[name]
			if !ok {
				t.Fatalf("fixture %s has no expected event in this test", name)
			}
			f := webhookReadFixture(t, name)
			r, events := webhookNewReceiver(t)
			if code := webhookPost(t, r, []byte(f.Body), f.Signature); code != http.StatusOK {
				t.Fatalf("status %d, want 200", code)
			}
			got := webhookTaken(events)
			if len(got) != 1 {
				t.Fatalf("receiver sent %d events, want 1", len(got))
			}
			if !reflect.DeepEqual(got[0], want) {
				t.Errorf("event\n got %s\nwant %s", webhookShow(got[0]), webhookShow(want))
			}
		})
	}
}

// TestAWebhookWithABadSignatureIsDropped holds that a body is never trusted
// unless its signature is Attendee's over that body: each case gets a refusal
// and sends nothing, while the untouched fixture, posted in the same run, is
// taken.
func TestAWebhookWithABadSignatureIsDropped(t *testing.T) {
	f := webhookReadFixture(t, "state_change_ended.json")
	sig, err := base64.StdEncoding.DecodeString(f.Signature)
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(sig)
	flipped[len(flipped)-1] ^= 0x01
	other := webhookReadFixture(t, "state_change_joined.json")

	cases := []struct {
		name, body, signature string
	}{
		{"no signature", f.Body, ""},
		{"one bit of the signature changed", f.Body, base64.StdEncoding.EncodeToString(flipped)},
		{"the signature of another body", f.Body, other.Signature},
		{"the body changed after signing", strings.Replace(f.Body, `"new_state": "ended"`, `"new_state": "fatal_error"`, 1), f.Signature},
		{"half the signature", f.Body, base64.StdEncoding.EncodeToString(sig[:len(sig)/2])},
		{"a signature that is not base64", f.Body, "not base64 at all!"},
		{"a body that is not JSON", "not json", f.Signature},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.body == f.Body && c.signature == f.Signature {
				t.Fatal("the case changes nothing")
			}
			r, events := webhookNewReceiver(t)
			code := webhookPost(t, r, []byte(c.body), c.signature)
			if code >= 200 && code < 300 {
				t.Errorf("status %d, want a refusal", code)
			}
			if got := webhookTaken(events); len(got) != 0 {
				t.Errorf("receiver sent %d events: %s", len(got), webhookShow(got[0]))
			}
		})
	}
	t.Run("control: the fixture itself is taken", func(t *testing.T) {
		r, events := webhookNewReceiver(t)
		if code := webhookPost(t, r, []byte(f.Body), f.Signature); code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
		if got := webhookTaken(events); len(got) != 1 {
			t.Fatalf("receiver sent %d events, want 1", len(got))
		}
	})
}

// webhookSignedLog makes a bot_logs.update webhook whose body is already in
// sign_payload's form (sorted keys, no spaces, ASCII only), so its signature
// is the HMAC of its own bytes, made here without the code under test.
func webhookSignedLog(t *testing.T, key string) ([]byte, string) {
	t.Helper()
	body := []byte(`{"bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"event_id":"evt0001","nonce":"n1"},` +
		`"data":{"created_at":"2026-12-01T17:05:09.250000+00:00","entry_type":"uncategorized","id":"log_1","level":"info","message":"m"},` +
		`"idempotency_key":"` + key + `","trigger":"bot_logs.update"}`)
	secret, err := base64.StdEncoding.DecodeString(webhookTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return body, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestARepeatedIdempotencyKeyIsDropped holds that Attendee's retry of a
// delivery the bridge already took is answered 200 and sends nothing, and
// that the receiver remembers only its last keys, so its memory is bounded.
func TestARepeatedIdempotencyKeyIsDropped(t *testing.T) {
	t.Run("a fixture posted twice", func(t *testing.T) {
		f := webhookReadFixture(t, "chat_message.json")
		r, events := webhookNewReceiver(t)
		for i := range 2 {
			if code := webhookPost(t, r, []byte(f.Body), f.Signature); code != http.StatusOK {
				t.Fatalf("post %d: status %d, want 200", i+1, code)
			}
		}
		if got := webhookTaken(events); len(got) != 1 {
			t.Fatalf("receiver sent %d events for one key posted twice, want 1", len(got))
		}
	})

	t.Run("only the last keys are kept", func(t *testing.T) {
		r, events := webhookNewReceiver(t)
		r.recent.max = 2
		steps := []struct {
			key   string
			taken bool
		}{
			{"k1", true},
			{"k2", true},
			{"k1", false}, // still among the last two
			{"k3", true},  // k1 is forgotten
			{"k1", true},
			{"k3", false},
		}
		for i, s := range steps {
			body, sig := webhookSignedLog(t, s.key)
			if code := webhookPost(t, r, body, sig); code != http.StatusOK {
				t.Fatalf("step %d (%s): status %d, want 200", i+1, s.key, code)
			}
			got := webhookTaken(events)
			if taken := len(got) == 1; taken != s.taken || len(got) > 1 {
				t.Fatalf("step %d (%s): receiver sent %d events, want taken=%t", i+1, s.key, len(got), s.taken)
			}
		}
		if n := len(r.recent.set); n > 2 {
			t.Errorf("receiver holds %d keys, want at most 2", n)
		}
	})
}
