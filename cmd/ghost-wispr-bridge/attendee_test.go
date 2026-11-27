package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

const attendeeTestKey = "attendee-test-key"

// fakeAttendeeServer stands in for Attendee's web app on 127.0.0.1:8000: it
// refuses a request without the project's API key as Attendee's
// ApiKeyAuthentication does ("Authorization: Token <key>",
// bots/authentication.py), serves /api/v1 by the routes a test gives it, and
// records each request.
type fakeAttendeeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	routes   map[string]http.HandlerFunc // "METHOD /path" below /api/v1
	requests []fakeAttendeeServerRequest
}

type fakeAttendeeServerRequest struct {
	Method, Path, Query string
	Body                []byte
}

func newFakeAttendeeServer(t *testing.T) *fakeAttendeeServer {
	t.Helper()
	f := &fakeAttendeeServer{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAttendeeServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("Authorization") != "Token "+attendeeTestKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"Invalid token"}`)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api/v1")
	f.mu.Lock()
	f.requests = append(f.requests, fakeAttendeeServerRequest{Method: r.Method, Path: path, Query: r.URL.RawQuery, Body: body})
	h := f.routes[r.Method+" "+path]
	f.mu.Unlock()
	if !ok || h == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"Not found"}`)
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	w.Header().Set("Content-Type", "application/json")
	h(w, r)
}

func (f *fakeAttendeeServer) on(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

func (f *fakeAttendeeServer) calls(method, path string) []fakeAttendeeServerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeAttendeeServerRequest
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAttendeeServer) client() *attendeeClient {
	c := newAttendeeClient(f.srv.URL, attendeeTestKey)
	c.deleteDataRetry = time.Millisecond
	return c
}

// attendeeTestReply answers with status and a JSON body.
func attendeeTestReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// attendeeTestBotJSON is a bot as BotSerializer writes it
// (bots/serializers.py:1911-1998), in state.
func attendeeTestBotJSON(id, state string) string {
	return `{"id":"` + id + `","metadata":{"event_id":"evt0001","nonce":"n1"},"meeting_url":"https://meet.google.com/abc-defg-hij",` +
		`"state":"` + state + `","events":[],"transcription_state":null,"recording_state":null,"join_at":null,"deduplication_key":null}`
}

func attendeeTestJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("the expected body is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

// TestTheBotHasNoTranscriptionAndItsLeaveSettings holds the body of POST
// /api/v1/bots, key for key as Attendee v1.100.6's CreateBotSerializer takes
// it, against the spec's table "The bot's settings": the Pi's bot name, the
// event's Meet link, metadata of the event id and the nonce alone, join_at
// only when the start is more than 60 s away, no recording file, no
// transcription, the mixed audio at 24000 and the per-participant audio at
// 16000 on one socket URL, the four triggers, and the leave settings, among
// them the host wait of the seconds to the event's end but at least 600.
func TestTheBotHasNoTranscriptionAndItsLeaveSettings(t *testing.T) {
	const (
		nonce   = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		socket  = "wss://localhost:9443/attendee/audio/5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8"
		webhook = "http://localhost:9081/attendee/webhook"
	)
	now := time.Date(2026, 12, 1, 16, 58, 0, 0, time.UTC)
	texts := call.Texts{BotName: "Ghost Wispr test (recording)", Consent: "consent text"}
	body := func(joinAt string, hostWait string) string {
		return `{
			"bot_name": "Ghost Wispr test (recording)",
			"meeting_url": "https://meet.google.com/abc-defg-hij",
			"metadata": {"event_id": "evt0001_20261201T170000Z", "nonce": "` + nonce + `"},` + joinAt + `
			"recording_settings": {"format": "none"},
			"transcription_settings": {"enabled": false},
			"websocket_settings": {
				"audio": {"url": "` + socket + `", "sample_rate": 24000},
				"per_participant_audio": {"url": "` + socket + `", "sample_rate": 16000}
			},
			"webhooks": [{"url": "` + webhook + `", "triggers": ["bot.state_change", "chat_messages.update", "participant_events.join_leave", "bot_logs.update"]}],
			"automatic_leave_settings": {
				"silence_timeout_seconds": 14400,
				"only_participant_in_meeting_timeout_seconds": 14400,
				"wait_for_host_to_start_meeting_timeout_seconds": ` + hostWait + `,
				"waiting_room_timeout_seconds": 900
			},
			"google_meet_settings": {"use_login": false}
		}`
	}
	cases := []struct {
		name       string
		start, end time.Time
		want       string
	}{
		{
			name:  "start 120 s away: join_at is the start; host wait to the end",
			start: time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC),
			want:  body(`"join_at": "2026-12-01T17:00:00Z",`, "3720"),
		},
		{
			name:  "start 61 s away, in another zone: join_at in UTC",
			start: time.Date(2026, 12, 1, 8, 59, 1, 0, time.FixedZone("PST", -8*3600)),
			end:   time.Date(2026, 12, 1, 19, 0, 0, 0, time.UTC),
			want:  body(`"join_at": "2026-12-01T16:59:01Z",`, "7320"),
		},
		{
			name:  "start exactly 60 s away: no join_at; host wait at least 600",
			start: time.Date(2026, 12, 1, 16, 59, 0, 0, time.UTC),
			end:   time.Date(2026, 12, 1, 17, 5, 0, 0, time.UTC),
			want:  body("", "600"),
		},
		{
			name:  "started already: no join_at",
			start: time.Date(2026, 12, 1, 16, 30, 0, 0, time.UTC),
			end:   time.Date(2026, 12, 1, 17, 30, 0, 0, time.UTC),
			want:  body("", "1920"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeAttendeeServer(t)
			f.on(http.MethodPost, "/bots", attendeeTestReply(http.StatusCreated, attendeeTestBotJSON("bot_1", "joining")))
			ev := calendar.Event{
				ID: "evt0001_20261201T170000Z", Organizer: "organizer@example.com", MeetURL: "https://meet.google.com/abc-defg-hij",
				Start: c.start, End: c.end, Joins: 1,
			}
			if _, err := f.client().CreateBot(context.Background(), botSettingsFor(ev, texts, nonce, socket, webhook, now)); err != nil {
				t.Fatalf("CreateBot: %v", err)
			}
			posts := f.calls(http.MethodPost, "/bots")
			if len(posts) != 1 {
				t.Fatalf("Attendee got %d POST /api/v1/bots, want 1", len(posts))
			}
			attendeeTestJSONEqual(t, posts[0].Body, c.want)
		})
	}
}

// TestDeleteDataRetriesUntilAttendeeAccepts holds that DeleteData calls
// delete_data again while Attendee refuses it, as it does with 400 until the
// bot reaches ended or fatal_error (bots/bots_api_views.py:638-643), returns
// once Attendee accepts, and gives up only when its context ends.
func TestDeleteDataRetriesUntilAttendeeAccepts(t *testing.T) {
	const refusal = `{"error":"Bot is not in a state where the data deleted event can be created"}`
	t.Run("400 twice, then 200", func(t *testing.T) {
		f := newFakeAttendeeServer(t)
		var mu sync.Mutex
		n := 0
		f.on(http.MethodPost, "/bots/bot_1/delete_data", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			n++
			attempt := n
			mu.Unlock()
			if attempt <= 2 {
				attendeeTestReply(http.StatusBadRequest, refusal)(w, nil)
				return
			}
			attendeeTestReply(http.StatusOK, attendeeTestBotJSON("bot_1", "data_deleted"))(w, nil)
		})
		f.on(http.MethodGet, "/bots/bot_1", attendeeTestReply(http.StatusOK, attendeeTestBotJSON("bot_1", "post_processing")))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.client().DeleteData(ctx, "bot_1"); err != nil {
			t.Fatalf("DeleteData: %v", err)
		}
		if got := len(f.calls(http.MethodPost, "/bots/bot_1/delete_data")); got != 3 {
			t.Errorf("Attendee got %d delete_data calls, want 3", got)
		}
	})
	t.Run("refused until the context ends", func(t *testing.T) {
		f := newFakeAttendeeServer(t)
		f.on(http.MethodPost, "/bots/bot_1/delete_data", attendeeTestReply(http.StatusBadRequest, refusal))
		f.on(http.MethodGet, "/bots/bot_1", attendeeTestReply(http.StatusOK, attendeeTestBotJSON("bot_1", "leaving")))
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		err := f.client().DeleteData(ctx, "bot_1")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("DeleteData returned %v, want the context's end", err)
		}
		if got := len(f.calls(http.MethodPost, "/bots/bot_1/delete_data")); got < 2 {
			t.Errorf("Attendee got %d delete_data calls in 200 ms, want it tried again", got)
		}
	})
}

