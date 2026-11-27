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
	"sync"
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

// restartSentAfterHello gives the control messages the link took from n on,
// and fails unless the first is the bridge's hello (A1).
func restartSentAfterHello(t *testing.T, h *botHarness, n int) []call.Control {
	t.Helper()
	after := slices.Clone(h.link.sent[n:])
	if len(after) == 0 || after[0].Type != call.TypeHello {
		t.Fatalf("messages after the link-up = %+v, want the bridge's hello first", after)
	}
	return after
}

// An owed busy survives a restart of the bridge (R14, spec "One call at a
// time"): B's end passes while A's bot is on and the link is down, and the
// bridge stops right after B's done mark. The restarted bridge sends one busy
// for B, with B's organizer, start and end, once the link is up and after
// its hello; a second restart sends none.
func TestAnOwedBusyIsSentOnceAfterABridgeRestart(t *testing.T) {
	startA := botT0.Add(2 * time.Minute)
	evB := botCalEvent("evB", startA.Add(10*time.Minute), startA.Add(20*time.Minute))
	h := newBotHarness(t, botCalEvent("evA", startA, startA.Add(60*time.Minute)), evB)
	h.start()
	h.linkUp(botPiHello(botTexts()))
	h.inCall()
	h.advanceTo(startA.Add(15 * time.Minute))
	h.linkDown()
	// The bridge stops at B's done mark: that write takes effect, and no
	// later call of the calendar or of Attendee does.
	stop := &botStopper{armed: true}
	h.api.stop, h.cal.stop = stop, stop
	h.advanceTo(startA.Add(21 * time.Minute))
	if !stop.isDead() || !h.cal.event("evB").Done {
		t.Fatalf("B's end passed during A's call: stopped %v, B done %v; want B's done mark", stop.isDead(), h.cal.event("evB").Done)
	}
	stop.revive()
	restartBridge(h)
	h.advance(time.Minute)
	if n := len(h.link.sentOf(call.TypeBusy)); n != 0 {
		t.Fatalf("busy sent %d times while the link was down", n)
	}
	n := len(h.link.sent)
	h.linkUp(botPiHello(botTexts()))
	restartSentAfterHello(t, h, n)
	busy := h.link.sentOf(call.TypeBusy)
	if len(busy) != 1 {
		t.Fatalf("busy controls after the restarted bridge's link-up = %+v, want one for evB", busy)
	}
	if got := busy[0].Busy.Event; got.ID != "evB" || got.Organizer != evB.Organizer || !got.Start.Equal(evB.Start) || !got.End.Equal(evB.End) {
		t.Fatalf("the busy names %+v, want evB's id, organizer, start and end", got)
	}
	h.advance(time.Minute)
	restartBridge(h)
	h.linkUp(botPiHello(botTexts()))
	h.advance(time.Minute)
	if n := len(h.link.sentOf(call.TypeBusy)); n != 1 {
		t.Fatalf("busy sent %d times over two restarts, want once", n)
	}
}

// An owed failed chat_check survives a restart of the bridge (R2): the check
// fails while the link is down, the bot leaves, and the bridge restarts
// before the bot's end is seen. The restarted bridge ends the event as Done
// with no new bot, and sends one failed chat_check once the link is up and
// after its hello; a second restart sends none.
func TestAnOwedChatCheckFailureIsSentOnceAfterABridgeRestart(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	h.linkDown()
	h.advance(31 * time.Second)
	if n := len(h.api.callsOf("Leave")); n != 1 {
		t.Fatalf("Leave calls after the failed check = %d, want 1", n)
	}
	restartBridge(h)
	h.end(id, "ended", botRecord(h.clk.Now(), "left_meeting", "post_processing_completed")...)
	h.advance(time.Minute)
	if checks := h.link.sentOf(call.TypeChatCheck); len(checks) != 0 {
		t.Fatalf("chat_check sent while the link was down: %+v", checks)
	}
	if done, made := h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")); !done || made != 1 {
		t.Fatalf("after the failed check's bot ended: done %v, bots made %d; want done and no new bot", done, made)
	}
	n := len(h.link.sent)
	h.linkUp(botPiHello(botTexts()))
	restartSentAfterHello(t, h, n)
	checks := h.link.sentOf(call.TypeChatCheck)
	if len(checks) != 1 || *checks[0].ChatCheck != (call.ChatCheck{Result: "failed", Event: "ev1"}) {
		t.Fatalf("chat_check controls after the restarted bridge's link-up = %+v, want one failed for ev1", checks)
	}
	h.advance(time.Minute)
	restartBridge(h)
	h.linkUp(botPiHello(botTexts()))
	h.advance(time.Minute)
	if n := len(h.link.sentOf(call.TypeChatCheck)); n != 1 {
		t.Fatalf("chat_check sent %d times over two restarts, want once", n)
	}
}

