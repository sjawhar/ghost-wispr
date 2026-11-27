package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

type failingDoneCalendar struct {
	calendar.Reader
	fail bool
}

func (c *failingDoneCalendar) MarkDone(ctx context.Context, id string) error {
	if c.fail {
		return errors.New("calendar write unavailable")
	}
	return c.Reader.MarkDone(ctx, id)
}

// Erasing a terminal record before MarkDone succeeds lets a restart join an
// event again after a denial, removal or failed chat check.
func TestBotDataIsKeptUntilTheDoneRecordIsDurable(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	cal := &failingDoneCalendar{Reader: h.cal, fail: true}
	h.b.cal = cal
	h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
	if n := len(h.api.callsOf("DeleteData")); n != 0 {
		t.Fatalf("DeleteData ran %d times while the calendar had not recorded done", n)
	}
	cal.fail = false
	h.b.joinPass(h.ctx)
	h.settle()
	if !h.cal.event("ev1").Done || len(h.api.callsOf("DeleteData")) != 1 {
		t.Fatal("retry did not record done before deleting the bot's data")
	}
}

// The first bot can end between calendar passes, after a waiting event's
// end. Its final pass must still classify that event as busy.
func TestAnEventExpiringJustBeforeTheOtherBotEndsIsBusy(t *testing.T) {
	endB := botT0.Add(20*time.Minute + 17*time.Second)
	h := newBotHarness(t,
		botCalEvent("evA", botT0.Add(2*time.Minute), botT0.Add(time.Hour)),
		botCalEvent("evB", botT0.Add(10*time.Minute), endB))
	h.start()
	h.linkUp(botPiHello(botTexts()))
	id := h.inCall()
	h.advanceTo(endB.Add(time.Second))
	h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
	busy := h.link.sentOf(call.TypeBusy)
	if !h.cal.event("evB").Done || len(busy) != 1 || busy[0].Busy.Event.ID != "evB" {
		t.Fatalf("event expiring during A was lost: done=%t busy=%+v", h.cal.event("evB").Done, busy)
	}
}

type stalledPollAttendee struct {
	attendeeAPI
	left chan time.Time
}

func (a *stalledPollAttendee) Bot(ctx context.Context, _ string) (bot, error) {
	<-ctx.Done()
	return bot{}, ctx.Err()
}

func (a *stalledPollAttendee) Leave(context.Context, string) error {
	a.left <- time.Now()
	return nil
}

// A fake API returning immediately hides the 15-second HTTP timeout in the
// foreground poll. R2 allows only ten seconds to leave after check failure.
func TestChatCheckLeaveIsNotDelayedByAStalledPoll(t *testing.T) {
	h, _ := joinOneBot(t)
	b := h.b
	now := time.Now()
	b.clock = realClock{}
	b.bot.admitted = now.Add(-chatCheckWindow)
	b.bot.lastPoll = now.Add(-pollEvery)
	api := &stalledPollAttendee{attendeeAPI: h.api, left: make(chan time.Time, 1)}
	b.api = api
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.botTick(ctx, b.bot, now)
	}()
	defer func() { cancel(); <-done }()
	select {
	case at := <-api.left:
		if at.Sub(now) > 10*time.Second {
			t.Fatalf("leave came %v after the chat check deadline", at.Sub(now))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bot did not leave within ten real seconds of check failure")
	}
}

type unreadableBotAttendee struct{ attendeeAPI }

func (a unreadableBotAttendee) Bot(context.Context, string) (bot, error) {
	return bot{}, errors.New("bot record temporarily unavailable")
}

// An ended webhook does not contain the cause of the end. A failed record
// read must not turn a denial into a crash eligible for a replacement.
func TestAnUnreadableEndedBotIsNotAssumedToHaveCrashed(t *testing.T) {
	h, id := joinOneBot(t)
	h.b.api = unreadableBotAttendee{attendeeAPI: h.api}
	h.end(id, "ended", botRecord(h.clk.Now(), "could_not_join_meeting/request_to_join_denied", "post_processing_completed")...)
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("an unreadable denied bot was replaced: %d bots created", n)
	}
	h.b.api = h.api
	h.advance(pollEvery)
	if !h.cal.event("ev1").Done || len(h.api.callsOf("DeleteData")) != 1 {
		t.Fatal("the retried record read did not finish the denied event")
	}
}

