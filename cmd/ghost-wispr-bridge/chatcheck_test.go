package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

func TestAFailedChatCheckLeavesWithin10s(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	admitted := h.clk.Now()
	h.advance(29 * time.Second)
	if n := len(h.api.callsOf("Leave")); n != 0 {
		t.Fatalf("the bot left %d times inside the check's 30 s", n)
	}
	h.advance(11 * time.Second)
	leaves := h.api.callsOf("Leave")
	if len(leaves) != 1 {
		t.Fatalf("Leave calls by 40 s after the admission = %d, want 1", len(leaves))
	}
	if d := leaves[0].At.Sub(admitted); d < 30*time.Second || d > 40*time.Second {
		t.Fatalf("the bot left %v after the admission, want 30 to 40 s", d)
	}
}

func TestAFailedChatCheckReportsChatCheckFailed(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	h.advance(31 * time.Second)
	checks := h.link.sentOf(call.TypeChatCheck)
	if len(checks) != 1 || checks[0].ChatCheck == nil {
		t.Fatalf("chat_check controls = %+v, want one", checks)
	}
	if got := *checks[0].ChatCheck; got.Result != "failed" || got.Event != "ev1" {
		t.Fatalf("chat_check = %+v, want failed for ev1", got)
	}
	if !slices.Contains(h.logs.messages(t), "CHAT-CHECK result=failed bot=") {
		t.Fatalf("no CHAT-CHECK failed line in %q", h.logs.messages(t))
	}
}

// A chat_check result made while the link is down waits for the link, as
// busy does, so the Pi can still publish call_not_recorded with
// chat_check_failed (R2).
func TestAnUnsentChatCheckFailureWaitsForTheLink(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	h.linkDown()
	h.advance(31 * time.Second)
	if n := len(h.api.callsOf("Leave")); n != 1 {
		t.Fatalf("Leave calls after the failed check with the link down = %d, want 1", n)
	}
	if checks := h.link.sentOf(call.TypeChatCheck); len(checks) != 0 {
		t.Fatalf("chat_check sent while the link was down: %+v", checks)
	}
	h.advance(time.Minute)
	n := len(h.link.sent)
	h.linkUp(botPiHello(botTexts()))
	after := h.link.sent[n:]
	if len(after) < 2 || after[0].Type != call.TypeHello {
		t.Fatalf("messages after the link-up = %+v, want the hello, then the chat_check", after)
	}
	checks := h.link.sentOf(call.TypeChatCheck)
	if len(checks) != 1 || checks[0].ChatCheck.Result != "failed" || checks[0].ChatCheck.Event != "ev1" {
		t.Fatalf("chat_check controls after the link-up = %+v, want one failed for ev1", checks)
	}
	h.advance(time.Minute)
	if n := len(h.link.sentOf(call.TypeChatCheck)); n != 1 {
		t.Fatalf("chat_check sent %d times, want once", n)
	}
}

func TestTheEchoFromARosterSenderDoesNotPass(t *testing.T) {
	t.Run("a roster sender", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.admit(id)
		h.join(id, "p1", "Person One")
		h.chat(id, "p1", botTexts().Consent)
		h.advance(31 * time.Second)
		checks := h.link.sentOf(call.TypeChatCheck)
		if len(checks) != 1 || checks[0].ChatCheck.Result != "failed" {
			t.Fatalf("chat_check controls = %+v, want one failed", checks)
		}
		if n := len(h.api.callsOf("Leave")); n != 1 {
			t.Fatalf("Leave calls = %d, want 1", n)
		}
	})
	// Control: the same echo from a sender outside the roster passes, and a
	// line of other text from outside the roster before it does not.
	t.Run("a sender outside the roster", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.admit(id)
		h.join(id, "p1", "Person One")
		h.chat(id, "stranger-device", "a line that is not the notice")
		if checks := h.link.sentOf(call.TypeChatCheck); len(checks) != 0 {
			t.Fatalf("chat_check controls after a line of other text = %+v, want none yet", checks)
		}
		h.chat(id, "bot-device", botTexts().Consent)
		h.advance(31 * time.Second)
		checks := h.link.sentOf(call.TypeChatCheck)
		if len(checks) != 1 || checks[0].ChatCheck.Result != "passed" || checks[0].ChatCheck.Event != "ev1" {
			t.Fatalf("chat_check controls = %+v, want one passed for ev1", checks)
		}
		if !slices.Contains(h.logs.messages(t), "CHAT-CHECK result=passed bot=bot-device") {
			t.Fatalf("no CHAT-CHECK line naming the echo's sender in %q", h.logs.messages(t))
		}
		if n := len(h.api.callsOf("Leave")); n != 0 {
			t.Fatalf("Leave calls after a passed check = %d, want 0", n)
		}
	})
}

