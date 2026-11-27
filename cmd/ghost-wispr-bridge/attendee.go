package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// attendeeAPI is the part of Attendee's REST API the bridge uses
// (Attendee v1.100.6 docs/openapi.yml, under /api/v1).
type attendeeAPI interface {
	CreateBot(ctx context.Context, s botSettings) (bot, error)
	Chat(ctx context.Context, botID, text string) error
	Leave(ctx context.Context, botID string) error
	// Remove deletes a bot that Attendee still holds for its join_at, which
	// cannot leave; Attendee refuses it with 400 once the bot launched.
	Remove(ctx context.Context, botID string) error
	DeleteData(ctx context.Context, botID string) error // tries again until Attendee accepts or ctx ends
	Bot(ctx context.Context, botID string) (bot, error)
	Bots(ctx context.Context) ([]bot, error)
	ChatMessages(ctx context.Context, botID string) ([]chatMessage, error)
	// ParticipantEvents gives the joins and leaves of the bot's call, oldest
	// first. Attendee leaves out the bot's own (bots/bots_api_views.py:1567).
	ParticipantEvents(ctx context.Context, botID string) ([]participantEvent, error)
}

// bot is one of Attendee's bots as its API gives it (BotSerializer, Attendee
// bots/serializers.py:1911-1998).
type bot struct {
	ID      string
	State   string     // Attendee's state name as its API gives it ("joining", "joined_recording", "ended", "fatal_error", "data_deleted", …)
	EventID string     // from the bot's metadata
	Nonce   string     // from the bot's metadata
	Events  []botEvent // Attendee's bot events, oldest first
}

// botEvent is one of a bot's events: its type and sub type as Attendee's API
// names them (BotEventTypes.type_to_api_code and
// BotEventSubTypes.sub_type_to_api_code, bots/models.py:1551-1693), such as
// "leave_requested" and "auto_leave_silence". SubType is "" when none.
type botEvent struct {
	Type, SubType string
	At            time.Time
}

// chatMessage is one chat line of the call (ChatMessageSerializer,
// bots/serializers.py:2105-2120).
type chatMessage struct {
	ID       string
	SenderID string // Meet's participant id of the sender
	Text     string
	At       time.Time // the chat line's time
}

// attendeeChatLine is a chat line as Attendee writes it, in the webhook's
// data and in GET /bots/{id}/chat_messages (ChatMessageSerializer,
// bots/serializers.py:2105-2120). timestamp is whole seconds.
type attendeeChatLine struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Timestamp  int64  `json:"timestamp"`
	SenderUUID string `json:"sender_uuid"`
}

func (l attendeeChatLine) chatMessage() chatMessage {
	return chatMessage{ID: l.ID, SenderID: l.SenderUUID, Text: l.Text, At: time.Unix(l.Timestamp, 0).UTC()}
}

// attendeeParticipantEvent is a participant event as Attendee writes it, in
// the webhook's data and in GET /bots/{id}/participant_events
// (ParticipantEventSerializer, bots/serializers.py:2131-2142).
type attendeeParticipantEvent struct {
	Name        string `json:"participant_name"`
	UUID        string `json:"participant_uuid"`
	EventType   string `json:"event_type"`
	TimestampMS int64  `json:"timestamp_ms"`
}

// participantEvent gives the join or leave; false for Attendee's other
// participant events (speech, screen share, update).
func (e attendeeParticipantEvent) participantEvent() (participantEvent, bool) {
	if e.EventType != "join" && e.EventType != "leave" {
		return participantEvent{}, false
	}
	return participantEvent{ID: e.UUID, Name: e.Name, Join: e.EventType == "join", At: time.UnixMilli(e.TimestampMS).UTC()}, true
}

// The bot's metadata keys: the call's event id and the bot's nonce, from
// which the bridge makes the bot's socket secret again after a restart.
const (
	botMetadataEventID = "event_id"
	botMetadataNonce   = "nonce"
)

// The bot's settings that do not depend on the call (spec, "The bot's
// settings").
const (
	// botLeaveSeconds is Attendee's silence and only-participant timeouts:
	// the pool's 4-hour hold, so the bridge's own leave rule acts first.
	botLeaveSeconds = int(calendar.PoolHold / time.Second)
	// botHostWaitMinSeconds is the least host wait, Attendee's default
	// (bots/serializers.py:973-977).
	botHostWaitMinSeconds = 600
	// botWaitingRoomSeconds is Attendee's default waiting room timeout
	// (bots/serializers.py:978-982): a bot nobody admits in 15 minutes ends.
	botWaitingRoomSeconds = 900
	// The highest rates Attendee takes (bots/serializers.py:1153-1175).
	botMixedRate          = 24000
	botPerParticipantRate = 16000
)