// The bridge's own leave survives a restart of the bridge before the bot's
// end is seen (spec, the table of ends): after a leave for a lost link or for
// a failed socket the end gives a new bot once the link is back, and a leave
// by the leave rule in the lost link's place stays Done.
func TestABridgesOwnLeaveSurvivesARestartBeforeTheBotsEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		leave  func(h *botHarness)
		newBot bool
	}{
		{"a leave for a lost link", func(h *botHarness) {
			h.linkDown()
			h.advance(2 * time.Minute)
			h.b.leave(h.ctx, calendar.EndLinkLost)
			h.settle()
		}, true},
		{"a leave for a failed socket", func(h *botHarness) {
			h.sockets.setClosed(true)
			h.advance(time.Second)
			h.advance(300 * time.Second)
		}, true},
		{"a leave by the leave rule, in the lost link's place", func(h *botHarness) {
			h.linkDown()
			h.advance(2 * time.Minute)
			h.b.leave(h.ctx, calendar.EndLeaveRule)
			h.settle()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, id := joinOneBot(t)
			h.inCall()
			tc.leave(h)
			if n := len(h.api.callsOf("Leave")); n != 1 {
				t.Fatalf("Leave calls = %d, want 1", n)
			}
			restartBridge(h)
			h.end(id, "ended", botRecord(h.clk.Now(), "left_meeting", "post_processing_completed")...)
			h.linkUp(botPiHello(botTexts()))
			h.advance(time.Minute)
			made, done := len(h.api.callsOf("CreateBot")), h.cal.event("ev1").Done
			switch {
			case tc.newBot && (made != 2 || done):
				t.Fatalf("after the restart and the bot's end: bots made %d, done %v; want a new bot and the event open", made, done)
			case !tc.newBot && (made != 1 || !done):
				t.Fatalf("after the restart and the bot's end: bots made %d, done %v; want no new bot and the event done", made, done)
			}
		})
	}
}

// restartRecordCalendar is the bot's calendar that keeps each pending record
// the bridge wrote, with the count of Attendee's Leave calls before it.
type restartRecordCalendar struct {
	*fakeCalendar
	api    *fakeAttendee
	mu     sync.Mutex
	writes []restartRecordWrite
}

type restartRecordWrite struct {
	p      calendar.Pending
	leaves int
}

func (c *restartRecordCalendar) SetPending(ctx context.Context, id string, p calendar.Pending) error {
	leaves := len(c.api.callsOf("Leave"))
	c.mu.Lock()
	c.writes = append(c.writes, restartRecordWrite{p: p, leaves: leaves})
	c.mu.Unlock()
	return c.fakeCalendar.SetPending(ctx, id, p)
}

func (c *restartRecordCalendar) written() []restartRecordWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.writes)
}

