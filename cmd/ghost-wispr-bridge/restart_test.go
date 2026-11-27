package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// restartBridge stops the harness's bridge and starts a new one on the same
// Attendee and calendar, as a restart of the bridge's container: a new audio
// socket, and the link down until the new bridge's next hello.
func restartBridge(h *botHarness) {
	h.t.Helper()
	h.sockets = &botSockets{}
	h.linkDown()
	h.start()
}

// restartProcess is one bridge process: runBridge's loop with its own audio
// socket served over TLS, and its own link and webhook channel.
type restartProcess struct {
	link     *fakeLink
	webhooks chan webhookEvent
	srv      audiosocketServer
	stop     func()
}

// restartRun starts a bridge process on api and cal.
func restartRun(t *testing.T, s *settings, api *fakeAttendee, cal *fakeCalendar, clk *fakeClock) *restartProcess {
	t.Helper()
	certPEM, keyFile, pool := audiosocketCert(t)
	cert, err := loadAudioCert(certPEM, keyFile)
	if err != nil {
		t.Fatalf("loadAudioCert: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	in := make(chan audioIn, 64)
	sock := newAudioSocket(s.BridgeKey, in, clk.Now)
	p := &restartProcess{link: newFakeLink(), webhooks: make(chan webhookEvent),
		srv: audiosocketServer{addr: ln.Addr().String(), pool: pool}}
	ctx, cancel := context.WithCancel(t.Context())
	log := newLogger(io.Discard, "bridge", slog.LevelDebug)
	served, ran := make(chan error, 1), make(chan error, 1)
	go func() { served <- serveAudioSocket(ctx, ln, cert, sock, log) }()
	deps := bridgeDeps{API: api, Link: p.link, Calendar: cal, Clock: clk, Log: log, Webhooks: p.webhooks, Audio: in, Sockets: sock}
	go func() { ran <- runBridge(ctx, *s, deps) }()
	var once bool
	p.stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		if err := <-ran; err != nil {
			t.Errorf("runBridge: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serveAudioSocket: %v", err)
		}
	}
	t.Cleanup(p.stop)
	return p
}

// A restarted bridge makes each bot's socket secret again from the nonce in
// its metadata, so Attendee's reconnect on the bot's old URL is taken.
func TestABotsSocketSecretSurvivesABridgeRestart(t *testing.T) {
	clk := newFakeClock(botT0)
	api := newFakeAttendee(clk)
	cal := newFakeCalendar(clk, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	s := botSettingsForTest()

	first := restartRun(t, &s, api, cal, clk)
	first.link.recv <- *first.link.set(true, botPiHello(botTexts()))
	clockWaitFor(t, func() bool { return len(api.callsOf("CreateBot")) == 1 }, "the first bridge's bot")
	id := api.callsOf("CreateBot")[0].ID
	api.setBot(id, "joined_recording", botEvent{Type: "joined_meeting", At: clk.Now()})
	first.webhooks <- webhookEvent{Trigger: "bot.state_change", BotID: id, State: &stateChange{New: "joined_recording", EventType: "joined_meeting"}}
	echo := chatMessage{ID: "c1", SenderID: "bot-device", Text: botTexts().Consent, At: clk.Now()}
	api.mu.Lock()
	api.chats[id] = append(api.chats[id], echo)
	api.mu.Unlock()
	first.webhooks <- webhookEvent{Trigger: "chat_messages.update", BotID: id, Chat: &echo}
	clockWaitFor(t, func() bool { return len(first.link.sentOf(call.TypeChatCheck)) == 1 }, "the chat check")

	u, err := url.Parse(api.createdSettings()[0].SocketURL)
	if err != nil || u.Scheme != "wss" || u.Hostname() != "localhost" {
		t.Fatalf("the bot's socket URL %q is not wss on localhost: %v", api.createdSettings()[0].SocketURL, err)
	}
	conn, _, err := audiosocketDial(t, first.srv, u.Path)
	if err != nil {
		t.Fatalf("the first bridge refused its own bot's socket: %v", err)
	}
	_ = conn.Close()
	first.stop()

	second := restartRun(t, &s, api, cal, clk)
	var taken bool
	clockWaitFor(t, func() bool {
		c, _, err := audiosocketDial(t, second.srv, u.Path)
		if err == nil {
			_ = c.Close()
			taken = true
		}
		return taken
	}, "the restarted bridge to take the bot's old URL")
	// Control: the restarted bridge's socket refuses a secret it was not given.
	if c, _, err := audiosocketDial(t, second.srv, audioSocketPath+socketSecret(s.BridgeKey, "another-nonce")); err == nil {
		_ = c.Close()
		t.Fatal("the restarted bridge took a socket secret of no bot")
	}
	if n := len(api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("bots made = %d, want the one in the call", n)
	}
}

func TestARestartDuringAnEventJoinsAtOnce(t *testing.T) {
	ev := botCalEvent("ev1", botT0.Add(-10*time.Minute), botT0.Add(50*time.Minute))
	ev.Joins = 1 // the first bot went with the instance
	h := newBotHarness(t, ev)
	h.start()
	h.linkUp(botPiHello(botTexts()))
	created := h.api.callsOf("CreateBot")
	if len(created) != 1 || !created[0].At.Equal(botT0) {
		t.Fatalf("bots made at the start = %+v, want one at once", created)
	}
	if got := h.api.createdSettings()[0].JoinAt; !got.IsZero() {
		t.Fatalf("the new bot's join_at = %v, want none", got)
	}
	if got := h.cal.event("ev1").Joins; got != 2 {
		t.Fatalf("joins = %d, want 2", got)
	}
}

// A2: an event in progress at its join limit with no bot in Attendee is
// marked done at the bridge's start, so the pool can stop, also with the link
// down.
func TestARestartAtTheJoinLimitMarksTheEventDone(t *testing.T) {
	for _, tc := range []struct {
		joins    int
		wantDone bool
	}{{calendar.MaxJoins, true}, {calendar.MaxJoins - 1, false}} {
		ev := botCalEvent("ev1", botT0.Add(-10*time.Minute), botT0.Add(50*time.Minute))
		ev.Joins = tc.joins
		h := newBotHarness(t, ev)
		h.start()
		if got := h.cal.event("ev1").Done; got != tc.wantDone {
			t.Errorf("joins %d: done after the start = %v, want %v", tc.joins, got, tc.wantDone)
		}
	}
}

// A3: a restarted bridge rebuilds the roster from Attendee's join and leave
// events, so the leave rule still waits for the last person.
func TestARestartRebuildsTheRosterFromParticipantEvents(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	h.join(id, "p1", "Person One")
	h.join(id, "p2", "Person Two")
	h.advance(time.Minute)
	h.leave(id, "p1", "Person One")
	restartBridge(h)
	h.linkUp(botPiHello(botTexts()))
	h.advanceTo(botT0.Add(33 * time.Minute)) // past the end; p2 is still in the call
	if n := len(h.api.callsOf("Leave")); n != 0 {
		t.Fatalf("the restarted bridge left %d times with p2 in the call", n)
	}
	h.leave(id, "p2", "Person Two")
	h.advance(3 * time.Second)
	if n := len(h.api.callsOf("Leave")); n != 1 {
		t.Fatalf("Leave calls after p2 left = %d, want 1", n)
	}
}

func TestARestartedBridgeKeepsAPassedChatCheck(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	h.advance(5 * time.Second)
	restartBridge(h)
	h.linkUp(botPiHello(botTexts()))
	h.advance(time.Minute)
	if n := len(h.api.callsOf("Leave")); n != 0 {
		t.Fatalf("the restarted bridge left %d times after a passed chat check", n)
	}
	sent := h.link.sentOf(call.TypeHello)
	last := sent[len(sent)-1].Hello
	if last.Event == nil || last.Event.ID != "ev1" || last.BotState != "in_call" {
		t.Fatalf("the restarted bridge's hello = %+v, want ev1 in_call", last)
	}
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("bots made = %d, want the one in the call", n)
	}
	_ = id
}

// Done means MarkDone first, then delete_data: a bridge that stops between
// the two must not bring the bot back.
func TestADoneEventIsMarkedBeforeDeleteData(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	stop := &botStopper{armed: true}
	h.api.stop, h.cal.stop = stop, stop
	h.advance(time.Minute)
	h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
	if !stop.isDead() {
		t.Fatal("the bridge called neither MarkDone nor DeleteData at a Done end")
	}
	marks, deletes := h.cal.callsOf("MarkDone"), h.api.callsOf("DeleteData")
	if len(marks) != 1 || len(deletes) != 0 {
		t.Fatalf("before the stop: MarkDone %d, DeleteData %d; want MarkDone first", len(marks), len(deletes))
	}
	stop.mu.Lock()
	stop.armed, stop.dead = false, false
	stop.mu.Unlock()
	restartBridge(h)
	h.linkUp(botPiHello(botTexts()))
	h.advance(2 * time.Minute)
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("bots made after the restart = %d, want no new bot", n)
	}
	if n := len(h.api.callsOf("DeleteData")); n != 1 {
		t.Fatalf("DeleteData calls after the restart = %d, want 1", n)
	}
}