// botSettings is one bot's creation request: what depends on the call. Its
// JSON form is the body of POST /api/v1/bots, each key as Attendee v1.100.6's
// CreateBotSerializer takes it (bots/serializers.py:1366-1908), with the
// settings every bot shares.
type botSettings struct {
	BotName    string
	MeetingURL string
	JoinAt     time.Time // zero: no join_at, so Attendee launches the bot at once
	EventID    string
	Nonce      string

	SocketURL       string // the audio socket, for the mixed and the per-participant audio
	WebhookURL      string
	HostWaitSeconds int
}

// botSettingsFor gives the spec's table "The bot's settings" for the event:
// the Pi's bot name; the event's Meet link; metadata of the event id and the
// nonce alone; join_at the start only when the start is more than 60 s away
// (Attendee refuses a join_at in the past, bots/serializers.py:603-614); no
// recording file and no transcription; the mixed audio at 24000 and the
// per-participant audio at 16000 on the one socket URL; the bot's own webhook
// with the four triggers; Attendee's automatic leave held off for the pool's
// hold, the host wait the seconds to the event's end but at least 600, the
// waiting room 900; and no sign-in, until INFRA-3416's signed-in join lands.
//
//nolint:gocritic // hugeParam: the contract passes calendar.Event and call.Texts by value
func botSettingsFor(ev calendar.Event, texts call.Texts, nonce string, socketURL, webhookURL string, now time.Time) botSettings {
	s := botSettings{
		BotName:         texts.BotName,
		MeetingURL:      ev.MeetURL,
		EventID:         ev.ID,
		Nonce:           nonce,
		SocketURL:       socketURL,
		WebhookURL:      webhookURL,
		HostWaitSeconds: max(int(ev.End.Sub(now)/time.Second), botHostWaitMinSeconds),
	}
	if ev.Start.Sub(now) > calendar.JoinAtMin {
		s.JoinAt = ev.Start.UTC()
	}
	return s
}

type attendeeSocketSettings struct {
	URL        string `json:"url"`
	SampleRate int    `json:"sample_rate"`
}

type attendeeWebhookSettings struct {
	URL      string   `json:"url"`
	Triggers []string `json:"triggers"`
}

// attendeeCreateBot is the body of POST /api/v1/bots.
type attendeeCreateBot struct {
	BotName    string            `json:"bot_name"`
	MeetingURL string            `json:"meeting_url"`
	Metadata   map[string]string `json:"metadata"`
	JoinAt     *time.Time        `json:"join_at,omitempty"`

	RecordingSettings struct {
		Format string `json:"format"`
	} `json:"recording_settings"`
	TranscriptionSettings struct {
		Enabled bool `json:"enabled"`
	} `json:"transcription_settings"`
	WebsocketSettings struct {
		Audio               attendeeSocketSettings `json:"audio"`
		PerParticipantAudio attendeeSocketSettings `json:"per_participant_audio"`
	} `json:"websocket_settings"`
	Webhooks               []attendeeWebhookSettings `json:"webhooks"`
	AutomaticLeaveSettings struct {
		SilenceTimeoutSeconds                  int `json:"silence_timeout_seconds"`
		OnlyParticipantInMeetingTimeoutSeconds int `json:"only_participant_in_meeting_timeout_seconds"`
		WaitForHostToStartMeetingTimeout       int `json:"wait_for_host_to_start_meeting_timeout_seconds"`
		WaitingRoomTimeoutSeconds              int `json:"waiting_room_timeout_seconds"`
	} `json:"automatic_leave_settings"`
	GoogleMeetSettings struct {
		UseLogin bool `json:"use_login"`
	} `json:"google_meet_settings"`
}