// The bridge writes why it leaves on the event's record before it asks
// Attendee for the leave, with a failed chat_check the link has not taken, and
// the record keeps the reason until the bot's end is decided: a Done end
// clears it after the done mark; a new bot's end keeps it until the new bot
// is made, so a restart in between still gives the new bot. An owed control
// is cleared once the link took it.
func TestTheLeaveReasonIsOnRecordFromBeforeTheLeaveUntilTheEndIsDecided(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     calendar.BotEnd
		leave    func(h *botHarness, id string)
		owes     calendar.Pending // what the record holds beside the reason when the bridge leaves: an owed check, or the bot's passed one
		linkDown bool             // the link is down at the bot's end
		newBot   bool
	}{
		{"the leave rule", calendar.EndLeaveRule, func(h *botHarness, _ string) {
			h.inCall()
			h.advanceTo(botT0.Add(33 * time.Minute))
		}, calendar.Pending{Passed: "bot_1", Sender: "bot-device"}, false, false},
		{"a failed chat check, the link down", calendar.EndChatCheckFailed, func(h *botHarness, id string) {
			h.admit(id)
			h.linkDown()
			h.advance(31 * time.Second)
		}, calendar.Pending{ChatCheckFailed: true}, true, false},
		{"a lost link", calendar.EndLinkLost, func(h *botHarness, _ string) {
			h.inCall()
			h.linkDown()
			h.advance(2 * time.Minute)
			h.b.leave(h.ctx, calendar.EndLinkLost)
			h.settle()
		}, calendar.Pending{Passed: "bot_1", Sender: "bot-device"}, true, true},
		{"a failed socket", calendar.EndSocketFailed, func(h *botHarness, _ string) {
			h.inCall()
			h.sockets.setClosed(true)
			h.advance(time.Second)
			h.advance(300 * time.Second)
		}, calendar.Pending{Passed: "bot_1", Sender: "bot-device"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
			cal := &restartRecordCalendar{fakeCalendar: h.cal, api: h.api}
			h.calendar = cal
			h.start()
			h.linkUp(botPiHello(botTexts()))
			id := h.api.callsOf("CreateBot")[0].ID
			tc.leave(h, id)
			if n := len(h.api.callsOf("Leave")); n != 1 {
				t.Fatalf("Leave calls = %d, want 1", n)
			}
			want := tc.owes
			want.Leave, want.Bot = tc.kind, id
			if !slices.Contains(cal.written(), restartRecordWrite{p: want, leaves: 0}) {
				t.Fatalf("pending records written %+v, want %+v before Attendee's Leave", cal.written(), want)
			}
			h.end(id, "ended", botRecord(h.clk.Now(), "left_meeting", "post_processing_completed")...)
			if tc.linkDown {
				kept := tc.owes
				if tc.newBot {
					kept = want
				}
				if got := h.cal.event("ev1").Pending; got != kept {
					t.Fatalf("the record after the bot's end, the link down = %+v, want %+v", got, kept)
				}
				h.linkUp(botPiHello(botTexts()))
			}
			h.advance(confirmAfter + tickEvery) // a link keeps what the record owed
			if made, done := len(h.api.callsOf("CreateBot")), h.cal.event("ev1").Done; (made == 2) != tc.newBot || done == tc.newBot {
				t.Fatalf("after the bot's end: bots made %d, done %v; want a new bot %v", made, done, tc.newBot)
			}
			if got := h.cal.event("ev1").Pending; got != (calendar.Pending{}) {
				t.Fatalf("the record once the end is decided and the link took what it owed = %+v, want nothing pending", got)
			}
		})
	}
}

