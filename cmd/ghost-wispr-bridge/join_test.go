package main

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// joinOneBot starts a bridge for one event that starts 2 minutes after the
// bridge, puts the link up with the Pi's texts, and gives the bot's id.
func joinOneBot(t *testing.T) (*botHarness, string) {
	t.Helper()
	return joinOneBotWith(t, faultSet{})
}

// joinOneBotWith is joinOneBot for a bridge with faults.
func joinOneBotWith(t *testing.T, f faultSet) (*botHarness, string) {
	t.Helper()
	h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	h.faults = f
	h.start()
	h.linkUp(botPiHello(botTexts()))
	created := h.api.callsOf("CreateBot")
	if len(created) != 1 {
		t.Fatalf("CreateBot calls at the link-up = %d, want 1", len(created))
	}
	return h, created[0].ID
}

// The bot's name is the Pi's bot_name: a name no fixture holds, so a name
// written into the bridge cannot pass.
func TestTheBotGetsItsNameFromThePi(t *testing.T) {
	const name = "Bot name 4417 that only this test gives"
	texts := botTexts()
	texts.BotName = name
	h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	h.start()
	h.linkUp(botPiHello(texts))
	if created := h.api.createdSettings(); len(created) != 1 || created[0].BotName != name {
		t.Fatalf("bots made = %+v, want one named %q, the Pi's bot_name", created, name)
	}
}

func TestNoBotBeforeThePisTexts(t *testing.T) {
	h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	h.start()
	h.linkUp(botPiHello(nil))
	h.advance(3 * time.Minute)
	if n := len(h.api.callsOf("CreateBot")); n != 0 {
		t.Fatalf("CreateBot calls before the Pi's texts = %d, want 0", n)
	}
	if n := len(h.cal.callsOf("AddJoin")); n != 0 {
		t.Fatalf("AddJoin calls before the Pi's texts = %d, want 0", n)
	}
	h.linkUp(botPiHello(botTexts()))
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("CreateBot calls at the hello with texts = %d, want 1", n)
	}
}

func TestTheBotIsMadeWithin5MinutesOfTheStart(t *testing.T) {
	start := botT0.Add(20 * time.Minute)
	h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
	h.start()
	h.linkUp(botPiHello(botTexts()))
	h.advanceTo(start.Add(-5 * time.Minute).Add(-time.Second))
	if n := len(h.api.callsOf("CreateBot")); n != 0 {
		t.Fatalf("a bot was made %v before the start", start.Sub(h.api.callsOf("CreateBot")[0].At))
	}
	h.advanceTo(start.Add(-5 * time.Minute).Add(30 * time.Second))
	created := h.api.callsOf("CreateBot")
	if len(created) != 1 {
		t.Fatalf("CreateBot calls by 4m30s before the start = %d, want 1", len(created))
	}
	if lead := start.Sub(created[0].At); lead > 5*time.Minute {
		t.Fatalf("the bot was made %v before the start, more than 5 minutes", lead)
	}
}

func TestJoinAtIsTheStartOnlyWhenItIsMoreThan60sAway(t *testing.T) {
	t.Run("made 5 minutes ahead", func(t *testing.T) {
		start := botT0.Add(5 * time.Minute)
		h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
		h.start()
		h.linkUp(botPiHello(botTexts()))
		if got := h.api.createdSettings()[0].JoinAt; !got.Equal(time.Date(2026, 12, 1, 16, 5, 0, 0, time.UTC)) {
			t.Fatalf("join_at = %v, want the start 2026-12-01T16:05:00Z", got)
		}
	})
	t.Run("made 30 s ahead", func(t *testing.T) {
		start := botT0.Add(30 * time.Second)
		h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
		h.start()
		h.linkUp(botPiHello(botTexts()))
		if got := h.api.createdSettings()[0].JoinAt; !got.IsZero() {
			t.Fatalf("join_at = %v, want none 30 s before the start", got)
		}
	})
}

func TestNoBotWhileTheLinkIsDown(t *testing.T) {
	start := botT0.Add(2 * time.Minute)
	h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
	h.start()
	// The Pi's last hello gave texts, but the link is down.
	h.link.set(false, botPiHello(botTexts()))
	h.advanceTo(start.Add(31 * time.Minute))
	if n := len(h.api.callsOf("CreateBot")); n != 0 {
		t.Fatalf("CreateBot calls while the link was down = %d, want 0", n)
	}
}