// MarshalJSON writes the settings as the body of POST /api/v1/bots.
//
//nolint:gocritic // hugeParam: the contract passes botSettings by value, and json.Marshal of a value calls only a value method
func (s botSettings) MarshalJSON() ([]byte, error) {
	var b attendeeCreateBot
	b.BotName = s.BotName
	b.MeetingURL = s.MeetingURL
	b.Metadata = map[string]string{botMetadataEventID: s.EventID, botMetadataNonce: s.Nonce}
	if !s.JoinAt.IsZero() {
		joinAt := s.JoinAt.UTC()
		b.JoinAt = &joinAt
	}
	b.RecordingSettings.Format = "none"
	b.TranscriptionSettings.Enabled = false
	b.WebsocketSettings.Audio = attendeeSocketSettings{URL: s.SocketURL, SampleRate: botMixedRate}
	b.WebsocketSettings.PerParticipantAudio = attendeeSocketSettings{URL: s.SocketURL, SampleRate: botPerParticipantRate}
	b.Webhooks = []attendeeWebhookSettings{{URL: s.WebhookURL, Triggers: webhookTriggers}}
	b.AutomaticLeaveSettings.SilenceTimeoutSeconds = botLeaveSeconds
	b.AutomaticLeaveSettings.OnlyParticipantInMeetingTimeoutSeconds = botLeaveSeconds
	b.AutomaticLeaveSettings.WaitForHostToStartMeetingTimeout = s.HostWaitSeconds
	b.AutomaticLeaveSettings.WaitingRoomTimeoutSeconds = botWaitingRoomSeconds
	b.GoogleMeetSettings.UseLogin = false // INFRA-3416's signed-in join turns it on
	return json.Marshal(b)
}

// attendeeTimeout bounds one request to Attendee's web app, which answers
// within about 1 s even when it waits on a bot (bots/bots_api_views.py:1403-1420).
const attendeeTimeout = 15 * time.Second

// attendeeResponseLimit bounds a response's body: a page holds 25 items.
const attendeeResponseLimit = 8 << 20

// attendeeClient is the bridge's client of Attendee's REST API, over
// <BRIDGE_ATTENDEE_URL>/api/v1 with the project's API key.
type attendeeClient struct {
	base string
	key  string
	http *http.Client

	// deleteDataRetry is the wait between DeleteData's tries.
	deleteDataRetry time.Duration
}

// newAttendeeClient makes the client. baseURL is BRIDGE_ATTENDEE_URL, such as
// http://127.0.0.1:8000; apiKey is BRIDGE_ATTENDEE_API_KEY.
func newAttendeeClient(baseURL, apiKey string) *attendeeClient {
	return &attendeeClient{
		base:            strings.TrimRight(baseURL, "/") + "/api/v1",
		key:             apiKey,
		http:            &http.Client{Timeout: attendeeTimeout},
		deleteDataRetry: 5 * time.Second,
	}
}

// attendeeStatusError is an answer of Attendee other than the one expected.
// It names the call and the status only: Attendee's error text can repeat
// what the request held.
type attendeeStatusError struct {
	Method, Path string
	Status       int
}

func (e *attendeeStatusError) Error() string {
	return fmt.Sprintf("attendee: %s %s: status %d", e.Method, e.Path, e.Status)
}

// call sends one request to path below /api/v1 with in as its JSON body (nil:
// none), and decodes the answer into out (nil: none) when its status is want.
func (c *attendeeClient) call(ctx context.Context, method, path string, in, out any, want int) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, attendeeResponseLimit)
	if resp.StatusCode != want {
		_, _ = io.Copy(io.Discard, limited)
		return &attendeeStatusError{Method: method, Path: pathOnly(path), Status: resp.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, limited)
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("attendee: %s %s: %w", method, pathOnly(path), err)
	}
	return nil
}