// A restart of the instance loses Attendee's bots with it (A2). The leave
// the bridge recorded for its bot still decides the event: after a failed
// chat check the event is done, with no new bot, and an owed chat_check is
// sent once the link is up; after a lost link a new bot comes, as for a bot
// that went with the instance.
func TestARestartWithNoBotLeftKeepsItsRecordedLeave(t *testing.T) {
	t.Run("a failed chat check that the Pi had", func(t *testing.T) {
		ev := restartInProgress("ev1")
		ev.Pending = calendar.Pending{Leave: calendar.EndChatCheckFailed, Bot: "bot_9"}
		h := newBotHarness(t, ev)
		h.start()
		h.linkUp(botPiHello(botTexts()))
		h.advance(time.Minute)
		if done, made := h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")); !done || made != 0 {
			t.Fatalf("done %v, bots made %d; want done and no new bot after a failed chat check", done, made)
		}
		if checks := h.link.sentOf(call.TypeChatCheck); len(checks) != 0 {
			t.Fatalf("chat_check controls = %+v, want none: the record owed none", checks)
		}
	})
	t.Run("a failed chat check", func(t *testing.T) {
		ev := restartInProgress("ev1")
		ev.Pending = calendar.Pending{Leave: calendar.EndChatCheckFailed, Bot: "bot_9", ChatCheckFailed: true}
		h := newBotHarness(t, ev)
		h.start()
		h.linkUp(botPiHello(botTexts()))
		h.advance(time.Minute)
		if done, made := h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")); !done || made != 0 {
			t.Fatalf("done %v, bots made %d; want done and no new bot after a failed chat check", done, made)
		}
		if checks := h.link.sentOf(call.TypeChatCheck); len(checks) != 1 || *checks[0].ChatCheck != (call.ChatCheck{Result: "failed", Event: "ev1"}) {
			t.Fatalf("chat_check controls = %+v, want one failed for ev1", checks)
		}
	})
	t.Run("a lost link", func(t *testing.T) {
		ev := restartInProgress("ev1")
		ev.Pending = calendar.Pending{Leave: calendar.EndLinkLost, Bot: "bot_9"}
		h := newBotHarness(t, ev)
		h.start()
		h.linkUp(botPiHello(botTexts()))
		if done, made := h.cal.event("ev1").Done, len(h.api.callsOf("CreateBot")); done || made != 1 {
			t.Fatalf("done %v, bots made %d; want a new bot after a lost link", done, made)
		}
	})
}

// A restart that finds a busy on record whose done mark had not landed sends
// it once: the restart marks the event done, so its pass does not find the
// same busy again.
func TestAnOwedBusyWhoseDoneMarkDidNotLandIsSentOnce(t *testing.T) {
	evB := botCalEvent("evB", botT0.Add(-8*time.Minute), botT0.Add(-time.Minute))
	evB.Pending = calendar.Pending{Busy: true}
	h := newBotHarness(t, restartInProgress("evA"), evB)
	h.api.bots = append(h.api.bots, &bot{ID: "bot_1", State: "joined_recording", EventID: "evA", Nonce: "nonce-1", Events: restartRecord()})
	h.api.chats["bot_1"] = []chatMessage{{ID: "c1", SenderID: "bot-device", Text: botTexts().Consent, At: botT0.Add(-5*time.Minute + 2*time.Second)}}
	h.start()
	h.linkUp(botPiHello(botTexts()))
	h.advance(time.Minute)
	if busy := h.link.sentOf(call.TypeBusy); len(busy) != 1 || busy[0].Busy.Event.ID != "evB" {
		t.Fatalf("busy controls = %+v, want one for evB", busy)
	}
	if !h.cal.event("evB").Done {
		t.Fatal("evB is not done after its busy")
	}
}