func TestABotFollowsWithin60sOfALinkUp(t *testing.T) {
	start := botT0.Add(2 * time.Minute)
	h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
	h.start()
	h.advanceTo(start.Add(10 * time.Minute))
	upAt := h.clk.Now()
	h.linkUp(botPiHello(botTexts()))
	h.advance(60 * time.Second)
	created := h.api.callsOf("CreateBot")
	if len(created) != 1 {
		t.Fatalf("CreateBot calls in the 60 s after the link-up = %d, want 1", len(created))
	}
	if d := created[0].At.Sub(upAt); d > 60*time.Second {
		t.Fatalf("the bot followed the link-up after %v", d)
	}
	if got := h.api.createdSettings()[0].JoinAt; !got.IsZero() {
		t.Fatal("a bot made during its event has a join_at")
	}
}

func TestNoBotAfterTheEventsEnd(t *testing.T) {
	start := botT0.Add(2 * time.Minute)
	h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
	h.start()
	h.advanceTo(start.Add(30 * time.Minute))
	h.linkUp(botPiHello(botTexts()))
	h.advance(2 * time.Minute)
	if n := len(h.api.callsOf("CreateBot")); n != 0 {
		t.Fatalf("CreateBot calls after the event's end = %d, want 0", n)
	}
}

func TestASecondEventWaitsForTheFirstBotToLeave(t *testing.T) {
	startA := botT0.Add(2 * time.Minute)
	startB := startA.Add(20 * time.Minute)
	h := newBotHarness(t,
		botCalEvent("evA", startA, startA.Add(30*time.Minute)),
		botCalEvent("evB", startB, startB.Add(40*time.Minute)))
	h.start()
	h.linkUp(botPiHello(botTexts()))
	botA := h.inCall()
	h.join(botA, "p1", "Person One")
	h.advanceTo(startA.Add(35 * time.Minute))
	if created := h.api.callsOf("CreateBot"); len(created) != 1 {
		t.Fatalf("CreateBot calls while A's bot was on = %d, want 1", len(created))
	}
	h.leave(botA, "p1", "Person One")
	h.advance(2 * time.Second)
	leftAt := h.clk.Now()
	h.end(botA, "ended", botRecord(leftAt, "leave_requested/user_requested", "left_meeting", "post_processing_completed")...)
	h.advance(60 * time.Second)
	created := h.api.callsOf("CreateBot")
	if len(created) != 2 || created[1].Text != "evB" {
		t.Fatalf("bots after A's bot left = %+v, want one for evB", created)
	}
	if d := created[1].At.Sub(leftAt); d > 60*time.Second {
		t.Fatalf("B's bot came %v after A's bot left", d)
	}
	if got := h.api.createdSettings()[1].JoinAt; !got.IsZero() {
		t.Fatal("B's bot has a join_at")
	}
}

func TestAnEventWhoseEndPassesDuringAnotherCallIsBusy(t *testing.T) {
	startA := botT0.Add(2 * time.Minute)
	h := newBotHarness(t,
		botCalEvent("evA", startA, startA.Add(60*time.Minute)),
		botCalEvent("evB", startA.Add(10*time.Minute), startA.Add(20*time.Minute)))
	h.start()
	h.linkUp(botPiHello(botTexts()))
	h.inCall()
	h.advanceTo(startA.Add(20 * time.Minute).Add(-time.Second))
	if n := len(h.link.sentOf(call.TypeBusy)); n != 0 {
		t.Fatalf("busy sent before B's end: %d", n)
	}
	h.advance(31 * time.Second)
	if !h.cal.event("evB").Done {
		t.Fatal("B is not done after its end passed during A's call")
	}
	busy := h.link.sentOf(call.TypeBusy)
	if len(busy) != 1 || busy[0].Busy == nil || busy[0].Busy.Event.ID != "evB" {
		t.Fatalf("busy controls = %+v, want one for evB", busy)
	}
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("CreateBot calls = %d, want only A's", n)
	}
}

func TestAnUnsentBusyWaitsForTheLink(t *testing.T) {
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
	if !h.cal.event("evB").Done {
		t.Fatal("B is not done after its end passed during A's call")
	}
	if n := len(h.link.sentOf(call.TypeBusy)); n != 0 {
		t.Fatalf("busy sent while the link was down: %d", n)
	}
	h.linkUp(botPiHello(botTexts()))
	busy := h.link.sentOf(call.TypeBusy)
	if len(busy) != 1 || busy[0].Busy.Event.ID != "evB" {
		t.Fatalf("busy controls after the link-up = %+v, want one for evB", busy)
	}
	h.advance(time.Minute)
	if n := len(h.link.sentOf(call.TypeBusy)); n != 1 {
		t.Fatalf("busy sent %d times, want once", n)
	}
}