// A8: the CHAT-CHECK line names the echo's Meet sender id, which c-chat-check
// reads; a line of other text from outside the roster before the echo is
// not the echo.
func TestTheChatCheckLineNamesTheEchosSender(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	h.chat(id, "stranger-device", "a line that is not the notice")
	if checks := h.link.sentOf(call.TypeChatCheck); len(checks) != 0 {
		t.Fatalf("chat_check controls after a line of other text = %+v, want none yet", checks)
	}
	h.chat(id, "bot-device", botTexts().Consent)
	if !slices.Contains(h.logs.messages(t), "CHAT-CHECK result=passed bot=bot-device") {
		t.Fatalf("no CHAT-CHECK line naming the echo's sender in %q", h.logs.messages(t))
	}
	calls := h.link.sentOf(call.TypeCall)
	if len(calls) == 0 || calls[len(calls)-1].Call.BotState != "in_call" {
		t.Fatalf("call controls = %+v, want in_call last", calls)
	}
}

func TestChatCheckAlwaysPassesPassesAtTheAdmission(t *testing.T) {
	h, id := joinOneBotWith(t, faultSet{ChatCheckAlwaysPasses: true})
	h.admit(id)
	checks := h.link.sentOf(call.TypeChatCheck)
	if len(checks) != 1 || checks[0].ChatCheck.Result != "passed" {
		t.Fatalf("chat_check controls at the admission = %+v, want one passed", checks)
	}
	h.advance(time.Minute)
	if n := len(h.api.callsOf("Leave")); n != 0 {
		t.Fatalf("Leave calls with chat_check_always_passes = %d, want 0", n)
	}
}

func TestLongLinesPostsFiveLengthsAfterTheChatCheck(t *testing.T) {
	h, id := joinOneBotWith(t, faultSet{LongLines: true})
	h.admit(id)
	before := len(h.api.callsOf("Chat"))
	if before != 1 {
		t.Fatalf("Chat calls before the check passed = %d, want only the consent notice", before)
	}
	h.chat(id, "bot-device", botTexts().Consent)
	var lengths []int
	for _, c := range h.api.callsOf("Chat")[before:] {
		if strings.Contains(c.Text, "recording") {
			continue
		}
		lengths = append(lengths, utf8.RuneCountInString(c.Text))
	}
	if want := []int{400, 499, 500, 501, 2000}; !slices.Equal(lengths, want) {
		t.Fatalf("long lines = %v characters, want %v", lengths, want)
	}
}

// chatcheckStall stalls the calls a test turns it on for: each waits for its
// context, and the stall records the clock's time when each began.
type chatcheckStall struct {
	mu      sync.Mutex
	on      bool
	clock   clock
	reached []time.Time
}

func (s *chatcheckStall) set(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.on = on
}

// wait stalls until ctx ends while the stall is on, and reports whether it
// stalled.
func (s *chatcheckStall) wait(ctx context.Context) (bool, error) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	on := s.on
	if on {
		s.reached = append(s.reached, s.clock.Now())
	}
	s.mu.Unlock()
	if !on {
		return false, nil
	}
	<-ctx.Done()
	return true, ctx.Err()
}

func (s *chatcheckStall) first() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reached) == 0 {
		return time.Time{}, false
	}
	return s.reached[0], true
}

// chatcheckCalendar stalls the calendar's reads while its stall is on.
type chatcheckCalendar struct {
	*fakeCalendar
	stall *chatcheckStall
}

func (c *chatcheckCalendar) Events(ctx context.Context, from, to time.Time) ([]calendar.Event, error) {
	if stalled, err := c.stall.wait(ctx); stalled {
		return nil, err
	}
	return c.fakeCalendar.Events(ctx, from, to)
}

// chatcheckAttendee stalls Attendee's reads of a bot while its stall is on.
type chatcheckAttendee struct {
	*fakeAttendee
	stall *chatcheckStall
}

func (a *chatcheckAttendee) Bot(ctx context.Context, id string) (bot, error) {
	if stalled, err := a.stall.wait(ctx); stalled {
		return bot{}, err
	}
	return a.fakeAttendee.Bot(ctx, id)
}