// A link's send queue takes a control before its writer writes it (C2's
// queue), so a link that drops right after it took an owed busy may have
// lost it: the busy stays on B's record, the bridge sends it again on the
// next link after its hello, and takes it off the record only once a link
// stayed up a while after taking it. The Pi drops a repeat by its event id.
func TestAnOwedBusyIsSentAgainWhenTheLinkDropsRightAfterTakingIt(t *testing.T) {
	startA := botT0.Add(2 * time.Minute)
	h := newBotHarness(t,
		botCalEvent("evA", startA, startA.Add(60*time.Minute)),
		botCalEvent("evB", startA.Add(10*time.Minute), startA.Add(20*time.Minute)))
	h.start()
	h.linkUp(botPiHello(botTexts()))
	h.inCall()
	h.advanceTo(startA.Add(15 * time.Minute))
	h.linkDown()
	h.advanceTo(startA.Add(30 * time.Minute))
	h.linkUp(botPiHello(botTexts()))
	if n := len(h.link.sentOf(call.TypeBusy)); n != 1 {
		t.Fatalf("busy sent %d times at the link-up, want once", n)
	}
	h.linkDown()
	h.advance(time.Second)
	if !h.cal.event("evB").Pending.Busy {
		t.Fatalf("B's record after the link dropped = %+v, want the busy still owed", h.cal.event("evB").Pending)
	}
	n := len(h.link.sent)
	h.linkUp(botPiHello(botTexts()))
	restartSentAfterHello(t, h, n)
	if busy := h.link.sentOf(call.TypeBusy); len(busy) != 2 || busy[1].Busy.Event.ID != "evB" {
		t.Fatalf("busy controls after the next link-up = %+v, want B's again", busy)
	}
	// A link that drops and comes back between two ticks: its hello ends
	// the link that took the busy.
	h.linkDown()
	h.linkUp(botPiHello(botTexts()))
	if n := len(h.link.sentOf(call.TypeBusy)); n != 3 {
		t.Fatalf("busy sent %d times after a link-up with no tick since the drop, want 3", n)
	}
	h.advance(time.Minute)
	if n := len(h.link.sentOf(call.TypeBusy)); n != 3 {
		t.Fatalf("busy sent %d times on a link that stayed up, want no more", n)
	}
	if got := h.cal.event("evB").Pending; got != (calendar.Pending{}) {
		t.Fatalf("B's record once a link kept the busy = %+v, want nothing pending", got)
	}
}

// A link that drops and comes back between two ticks can be up before the
// bridge takes its hello: the hello waits on Recv while the loop runs a tick.
// The busy the dropped link took is not counted received on that link-up:
// the bridge sends nothing on a link-up whose hello it has not answered, so
// the busy stays on B's record, goes again after the bridge's hello, and
// leaves the record only once the new link kept it. The control takes the
// hello before the tick. (Sol's second review of #202 at 2e217ba6,
// TestReviewReconnectBeforeHelloCannotConfirmOldSend.)
func TestAnOwedBusyIsNotConfirmedOnALinkUpWhoseHelloWaits(t *testing.T) {
	for _, tickFirst := range []bool{true, false} {
		name := "a tick before the new link's hello"
		if !tickFirst {
			name = "the new link's hello before a tick (control)"
		}
		t.Run(name, func(t *testing.T) {
			ev := botCalEvent("evB", botT0.Add(-time.Minute), botT0.Add(time.Minute))
			ev.Done, ev.Pending = true, calendar.Pending{Busy: true}
			h := newBotHarness(t, ev)
			h.start()
			hello := botPiHello(botTexts())
			h.linkUp(hello)
			if n := len(h.link.sentOf(call.TypeBusy)); n != 1 {
				t.Fatalf("busy sent %d times at the first link-up, want once", n)
			}
			h.advance(time.Second)
			h.linkDown()
			m := h.link.set(true, hello) // the new link is up; its hello waits on Recv
			if tickFirst {
				h.advance(time.Second) // confirmAfter since the dropped link took the busy
				if got := h.cal.event("evB").Pending; !got.Busy {
					t.Errorf("B's record after a tick before the new link's hello = %+v, want the busy still owed", got)
				}
			}
			h.b.onLink(h.ctx, *m)
			h.settle()
			if n := len(h.link.sentOf(call.TypeBusy)); n != 2 {
				t.Fatalf("busy sent %d times after the new link's hello, want twice: the dropped link may have lost it; B's record %+v", n, h.cal.event("evB").Pending)
			}
			if got := h.cal.event("evB").Pending; !got.Busy {
				t.Fatalf("B's record right after the busy went again = %+v, want the busy still owed", got)
			}
			h.advance(confirmAfter + tickEvery)
			if got := h.cal.event("evB").Pending; got != (calendar.Pending{}) {
				t.Fatalf("B's record once the new link kept the busy = %+v, want nothing pending", got)
			}
			if n := len(h.link.sentOf(call.TypeBusy)); n != 2 {
				t.Fatalf("busy sent %d times on a link that stayed up, want no more", n)
			}
		})
	}
}