// restartInProgress is an event in progress at the bridge's start whose first
// bot the calendar counted.
func restartInProgress(id string) calendar.Event {
	ev := botCalEvent(id, botT0.Add(-10*time.Minute), botT0.Add(50*time.Minute))
	ev.Joins = 1
	return ev
}

// restartRecord is a bot's record from its join, then end.
func restartRecord(end ...string) []botEvent {
	return botRecord(botT0.Add(-5*time.Minute), append([]string{"join_requested", "joined_meeting"}, end...)...)
}

// A restarted bridge finishes the end of an event in progress from the
// newest bot Attendee holds for it, ended, fatal_error or data_deleted: the
// table of ends decides from the bot's events, which Attendee keeps after its
// data is deleted, and a Done end marks the event done before any new bot
// (R14). An older bot's end was decided when the newer bot was made.
func TestARestartFinishesAnEndWhoseDataIsDeleted(t *testing.T) {
	t.Run("a meeting that ended, its data deleted", func(t *testing.T) {
		h := newBotHarness(t, restartInProgress("ev1"))
		h.api.bots = append(h.api.bots, &bot{ID: "bot_1", State: "data_deleted", EventID: "ev1", Nonce: "nonce-1",
			Events: restartRecord("meeting_ended", "post_processing_completed", "data_deleted")})
		h.start()
		h.linkUp(botPiHello(botTexts()))
		h.advance(time.Minute)
		if !h.cal.event("ev1").Done {
			t.Fatal("ev1 is not done after its bot's meeting ended")
		}
		if n := len(h.api.callsOf("CreateBot")); n != 0 {
			t.Fatalf("bots made = %d, want none for an event whose meeting ended", n)
		}
	})
	t.Run("a denied bot whose done write fails at first", func(t *testing.T) {
		h := newBotHarness(t, restartInProgress("ev1"))
		h.api.bots = append(h.api.bots, &bot{ID: "bot_1", State: "data_deleted", EventID: "ev1", Nonce: "nonce-1",
			Events: botRecord(botT0.Add(-5*time.Minute), "join_requested", "could_not_join_meeting/request_to_join_denied", "data_deleted")})
		cal := &failingDoneCalendar{Reader: h.cal, fail: true}
		h.calendar = cal
		h.start()
		h.linkUp(botPiHello(botTexts()))
		h.advance(time.Minute)
		if n := len(h.api.callsOf("CreateBot")); n != 0 {
			t.Fatalf("bots made = %d while the done write failed, want none for a denied bot", n)
		}
		cal.fail = false
		h.advance(time.Minute)
		if !h.cal.event("ev1").Done || len(h.api.callsOf("CreateBot")) != 0 {
			t.Fatalf("after the calendar recovered: done %v, bots made %d; want done and none", h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")))
		}
	})
	// Control: a crash is a new bot also when its data is deleted.
	t.Run("a crash, its data deleted", func(t *testing.T) {
		h := newBotHarness(t, restartInProgress("ev1"))
		h.api.bots = append(h.api.bots, &bot{ID: "bot_1", State: "data_deleted", EventID: "ev1", Nonce: "nonce-1",
			Events: restartRecord("fatal_error/heartbeat_timeout", "data_deleted")})
		h.start()
		h.linkUp(botPiHello(botTexts()))
		if h.cal.event("ev1").Done || len(h.api.callsOf("CreateBot")) != 1 {
			t.Fatalf("after a crash: done %v, bots made %d; want a new bot", h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")))
		}
	})
	// Control: the leave of an older bot (the bridge's after a lost link,
	// which made the newer bot) does not end the event.
	t.Run("an older bot's leave beside a live newer bot", func(t *testing.T) {
		ev := restartInProgress("ev1")
		ev.Joins = 2
		h := newBotHarness(t, ev)
		h.api.bots = append(h.api.bots,
			&bot{ID: "bot_1", State: "data_deleted", EventID: "ev1", Nonce: "nonce-1",
				Events: restartRecord("leave_requested/user_requested", "left_meeting", "post_processing_completed", "data_deleted")},
			&bot{ID: "bot_2", State: "joining", EventID: "ev1", Nonce: "nonce-2", Events: botRecord(botT0.Add(-time.Minute), "join_requested")})
		h.start()
		h.linkUp(botPiHello(botTexts()))
		h.advance(time.Minute)
		if h.cal.event("ev1").Done || len(h.api.callsOf("CreateBot")) != 0 {
			t.Fatalf("done %v, bots made %d; want bot_2 kept for the event", h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")))
		}
	})
}

// A restarted bridge reads the roster and the chat lines of the bot in the
// call before it judges either (A3, R2): a failed read is tried again, and
// until it succeeds no echo is judged and no empty-roster leave runs.
func TestARestartRetriesItsRosterAndChatReads(t *testing.T) {
	t.Run("the roster read fails once", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.inCall()
		h.join(id, "p1", "Person One")
		h.advanceTo(botT0.Add(33 * time.Minute)) // past the end; p1 is in the call
		h.api.fail(func(a *fakeAttendee) { a.participantFails = 1 })
		restartBridge(h)
		h.linkUp(botPiHello(botTexts()))
		h.advance(5 * time.Second)
		if n := len(h.api.callsOf("Leave")); n != 0 {
			t.Fatalf("the restarted bridge left %d times with p1 in the call: its roster read had failed", n)
		}
		if failed, read := len(h.api.callsOf("ParticipantEventsFailed")), len(h.api.callsOf("ParticipantEvents")); failed != 1 || read == 0 {
			t.Fatalf("roster reads: %d failed, %d read; want the failed one tried again", failed, read)
		}
		h.leave(id, "p1", "Person One")
		h.advance(3 * time.Second)
		if n := len(h.api.callsOf("Leave")); n != 1 {
			t.Fatalf("Leave calls after p1 left = %d, want 1", n)
		}
	})
	t.Run("the chat read fails once", func(t *testing.T) {
		h, _ := joinOneBot(t)
		h.inCall()
		h.advance(5 * time.Second)
		h.api.fail(func(a *fakeAttendee) { a.chatReadFails = 1 })
		restartBridge(h)
		h.linkUp(botPiHello(botTexts()))
		h.advance(time.Minute)
		if n := len(h.api.callsOf("Leave")); n != 0 {
			t.Fatalf("the restarted bridge left %d times: it failed a passed check whose lines it could not read at first", n)
		}
		if failed, read := len(h.api.callsOf("ChatMessagesFailed")), len(h.api.callsOf("ChatMessages")); failed != 1 || read == 0 {
			t.Fatalf("chat reads: %d failed, %d read; want the failed one tried again", failed, read)
		}
		checks := h.link.sentOf(call.TypeChatCheck)
		if last := checks[len(checks)-1]; last.ChatCheck.Result != "passed" {
			t.Fatalf("the restarted bridge's chat_check = %+v, want passed", last.ChatCheck)
		}
	})
}

// At a restart only the call's bot's socket secret is taken. A second live
// bot is not registered; the bridge asks it to leave until Attendee takes
// the leave, and deletes its data at its end (R7). The bridge's leave of
// such a bot is not R1's leave rule, so its event is not done (R14).
func TestARestartTakesOnlyTheCallsBotsSocket(t *testing.T) {
	h := newBotHarness(t, restartInProgress("ev1"), restartInProgress("ev2"))
	h.api.bots = append(h.api.bots,
		&bot{ID: "bot_1", State: "joined_recording", EventID: "ev1", Nonce: "nonce-1", Events: restartRecord()},
		&bot{ID: "bot_2", State: "joining", EventID: "ev2", Nonce: "nonce-2", Events: botRecord(botT0.Add(-time.Minute), "join_requested")})
	h.api.chats["bot_1"] = []chatMessage{{ID: "c1", SenderID: "bot-device", Text: botTexts().Consent, At: botT0.Add(-5*time.Minute + 2*time.Second)}}
	h.api.leaveFails["bot_2"] = 2
	h.start()
	h.linkUp(botPiHello(botTexts()))
	if current, registered := h.sockets.taken(); current != "nonce-1" || slices.Contains(registered, "nonce-2") {
		t.Fatalf("the socket takes %q of %q, want nonce-1 alone", current, registered)
	}
	h.advance(2*pollEvery + time.Second)
	if failed, taken := h.api.callsOf("LeaveFailed"), h.api.callsOf("Leave"); len(failed) != 2 || len(taken) != 1 || taken[0].ID != "bot_2" {
		t.Fatalf("leaves: failed %+v, taken %+v; want bot_2's tried until Attendee took it", failed, taken)
	}
	h.end("bot_2", "ended", botRecord(h.clk.Now(), "left_meeting", "post_processing_completed")...)
	if deletes := h.api.callsOf("DeleteData"); len(deletes) != 1 || deletes[0].ID != "bot_2" {
		t.Fatalf("DeleteData calls %+v, want bot_2's", deletes)
	}
	if h.cal.event("ev2").Done {
		t.Fatal("ev2 is done after the bridge made its second bot leave")
	}
}

// A restarted bridge whose calendar read fails tries it again every 30 s,
// the join pass's period, and not at each second's tick: each try of the
// delegated calendar is a signJwt and a token exchange.
func TestAFailedRestartReadIsTriedEvery30s(t *testing.T) {
	h := newBotHarness(t, restartInProgress("ev1"))
	cal := &botLifeErrCalendar{Reader: h.cal}
	for range 100 {
		cal.events.errs = append(cal.events.errs, errors.New("calendar unavailable"))
	}
	h.calendar = cal
	h.start()
	h.advance(70 * time.Second)
	n := 0
	for _, m := range h.logs.messages(t) {
		if strings.HasPrefix(m, "RESTART-READ-FAILED source=calendar") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("calendar reads of the restart in 70 s = %d, want 3: at the start, 30 s and 60 s", n)
	}
}

// O4: Attendee keeps every bot of the day until the instance stops. A
// restart logs RESTART-BOT only for a bot it acts on: one it adopts, makes
// leave, or ends and deletes the data of; not for a data_deleted bot whose
// event a newer bot or its done mark decided. One RESTART-COUNT line gives
// the counts, so the lines do not grow with the day's bots.
func TestARestartLogsOnlyTheBotsItActsOn(t *testing.T) {
	done := botCalEvent("ev1", botT0.Add(-3*time.Hour), botT0.Add(-2*time.Hour))
	done.Done, done.Joins = true, 1
	ev2 := restartInProgress("ev2")
	ev2.Joins = 2
	h := newBotHarness(t, done, ev2)
	h.api.bots = append(h.api.bots,
		&bot{ID: "bot_1", State: "data_deleted", EventID: "ev1", Nonce: "nonce-1",
			Events: botRecord(botT0.Add(-3*time.Hour), "join_requested", "joined_meeting", "meeting_ended", "data_deleted")},
		&bot{ID: "bot_2", State: "data_deleted", EventID: "ev0", Nonce: "nonce-2", // an event the read no longer holds
			Events: botRecord(botT0.Add(-6*time.Hour), "join_requested", "joined_meeting", "meeting_ended", "data_deleted")},
		&bot{ID: "bot_3", State: "fatal_error", EventID: "ev2", Nonce: "nonce-3", Events: restartRecord("fatal_error/process_terminated")},
		&bot{ID: "bot_4", State: "joining", EventID: "ev2", Nonce: "nonce-4", Events: botRecord(botT0.Add(-time.Minute), "join_requested")},
		&bot{ID: "bot_5", State: "ended", Nonce: "nonce-5"}) // not a bot of ours: no event id
	h.start()
	var restartBots []string
	for _, m := range h.logs.messages(t) {
		if strings.HasPrefix(m, "RESTART-BOT ") {
			restartBots = append(restartBots, m)
		}
	}
	want := []string{
		"RESTART-BOT bot=bot_3 event=ev2 state=fatal_error",
		"RESTART-BOT bot=bot_4 event=ev2 state=joining",
	}
	if !slices.Equal(restartBots, want) {
		t.Fatalf("RESTART-BOT lines = %q, want %q: the bots the restart acts on", restartBots, want)
	}
	if !slices.Contains(h.logs.messages(t), "RESTART-COUNT bots=5 acted=2") {
		t.Fatalf("no RESTART-COUNT bots=5 acted=2 line in %q", h.logs.messages(t))
	}
	if deletes := h.api.callsOf("DeleteData"); len(deletes) != 1 || deletes[0].ID != "bot_3" {
		t.Fatalf("DeleteData calls %+v, want bot_3's alone", deletes)
	}
}