// A restarted bridge that finds a second live bot tells it to leave and holds
// it beside the call's bot. The call's bot's poll does not read it, so when
// its ended webhook comes while its record cannot be read, a poll of its own
// must read the record again and delete its data (R7). The bridge's leave of
// a second bot is not R1's leave rule, so its event is not done (R14).
func TestAnUnreadableEndOfAnotherBotIsReadAgain(t *testing.T) {
	h := newBotHarness(t,
		botCalEvent("ev1", botT0.Add(-10*time.Minute), botT0.Add(50*time.Minute)),
		botCalEvent("ev2", botT0.Add(-5*time.Minute), botT0.Add(55*time.Minute)))
	h.api.bots = append(h.api.bots,
		&bot{ID: "bot_1", State: "joining", EventID: "ev1", Nonce: "nonce-1", Events: botRecord(botT0.Add(-time.Minute), "join_requested")},
		&bot{ID: "bot_2", State: "joining", EventID: "ev2", Nonce: "nonce-2", Events: botRecord(botT0.Add(-time.Minute), "join_requested")})
	h.start()
	if left := h.api.callsOf("Leave"); len(left) != 1 || left[0].ID != "bot_2" {
		t.Fatalf("Leave calls at the start = %+v, want bot_2's alone", left)
	}
	h.b.api = unreadableBotAttendee{attendeeAPI: h.api}
	h.end("bot_2", "ended", botRecord(h.clk.Now(), "left_meeting", "post_processing_completed")...)
	h.b.api = h.api
	h.advance(pollEvery)
	if deletes := h.api.callsOf("DeleteData"); len(deletes) != 1 || deletes[0].ID != "bot_2" {
		t.Fatalf("DeleteData calls %v after the record could be read again, want bot_2's", deletes)
	}
	if h.cal.event("ev2").Done {
		t.Fatal("bot_2's event is done after the bridge's leave of a second bot, which is not R1's leave rule")
	}
}

func TestDoneWriteRetriesAfterTheRunEnds(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	cal := &failingDoneCalendar{Reader: h.cal, fail: true}
	h.b.cal = cal
	h.b.faults.LeaveAt = h.clk.Now().Add(time.Second)
	h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
	cal.fail = false
	h.advance(time.Minute)
	if !h.cal.event("ev1").Done || len(h.api.callsOf("DeleteData")) != 1 {
		t.Fatal("the run's end stopped the pending done write and data deletion")
	}
}

// botFailureLines counts the log's lines that begin with prefix.
func botFailureLines(t *testing.T, h *botHarness, prefix string) int {
	t.Helper()
	n := 0
	for _, m := range h.logs.messages(t) {
		if strings.HasPrefix(m, prefix) {
			n++
		}
	}
	return n
}

// R7 and spec "Deletion": delete_data comes by 60 s after the end whatever
// the calendar says, and a done write that fails is tried again before and
// after it. An event the calendar no longer has counts as done at once: no
// bot can join it again. A restart's start tries a done write once.
func TestDeleteDataComesBy60sWhenTheDoneWriteFails(t *testing.T) {
	t.Run("the calendar refuses the done write", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.inCall()
		cal := &failingDoneCalendar{Reader: h.cal, fail: true}
		h.b.cal = cal
		endedAt := h.clk.Now()
		h.end(id, "ended", botRecord(endedAt, "meeting_ended", "post_processing_completed")...)
		h.advance(60 * time.Second)
		if deletes := h.api.callsOf("DeleteData"); len(deletes) != 1 || deletes[0].At.Sub(endedAt) > 60*time.Second {
			t.Fatalf("DeleteData calls %+v, want one by 60 s after the end %v", deletes, endedAt)
		}
		before := botFailureLines(t, h, "MARK-DONE-FAILED")
		h.advance(joinEvery)
		if after := botFailureLines(t, h, "MARK-DONE-FAILED"); after <= before {
			t.Fatalf("MARK-DONE-FAILED lines %d, then %d: the done write was not tried again after delete_data", before, after)
		}
		cal.fail = false
		h.advance(joinEvery)
		if !h.cal.event("ev1").Done {
			t.Fatal("ev1 is not done after the calendar recovered")
		}
	})
	t.Run("the calendar no longer has the event", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.inCall()
		h.cal.change("ev1", nil)
		h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
		if n := len(h.api.callsOf("DeleteData")); n != 1 {
			t.Fatalf("DeleteData calls = %d at the end, want 1: no done write can land for a deleted event", n)
		}
		h.advance(2 * joinEvery)
		if n := botFailureLines(t, h, "MARK-DONE-FAILED"); n != 0 {
			t.Fatalf("MARK-DONE-FAILED lines = %d for an event the calendar no longer has, want 0", n)
		}
	})
	t.Run("at a restart", func(t *testing.T) {
		h := newBotHarness(t, restartInProgress("ev1"))
		h.api.bots = append(h.api.bots, &bot{ID: "bot_1", State: "ended", EventID: "ev1", Nonce: "nonce-1",
			Events: restartRecord("meeting_ended", "post_processing_completed")})
		h.calendar = &failingDoneCalendar{Reader: h.cal, fail: true}
		h.start()
		if n := botFailureLines(t, h, "MARK-DONE-FAILED"); n != 1 {
			t.Fatalf("MARK-DONE-FAILED lines at the start = %d, want 1", n)
		}
		h.advance(60 * time.Second)
		if deletes := h.api.callsOf("DeleteData"); len(deletes) != 1 || deletes[0].ID != "bot_1" {
			t.Fatalf("DeleteData calls %+v by 60 s after the start, want bot_1's", deletes)
		}
	})
}