// A CreateBot that failed may still have made the bot (a timeout, a lost
// answer). Before it makes another, the bridge reads Attendee's bots and
// takes the one whose metadata holds the failed call's nonce; a read that
// fails is no proof that none exists, so it makes none then (R14).
func TestNoSecondBotAfterAnAmbiguousCreate(t *testing.T) {
	start := botT0.Add(2 * time.Minute)
	joinWith := func(t *testing.T, fails ...string) *botHarness {
		t.Helper()
		h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
		h.api.createErrs = fails
		h.start()
		h.linkUp(botPiHello(botTexts()))
		return h
	}
	t.Run("the failed call made the bot", func(t *testing.T) {
		h := joinWith(t, "made")
		h.advance(2 * joinEvery)
		if created := h.api.callsOf("CreateBot"); len(created) != 1 {
			t.Fatalf("CreateBot calls = %+v, want only the one that made bot_1", created)
		}
		if n := len(h.cal.callsOf("AddJoin")); n != 1 {
			t.Fatalf("joins counted = %d, want 1", n)
		}
		h.inCall() // bot_1 is the call's bot: its admission starts its chat check
	})
	t.Run("the read of Attendee's bots fails once", func(t *testing.T) {
		h := joinWith(t, "made")
		h.api.fail(func(a *fakeAttendee) { a.botsFails = 1 })
		h.advance(joinEvery)
		if n := len(h.api.callsOf("CreateBot")); n != 1 {
			t.Fatalf("CreateBot calls = %d while the bridge could not read whether bot_1 exists, want 1", n)
		}
		h.advance(joinEvery)
		if n := len(h.api.callsOf("CreateBot")); n != 1 {
			t.Fatalf("CreateBot calls = %d after the read recovered, want 1", n)
		}
		h.inCall()
	})
	// Control: when no bot holds the nonce, the next pass makes the bot.
	t.Run("the failed call made no bot", func(t *testing.T) {
		h := joinWith(t, "none")
		h.advance(joinEvery)
		if created := h.api.callsOf("CreateBot"); len(created) != 2 || created[1].ID == "" {
			t.Fatalf("CreateBot calls = %+v, want a second that makes the bot", created)
		}
		h.inCall()
	})
}

// Before its admission, a bot whose event no longer asks for it is
// removed: an event deleted or no longer a call's event, one marked done,
// or one whose start moved more than 5 minutes away. Attendee deletes a
// scheduled bot, and a bot that already launched leaves (R14).
func TestAPendingBotIsRemovedWhenItsEventIsGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		start  time.Duration           // the event's start after the bridge's
		change func(e *calendar.Event) // nil: the event is deleted
		op     string                  // the call that removes the bot; "" for none
		later  bool                    // the event still counts: a new bot comes 5 minutes before its new start
	}{
		{"a scheduled bot of a deleted event", 4 * time.Minute, nil, "Remove", false},
		{"a scheduled bot of an event marked done", 4 * time.Minute, func(e *calendar.Event) { e.Done = true }, "Remove", false},
		{"a scheduled bot of an event moved 6 minutes later", 4 * time.Minute, func(e *calendar.Event) { e.Start = e.Start.Add(6 * time.Minute) }, "Remove", true},
		{"a launched bot of a deleted event", 30 * time.Second, nil, "Leave", false},
		{"a scheduled bot of an event moved 2 minutes later, still within 5 minutes", 2 * time.Minute, func(e *calendar.Event) { e.Start = e.Start.Add(2 * time.Minute) }, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := botT0.Add(tc.start)
			h := newBotHarness(t, botCalEvent("ev1", start, start.Add(30*time.Minute)))
			h.start()
			h.linkUp(botPiHello(botTexts()))
			created := h.api.callsOf("CreateBot")
			if len(created) != 1 {
				t.Fatalf("CreateBot calls = %d, want 1", len(created))
			}
			h.cal.change("ev1", tc.change)
			h.advance(joinEvery + time.Second)
			removes := append(h.api.callsOf("Remove"), h.api.callsOf("Leave")...)
			if tc.op == "" {
				if len(removes) != 0 {
					t.Fatalf("the bridge removed the bot of an event that still asks for it: %+v", removes)
				}
				return
			}
			if got := h.api.callsOf(tc.op); len(got) != 1 || got[0].ID != created[0].ID || len(removes) != 1 {
				t.Fatalf("removals = %+v, want one %s of %s", removes, tc.op, created[0].ID)
			}
			if !tc.later {
				return
			}
			h.advanceTo(h.cal.event("ev1").Start.Add(-5 * time.Minute).Add(31 * time.Second))
			if n := len(h.api.callsOf("CreateBot")); n != 2 {
				t.Fatalf("bots made = %d by 4m30s before the new start, want a new bot", n)
			}
		})
	}
}
