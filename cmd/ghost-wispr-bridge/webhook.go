package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// webhookPath is where Attendee posts each bot's webhooks.
const webhookPath = "/attendee/webhook"

// The four triggers each bot's webhook subscribes to (Attendee v1.100.6
// WebhookTriggerTypes, bots/models.py:3339-3351).
const (
	triggerStateChange = "bot.state_change"
	triggerChat        = "chat_messages.update"
	triggerJoinLeave   = "participant_events.join_leave"
	triggerBotLogs     = "bot_logs.update"
)

// webhookTriggers is the bot's webhook subscription, in botSettingsFor.
var webhookTriggers = []string{triggerStateChange, triggerChat, triggerJoinLeave, triggerBotLogs}

// webhookKeysKept is how many idempotency keys the receiver remembers.
// Attendee makes 3 attempts of a delivery in all, a few seconds apart
// (bots/tasks/deliver_webhook_task.py:39-54, Celery's retry_backoff), so a
// repeat comes long before this many newer deliveries have.
const webhookKeysKept = 4096

// webhookBodyLimit bounds a webhook's body. Attendee's largest is a chat line
// of a few kilobytes.
const webhookBodyLimit = 1 << 20

// webhookEvent is one webhook the bridge took. Exactly the field of its
// trigger is set; bot_logs.update sets none.
type webhookEvent struct {
	Trigger     string // "bot.state_change" | "chat_messages.update" | "participant_events.join_leave" | "bot_logs.update"
	BotID       string
	State       *stateChange      // bot.state_change
	Chat        *chatMessage      // chat_messages.update
	Participant *participantEvent // participant_events.join_leave
}

// stateChange is a bot.state_change payload (bots/models.py:2240-2247): the
// states and the event's type and sub type as Attendee's API names them,
// EventSubType "" when the payload's is null, and At the event's own time
// (created_at), zero when the payload's does not parse.
type stateChange struct {
	New, Old, EventType, EventSubType string
	At                                time.Time
}

// participantEvent is a participant_events.join_leave payload
// (ParticipantEventSerializer, bots/serializers.py:2131-2142). ID is Meet's
// participant id, the participant_uuid that chat lines and per-participant
// audio carry too.
type participantEvent struct {
	ID, Name string
	Join     bool // false: a leave
	At       time.Time
}

// webhookReceiver takes Attendee's webhooks on webhookPath. It takes only a
// body whose X-Webhook-Signature is Attendee's over that body, drops a
// delivery whose idempotency key it took already, and sends each webhook it
// takes on its channel. It logs no word of a webhook: a reason, a trigger and
// a bot id at most.
type webhookReceiver struct {
	key    []byte
	events chan<- webhookEvent
	log    *slog.Logger

	mu      sync.Mutex
	recent  recentKeys
	pending map[string]chan struct{} // retries wait for the attempt currently delivering
}

// newWebhookReceiver makes the receiver. secret is BRIDGE_WEBHOOK_SECRET:
// Attendee's webhook secret in the base64 form Attendee shows it in
// (bots/projects_views.py:1164); its bytes are the HMAC key. The receiver
// blocks on events until the webhook's request ends.
func newWebhookReceiver(secret string, events chan<- webhookEvent, log *slog.Logger) (*webhookReceiver, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(secret))
	if err != nil {
		return nil, errors.New("the webhook secret is not base64")
	}
	if len(key) == 0 {
		return nil, errors.New("the webhook secret is empty")
	}
	return &webhookReceiver{
		key:     key,
		events:  events,
		log:     log,
		recent:  recentKeys{max: webhookKeysKept, set: map[string]struct{}{}},
		pending: map[string]chan struct{}{},
	}, nil
}

// webhookEnvelope is the body Attendee posts
// (bots/tasks/deliver_webhook_task.py:113-118).
type webhookEnvelope struct {
	IdempotencyKey string          `json:"idempotency_key"`
	BotID          string          `json:"bot_id"`
	Trigger        string          `json:"trigger"`
	Data           json.RawMessage `json:"data"`
}