// TestDeleteDataStopsWhenTheDataIsAlreadyDeleted holds that DeleteData
// returns when upkeep, which also deletes each ended bot's data, got there
// first: Attendee then refuses delete_data with the same 400 for good, and
// the bot reads data_deleted.
func TestDeleteDataStopsWhenTheDataIsAlreadyDeleted(t *testing.T) {
	f := newFakeAttendeeServer(t)
	f.on(http.MethodPost, "/bots/bot_1/delete_data", attendeeTestReply(http.StatusBadRequest, `{"error":"Bot is not in a state where the data deleted event can be created"}`))
	f.on(http.MethodGet, "/bots/bot_1", attendeeTestReply(http.StatusOK, attendeeTestBotJSON("bot_1", "data_deleted")))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.client().DeleteData(ctx, "bot_1"); err != nil {
		t.Fatalf("DeleteData: %v", err)
	}
	if got := len(f.calls(http.MethodPost, "/bots/bot_1/delete_data")); got != 1 {
		t.Errorf("Attendee got %d delete_data calls, want 1", got)
	}
}

// TestTheAttendeeClientReadsEveryPageOfBotsAndChat holds that the client
// follows Attendee's cursor pages (CursorPagination, 25 a page,
// bots/bots_api_views.py:137-139,1050-1052) to the last, and reads each bot's
// state, its metadata's event id and nonce, its events oldest first, and each
// chat line's id, sender, text and time.
func TestTheAttendeeClientReadsEveryPageOfBotsAndChat(t *testing.T) {
	f := newFakeAttendeeServer(t)
	bot1 := `{"id":"bot_1","metadata":{"event_id":"evt0001","nonce":"n1"},"meeting_url":"https://meet.google.com/abc-defg-hij","state":"joined_recording",` +
		`"events":[{"type":"join_requested","created_at":"2026-12-01T16:59:35.000001Z"},{"type":"joined_meeting","created_at":"2026-12-01T17:00:03Z"}],` +
		`"transcription_state":null,"recording_state":"in_progress","join_at":"2026-12-01T17:00:00Z","deduplication_key":null}`
	bot2 := `{"id":"bot_2","metadata":null,"meeting_url":"https://meet.google.com/abc-defg-hij","state":"ended",` +
		`"events":[{"type":"join_requested","created_at":"2026-12-01T15:00:00Z"},{"type":"leave_requested","sub_type":"auto_leave_silence","created_at":"2026-12-01T16:00:00.5+00:00"}],` +
		`"transcription_state":null,"recording_state":null,"join_at":null,"deduplication_key":null}`
	page := func(next, results string) string {
		if next == "" {
			return `{"next":null,"previous":null,"results":[` + results + `]}`
		}
		return `{"next":"` + f.srv.URL + next + `","previous":null,"results":[` + results + `]}`
	}
	f.on(http.MethodGet, "/bots", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			attendeeTestReply(http.StatusOK, page("/api/v1/bots?cursor=cD0yMDI2", bot1))(w, r)
		case "cD0yMDI2":
			attendeeTestReply(http.StatusOK, page("", bot2))(w, r)
		default:
			attendeeTestReply(http.StatusBadRequest, `{"detail":"Invalid cursor"}`)(w, r)
		}
	})
	f.on(http.MethodGet, "/bots/bot_1", attendeeTestReply(http.StatusOK, bot1))
	line := func(id, text string, ts int64) string {
		b, _ := json.Marshal(map[string]any{
			"id": id, "text": text, "timestamp_ms": ts * 1000, "timestamp": ts, "to": "everyone",
			"sender_name": "Zo\u00eb", "sender_uuid": "spaces/AAQAbc123/devices/42", "sender_user_uuid": nil, "additional_data": map[string]any{},
		})
		return string(b)
	}
	f.on(http.MethodGet, "/bots/bot_1/chat_messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			attendeeTestReply(http.StatusOK, page("/api/v1/bots/bot_1/chat_messages?cursor=cj0x", line("msg_1", "wispr pause", 1764608400)))(w, r)
			return
		}
		attendeeTestReply(http.StatusOK, page("", line("msg_2", "<b>&</b>", 1764608401)))(w, r)
	})
	c := f.client()
	ctx := context.Background()

	wantBot1 := bot{ID: "bot_1", State: "joined_recording", EventID: "evt0001", Nonce: "n1", Events: []botEvent{
		{Type: "join_requested", At: time.Date(2026, 12, 1, 16, 59, 35, 1000, time.UTC)},
		{Type: "joined_meeting", At: time.Date(2026, 12, 1, 17, 0, 3, 0, time.UTC)},
	}}
	wantBot2 := bot{ID: "bot_2", State: "ended", Events: []botEvent{
		{Type: "join_requested", At: time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)},
		{Type: "leave_requested", SubType: "auto_leave_silence", At: time.Date(2026, 12, 1, 16, 0, 0, 500_000_000, time.UTC)},
	}}
	bots, err := c.Bots(ctx)
	if err != nil {
		t.Fatalf("Bots: %v", err)
	}
	attendeeTestSameBots(t, bots, []bot{wantBot1, wantBot2})

	one, err := c.Bot(ctx, "bot_1")
	if err != nil {
		t.Fatalf("Bot: %v", err)
	}
	attendeeTestSameBots(t, []bot{one}, []bot{wantBot1})

	lines, err := c.ChatMessages(ctx, "bot_1")
	if err != nil {
		t.Fatalf("ChatMessages: %v", err)
	}
	want := []chatMessage{
		{ID: "msg_1", SenderID: "spaces/AAQAbc123/devices/42", Text: "wispr pause", At: time.Unix(1764608400, 0)},
		{ID: "msg_2", SenderID: "spaces/AAQAbc123/devices/42", Text: "<b>&</b>", At: time.Unix(1764608401, 0)},
	}
	if len(lines) != len(want) {
		t.Fatalf("ChatMessages gave %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i := range want {
		g, w := lines[i], want[i]
		if g.ID != w.ID || g.SenderID != w.SenderID || g.Text != w.Text || !g.At.Equal(w.At) {
			t.Errorf("line %d: got %+v, want %+v", i, g, w)
		}
	}
}