func pathOnly(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

func botPath(botID string, rest string) string {
	return "/bots/" + url.PathEscape(botID) + rest
}

// attendeeBot is a bot as BotSerializer writes it.
type attendeeBot struct {
	ID       string         `json:"id"`
	State    string         `json:"state"`
	Metadata map[string]any `json:"metadata"`
	Events   []struct {
		Type      string    `json:"type"`
		SubType   string    `json:"sub_type"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"events"`
}

func (b *attendeeBot) bot() bot {
	out := bot{ID: b.ID, State: b.State}
	out.EventID, _ = b.Metadata[botMetadataEventID].(string)
	out.Nonce, _ = b.Metadata[botMetadataNonce].(string)
	if len(b.Events) > 0 {
		out.Events = make([]botEvent, len(b.Events))
		for i, e := range b.Events {
			out.Events[i] = botEvent{Type: e.Type, SubType: e.SubType, At: e.CreatedAt}
		}
	}
	return out
}

// attendeePage is one page of a list (Django REST framework's
// CursorPagination: next is the next page's URL, null on the last).
type attendeePage[T any] struct {
	Next    *string `json:"next"`
	Results []T     `json:"results"`
}

// attendeeList reads every page of the list at path. It takes only the next
// page's query from Attendee's next URL, whose host is the one Attendee was
// asked by.
func attendeeList[T any](ctx context.Context, c *attendeeClient, path string) ([]T, error) {
	var all []T
	query := ""
	for {
		var page attendeePage[T]
		if err := c.call(ctx, http.MethodGet, path+query, nil, &page, http.StatusOK); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if page.Next == nil || *page.Next == "" {
			return all, nil
		}
		next, err := url.Parse(*page.Next)
		if err != nil || next.RawQuery == "" || "?"+next.RawQuery == query {
			return nil, fmt.Errorf("attendee: GET %s: a next page without a new cursor", path)
		}
		query = "?" + next.RawQuery
	}
}

//nolint:gocritic // hugeParam: the contract passes botSettings by value
func (c *attendeeClient) CreateBot(ctx context.Context, s botSettings) (bot, error) {
	var b attendeeBot
	if err := c.call(ctx, http.MethodPost, "/bots", s, &b, http.StatusCreated); err != nil {
		return bot{}, err
	}
	return b.bot(), nil
}

// Chat posts text in the call's chat, to everyone.
func (c *attendeeClient) Chat(ctx context.Context, botID, text string) error {
	return c.call(ctx, http.MethodPost, botPath(botID, "/send_chat_message"), map[string]string{"message": text}, nil, http.StatusOK)
}

// Leave makes the bot leave its call (a leave_requested event with the sub
// type user_requested, bots/bots_api_views.py:678).
func (c *attendeeClient) Leave(ctx context.Context, botID string) error {
	return c.call(ctx, http.MethodPost, botPath(botID, "/leave"), nil, nil, http.StatusOK)
}

// Remove deletes a scheduled bot (DELETE /api/v1/bots/{id},
// bots/bots_api_views.py:1036-1047, bots/bots_api_utils.py:463-488). A bot in
// another state gets 400: Leave takes it.
func (c *attendeeClient) Remove(ctx context.Context, botID string) error {
	return c.call(ctx, http.MethodDelete, botPath(botID, ""), nil, nil, http.StatusOK)
}

// DeleteData calls delete_data until Attendee accepts it, waiting
// deleteDataRetry between tries, or ctx ends. Attendee refuses with 400 until
// the bot is ended or fatal_error, and also once its data is deleted
// (bots/models.py:999-1007, bots/bots_api_views.py:638-643); after a 400 the
// client reads the bot, and a bot in data_deleted, whose data upkeep deleted
// first, counts as done. A bot Attendee does not have (404) is an error at
// once.
func (c *attendeeClient) DeleteData(ctx context.Context, botID string) error {
	for {
		err := c.call(ctx, http.MethodPost, botPath(botID, "/delete_data"), nil, nil, http.StatusOK)
		if err == nil {
			return nil
		}
		var se *attendeeStatusError
		if errors.As(err, &se) {
			if se.Status == http.StatusNotFound {
				return err
			}
			if se.Status == http.StatusBadRequest {
				if b, berr := c.Bot(ctx, botID); berr == nil && b.State == stateDataDeleted {
					return nil
				}
			}
		}
		t := time.NewTimer(c.deleteDataRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("delete_data of %s: %w (last try: %w)", botID, ctx.Err(), err)
		case <-t.C:
		}
	}
}

func (c *attendeeClient) Bot(ctx context.Context, botID string) (bot, error) {
	var b attendeeBot
	if err := c.call(ctx, http.MethodGet, botPath(botID, ""), nil, &b, http.StatusOK); err != nil {
		return bot{}, err
	}
	return b.bot(), nil
}

// Bots reads every bot of the project, oldest first.
func (c *attendeeClient) Bots(ctx context.Context) ([]bot, error) {
	list, err := attendeeList[attendeeBot](ctx, c, "/bots")
	if err != nil {
		return nil, err
	}
	out := make([]bot, len(list))
	for i := range list {
		out[i] = list[i].bot()
	}
	return out, nil
}

// ChatMessages reads every chat line of the bot's call, oldest first.
func (c *attendeeClient) ChatMessages(ctx context.Context, botID string) ([]chatMessage, error) {
	list, err := attendeeList[attendeeChatLine](ctx, c, botPath(botID, "/chat_messages"))
	if err != nil {
		return nil, err
	}
	out := make([]chatMessage, len(list))
	for i, l := range list {
		out[i] = l.chatMessage()
	}
	return out, nil
}

// ParticipantEvents reads the joins and leaves of the bot's call, oldest
// first, and skips Attendee's other participant events.
func (c *attendeeClient) ParticipantEvents(ctx context.Context, botID string) ([]participantEvent, error) {
	list, err := attendeeList[attendeeParticipantEvent](ctx, c, botPath(botID, "/participant_events"))
	if err != nil {
		return nil, err
	}
	var out []participantEvent
	for _, e := range list {
		if p, ok := e.participantEvent(); ok {
			out = append(out, p)
		}
	}
	return out, nil
}