// R2 and spec Limits: the leave after a failed chat check comes within 10 s
// of the check's 30 s also while a calendar read or a poll of the bot
// stalls across the deadline. The test runs runBridge's own loop and moves
// the clock only while the loop waits for its next tick, so a call that
// holds the loop fails it.
func TestAFailedChatCheckLeavesWithin10sWhileACallStalls(t *testing.T) {
	for _, stalled := range []string{"a calendar read", "a poll of the bot"} {
		t.Run(stalled, func(t *testing.T) {
			clk := newFakeClock(botT0)
			stall := &chatcheckStall{clock: clk}
			api := &chatcheckAttendee{fakeAttendee: newFakeAttendee(clk)}
			cal := &chatcheckCalendar{fakeCalendar: newFakeCalendar(clk, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))}
			if stalled == "a calendar read" {
				cal.stall = stall
			} else {
				api.stall = stall
			}
			link := newFakeLink()
			webhooks := make(chan webhookEvent)
			deps := bridgeDeps{API: api, Link: link, Calendar: cal, Clock: clk, Log: newLogger(io.Discard, "bridge", slog.LevelDebug),
				Webhooks: webhooks, Sockets: &botSockets{}}
			ctx, cancel := context.WithCancel(t.Context())
			ran := make(chan error, 1)
			go func() { ran <- runBridge(ctx, botSettingsForTest(), deps) }()
			defer func() { cancel(); <-ran }()
			// step moves the clock one tick once the loop waits for it.
			step := func() {
				clk.BlockUntil(t, 1)
				clk.Advance(tickEvery)
			}

			link.recv <- *link.set(true, botPiHello(botTexts()))
			clockWaitFor(t, func() bool { return len(api.callsOf("CreateBot")) == 1 }, "the bot's creation")
			id := api.callsOf("CreateBot")[0].ID
			for range 5 {
				step()
			}
			admitted := clk.Now()
			api.setBot(id, "joined_recording", botEvent{Type: "joined_meeting", At: admitted})
			webhooks <- webhookEvent{Trigger: "bot.state_change", BotID: id,
				State: &stateChange{New: "joined_recording", Old: "joining", EventType: "joined_meeting"}}
			clockWaitFor(t, func() bool { return len(api.callsOf("Chat")) == 1 }, "the consent notice")
			stall.set(true)
			for clk.Now().Before(admitted.Add(41 * time.Second)) {
				step()
			}
			clk.BlockUntil(t, 1)

			at, ok := stall.first()
			if !ok || !at.Before(admitted.Add(chatCheckWindow)) {
				t.Fatalf("the stalled call began at %v (made %v), want one before the check's deadline %v", at, ok, admitted.Add(chatCheckWindow))
			}
			leaves := api.callsOf("Leave")
			if len(leaves) != 1 {
				t.Fatalf("Leave calls by 41 s after the admission = %d, want 1", len(leaves))
			}
			if d := leaves[0].At.Sub(admitted); d < 30*time.Second || d > 40*time.Second {
				t.Fatalf("the bot left %v after the admission, want 30 to 40 s", d)
			}
		})
	}
}

// R2: the echo counts only from the consent notice's post to 30 s after the
// admission, the state change's own time, from a sender who was never in the
// roster; a restarted bridge applies the same window and roster to the chat
// lines it reads back.
func TestTheChatCheckCountsAnEchoOnlyInItsWindow(t *testing.T) {
	failed := func(t *testing.T, h *botHarness) {
		t.Helper()
		checks := h.link.sentOf(call.TypeChatCheck)
		if len(checks) == 0 || checks[len(checks)-1].ChatCheck.Result != "failed" {
			t.Fatalf("chat_check controls = %+v, want the last failed", checks)
		}
		if n := len(h.api.callsOf("Leave")); n != 1 {
			t.Fatalf("Leave calls = %d, want 1", n)
		}
	}
	t.Run("an admission webhook that came late", func(t *testing.T) {
		h, id := joinOneBot(t)
		admitted := h.clk.Now()
		h.api.setBot(id, "joined_recording", botEvent{Type: "joined_meeting", At: admitted})
		h.advance(20 * time.Second)
		h.webhook(webhookEvent{Trigger: "bot.state_change", BotID: id,
			State: &stateChange{New: "joined_recording", Old: "joining", EventType: "joined_meeting", At: admitted}})
		h.advanceTo(admitted.Add(41 * time.Second))
		leaves := h.api.callsOf("Leave")
		if len(leaves) != 1 {
			t.Fatalf("Leave calls by 41 s after the admission = %d, want 1", len(leaves))
		}
		if d := leaves[0].At.Sub(admitted); d < 30*time.Second || d > 40*time.Second {
			t.Fatalf("the bot left %v after its admission, whose webhook came 20 s late; want 30 to 40 s", d)
		}
	})
	t.Run("a copy of the notice before its post", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.chat(id, "bot-device", botTexts().Consent)
		h.advance(time.Second)
		h.admit(id)
		h.advance(31 * time.Second)
		failed(t, h)
	})
	t.Run("a copy from before the admission, read at a restart", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.admit(id)
		h.api.mu.Lock()
		h.api.chats[id] = append(h.api.chats[id], chatMessage{ID: "c0", SenderID: "bot-device", Text: botTexts().Consent, At: h.clk.Now().Add(-20 * time.Second)})
		h.api.mu.Unlock()
		h.advance(5 * time.Second)
		restartBridge(h)
		h.linkUp(botPiHello(botTexts()))
		h.advance(30 * time.Second)
		failed(t, h)
	})
	t.Run("a copy from a person who left, read at a restart", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.admit(id)
		h.advance(time.Second)
		h.join(id, "p1", "Person One")
		h.chat(id, "p1", botTexts().Consent)
		h.advance(time.Second)
		h.leave(id, "p1", "Person One")
		h.advance(2 * time.Second)
		restartBridge(h)
		h.linkUp(botPiHello(botTexts()))
		h.advance(30 * time.Second)
		failed(t, h)
	})
}