func attendeeTestSameBots(t *testing.T, got, want []bot) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d bots, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		same := g.ID == w.ID && g.State == w.State && g.EventID == w.EventID && g.Nonce == w.Nonce && len(g.Events) == len(w.Events)
		for j := 0; same && j < len(w.Events); j++ {
			same = g.Events[j].Type == w.Events[j].Type && g.Events[j].SubType == w.Events[j].SubType && g.Events[j].At.Equal(w.Events[j].At)
		}
		if !same {
			t.Errorf("bot %d: got %+v, want %+v", i, g, w)
		}
	}
}

// TestTheAttendeeClientPostsChatAndLeave holds the bodies and paths of the
// chat post (BotChatMessageRequestSerializer: "message", sent to everyone by
// default, bots/serializers.py:1078-1108) and of the leave, and that a
// refusal is an error that names its status.
func TestTheAttendeeClientPostsChatAndLeave(t *testing.T) {
	f := newFakeAttendeeServer(t)
	f.on(http.MethodPost, "/bots/bot_1/send_chat_message", attendeeTestReply(http.StatusOK, ""))
	f.on(http.MethodPost, "/bots/bot_1/leave", attendeeTestReply(http.StatusOK, attendeeTestBotJSON("bot_1", "leaving")))
	f.on(http.MethodPost, "/bots/bot_2/leave", attendeeTestReply(http.StatusBadRequest, `{"error":"Bot is in state ended"}`))
	c := f.client()
	ctx := context.Background()
	if err := c.Chat(ctx, "bot_1", "Type 'wispr resume' <to> resume & go."); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	posts := f.calls(http.MethodPost, "/bots/bot_1/send_chat_message")
	if len(posts) != 1 {
		t.Fatalf("Attendee got %d chat posts, want 1", len(posts))
	}
	attendeeTestJSONEqual(t, posts[0].Body, `{"message": "Type 'wispr resume' <to> resume & go."}`)

	if err := c.Leave(ctx, "bot_1"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if got := len(f.calls(http.MethodPost, "/bots/bot_1/leave")); got != 1 {
		t.Errorf("Attendee got %d leave calls, want 1", got)
	}
	err := c.Leave(ctx, "bot_2")
	var se *attendeeStatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Fatalf("Leave of an ended bot returned %v, want an attendeeStatusError with 400", err)
	}
	if strings.Contains(err.Error(), "ended") {
		t.Errorf("the error %q carries Attendee's text; it must give only the call and the status", err)
	}
}