func (r *webhookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, webhookBodyLimit))
	if err != nil {
		r.log.Warn("WEBHOOK-REFUSED", "reason", "body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !r.signedByAttendee(body, req.Header.Get("X-Webhook-Signature")) {
		r.log.Warn("WEBHOOK-REFUSED", "reason", "signature")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.IdempotencyKey == "" {
		r.log.Warn("WEBHOOK-REFUSED", "reason", "envelope")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	event, err := decodeWebhook(env)
	if err != nil {
		// Signed by Attendee, so a retry would read the same: answer 200.
		// The line names a trigger of the bridge's or "other", never a value
		// of the body.
		trigger := env.Trigger
		if !slices.Contains(webhookTriggers, trigger) {
			trigger = "other"
		}
		r.log.Warn("WEBHOOK-UNREAD", "trigger", trigger, "bot", logID(env.BotID), "error", errorClass(err))
		w.WriteHeader(http.StatusOK)
		return
	}
	for {
		r.mu.Lock()
		if _, seen := r.recent.set[env.IdempotencyKey]; seen {
			r.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if pending, delivering := r.pending[env.IdempotencyKey]; delivering {
			r.mu.Unlock()
			select {
			case <-pending:
				continue // acknowledge a delivered attempt, or take over a canceled one
			case <-req.Context().Done():
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		r.pending[env.IdempotencyKey] = make(chan struct{})
		r.mu.Unlock()
		break
	}
	status := http.StatusServiceUnavailable
	select {
	case r.events <- event:
		status = http.StatusOK
	case <-req.Context().Done():
	}
	r.mu.Lock()
	if status == http.StatusOK {
		r.recent.add(env.IdempotencyKey)
	}
	close(r.pending[env.IdempotencyKey])
	delete(r.pending, env.IdempotencyKey)
	r.mu.Unlock()
	w.WriteHeader(status)
}

// signedByAttendee tells whether signature is base64 of HMAC-SHA256, under
// the webhook secret, of body as sign_payload encodes it (D16).
func (r *webhookReceiver) signedByAttendee(body []byte, signature string) bool {
	if signature == "" {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	form, err := attendeeSigningForm(body)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, r.key)
	mac.Write(form)
	return hmac.Equal(got, mac.Sum(nil))
}

// decodeWebhook reads the payload of each of the four triggers. Another
// trigger, which no bot of the bridge subscribes to, is an error.
func decodeWebhook(env webhookEnvelope) (webhookEvent, error) {
	e := webhookEvent{Trigger: env.Trigger, BotID: env.BotID}
	switch env.Trigger {
	case triggerStateChange:
		var d struct {
			NewState     string `json:"new_state"`
			OldState     string `json:"old_state"`
			CreatedAt    string `json:"created_at"`
			EventType    string `json:"event_type"`
			EventSubType string `json:"event_sub_type"`
		}
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return e, err
		}
		at, _ := time.Parse(time.RFC3339Nano, d.CreatedAt)
		e.State = &stateChange{New: d.NewState, Old: d.OldState, EventType: d.EventType, EventSubType: d.EventSubType, At: at.UTC()}
	case triggerChat:
		var d attendeeChatLine
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return e, err
		}
		c := d.chatMessage()
		e.Chat = &c
	case triggerJoinLeave:
		var d attendeeParticipantEvent
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return e, err
		}
		p, ok := d.participantEvent()
		if !ok {
			return e, errors.New("participant event is neither a join nor a leave")
		}
		e.Participant = &p
	case triggerBotLogs:
	default:
		return e, errors.New("not a trigger of the bridge")
	}
	return e, nil
}

// recentKeys is the last max idempotency keys taken, oldest overwritten
// first, so its memory is bounded.
type recentKeys struct {
	max  int
	ring []string
	next int
	set  map[string]struct{}
}

// add records key and reports whether it was new.
func (k *recentKeys) add(key string) bool {
	if _, seen := k.set[key]; seen {
		return false
	}
	if k.ring == nil {
		k.ring = make([]string, k.max)
	}
	if old := k.ring[k.next]; old != "" {
		delete(k.set, old)
	}
	k.ring[k.next] = key
	k.next = (k.next + 1) % len(k.ring)
	k.set[key] = struct{}{}
	return true
}