// TestTheAttendeeClientReadsTheJoinsAndLeaves holds that ParticipantEvents
// follows every page of GET /bots/{id}/participant_events and gives the joins
// and leaves alone, oldest first, with each participant's Meet id, name and
// time; Attendee's speech, screen share and update events on the same list
// are not joins or leaves (ParticipantEventTypes, bots/models.py:2383-2404).
func TestTheAttendeeClientReadsTheJoinsAndLeaves(t *testing.T) {
	f := newFakeAttendeeServer(t)
	event := func(id, uuid, name, kind string, ms int64) string {
		b, _ := json.Marshal(map[string]any{
			"id": id, "participant_name": name, "participant_uuid": uuid, "participant_user_uuid": nil,
			"participant_is_host": false, "event_type": kind, "event_data": map[string]any{}, "timestamp_ms": ms,
		})
		return string(b)
	}
	f.on(http.MethodGet, "/bots/bot_1/participant_events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			attendeeTestReply(http.StatusOK, `{"next":"`+f.srv.URL+`/api/v1/bots/bot_1/participant_events?cursor=cD0x","previous":null,"results":[`+
				event("pe_1", "spaces/s/devices/1", "Zo\u00eb", "join", 1764608400000)+","+
				event("pe_2", "spaces/s/devices/1", "Zo\u00eb", "speech_start", 1764608401000)+","+
				event("pe_3", "spaces/s/devices/2", "Ann <a> & b", "join", 1764608402500)+`]}`)(w, r)
			return
		}
		attendeeTestReply(http.StatusOK, `{"next":null,"previous":null,"results":[`+
			event("pe_4", "spaces/s/devices/1", "Zo\u00eb", "update", 1764608403000)+","+
			event("pe_5", "spaces/s/devices/1", "Zo\u00eb", "screenshare_start", 1764608404000)+","+
			event("pe_6", "spaces/s/devices/1", "Zo\u00eb", "leave", 1764608405000)+`]}`)(w, r)
	})
	got, err := f.client().ParticipantEvents(context.Background(), "bot_1")
	if err != nil {
		t.Fatalf("ParticipantEvents: %v", err)
	}
	want := []participantEvent{
		{ID: "spaces/s/devices/1", Name: "Zo\u00eb", Join: true, At: time.UnixMilli(1764608400000)},
		{ID: "spaces/s/devices/2", Name: "Ann <a> & b", Join: true, At: time.UnixMilli(1764608402500)},
		{ID: "spaces/s/devices/1", Name: "Zo\u00eb", Join: false, At: time.UnixMilli(1764608405000)},
	}
	if len(got) != len(want) {
		t.Fatalf("ParticipantEvents gave %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Name != w.Name || g.Join != w.Join || !g.At.Equal(w.At) {
			t.Errorf("event %d: got %+v, want %+v", i, g, w)
		}
	}
}
