package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// lifecycleUnrecorded is the bot's calendar refusing every pending record.
type lifecycleUnrecorded struct{ calendar.Reader }

func (lifecycleUnrecorded) SetPending(context.Context, string, calendar.Pending) error {
	return errors.New("calendar write unavailable")
}

// lifecycleSlowRead is Attendee's API whose reads of a bot take delay on the
// bridge's clock, as a request that takes its time.
type lifecycleSlowRead struct {
	attendeeAPI
	clk   *fakeClock
	delay time.Duration
}

func (a lifecycleSlowRead) Bot(ctx context.Context, id string) (bot, error) {
	a.clk.Advance(a.delay)
	return a.attendeeAPI.Bot(ctx, id)
}

// R7 and spec "Deletion": delete_data comes within 60 s of the bot's end,
// the time in its record, not of the moment the bridge sees it. Here the
// ended webhook is lost, the poll sees the end up to 30 s later and its read
// takes 8 s; the wait for the done mark gets only what is left of the 60 s,
// and when the mark fails delete_data still comes in time. (Sol's review of
// #202 at c8f9db27, TestReviewDeletionDeadlineIncludesPollingAndCalendarFailure.)
func TestDeleteDataCountsItsDeadlineFromTheBotsEnd(t *testing.T) {
	for _, failDone := range []bool{false, true} {
		name := "the calendar takes the done mark"
		if failDone {
			name = "the calendar refuses the done mark"
		}
		t.Run(name, func(t *testing.T) {
			h, id := joinOneBot(t)
			h.inCall()
			h.advance(time.Second)
			endedAt := h.clk.Now()
			h.api.setBot(id, stateEnded, botRecord(endedAt, "meeting_ended", "post_processing_completed")...)
			h.b.api = lifecycleSlowRead{attendeeAPI: h.api, clk: h.clk, delay: 8 * time.Second}
			h.b.cal = &failingDoneCalendar{Reader: h.cal, fail: failDone}
			h.advanceTo(endedAt.Add(60 * time.Second))
			deletes := h.api.callsOf("DeleteData")
			if len(deletes) != 1 {
				t.Fatalf("delete_data calls %v after the bot's end = %d, want one within 60 s", h.clk.Now().Sub(endedAt), len(deletes))
			}
			if elapsed := deletes[0].At.Sub(endedAt); elapsed > 60*time.Second {
				t.Fatalf("delete_data came %v after the bot's end, want at most 60 s", elapsed)
			}
		})
	}
}

// R2: a chat check that has not passed fails at its deadline also when a
// restarted bridge cannot read the roster: an input it lacks stops a pass,
// never the deadline, so the bot leaves by 40 s after its admission. A
// restart long after the admission, whose bot's pass the calendar did not
// record, gives its reads one call's bound before the check fails. (Sol's
// review of #202 at c8f9db27, the first two cases:
// TestReviewRestartCannotWaitPastAnUnpassedChatCheck.)
func TestAChatCheckFailsAtItsDeadlineWhileARestartsReadsFail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		before     time.Duration // from the admission to the restart
		failRoster bool
		within     time.Duration // from the restart to the leave
	}{
		{"a restart at the admission, its reads working", 0, false, 41 * time.Second},
		{"a restart at the admission, its roster read failing", 0, true, 41 * time.Second},
		{"a restart 5 minutes after the admission, its roster read failing", 5 * time.Minute, true, callLimit + chatLeaveWithin + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, id := joinOneBot(t)
			if tc.before > 0 {
				h.b.cal = lifecycleUnrecorded{Reader: h.cal} // the pass is not recorded
			}
			h.admit(id)
			if tc.before > 0 {
				h.chat(id, "bot-device", botTexts().Consent) // the check passed before the restart
				h.advance(tc.before)
			}
			if tc.failRoster {
				h.api.fail(func(a *fakeAttendee) { a.participantFails = 1000 })
			}
			restartBridge(h)
			h.linkUp(botPiHello(botTexts()))
			h.advance(tc.within)
			if leaves := h.api.callsOf("Leave"); len(leaves) != 1 {
				t.Fatalf("Leave calls %v after the restart = %v, want one: the check has not passed", tc.within, leaves)
			}
		})
	}
}

// R2: a pass needs the Pi's texts, which give the notice; the deadline does
// not. A restarted bridge has a new link, with no hello until the Pi
// connects. With no texts and every roster and chat read failing, a check
// that has not passed still fails at its deadline, so the bot leaves by 40 s
// after its admission; the control is the Pi connecting at the restart. A
// pass recorded for this bot still applies no deadline. (Sol's second review
// of #202 at 2e217ba6, TestReviewFreshRestartWithoutPiTextsStillFailsUnpassedCheck.)
func TestAChatCheckFailsAtItsDeadlineBeforeThePisTexts(t *testing.T) {
	for _, tc := range []struct {
		name            string
		passed, connect bool
		leaves          int
	}{
		{"no texts since the restart", false, false, 1},
		{"the Pi connects at the restart (control)", false, true, 1},
		{"no texts since the restart, the pass recorded for this bot", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, id := joinOneBot(t)
			if tc.passed {
				h.inCall()
			} else {
				h.admit(id)
			}
			h.api.fail(func(a *fakeAttendee) { a.participantFails, a.chatReadFails = 1000, 1000 })
			h.link = newFakeLink() // a new bridge process's link: no hello yet
			restartBridge(h)
			if tc.connect {
				h.linkUp(botPiHello(botTexts()))
			}
			h.advance(41 * time.Second)
			if leaves := h.api.callsOf("Leave"); len(leaves) != tc.leaves {
				t.Fatalf("Leave calls 41 s after the admission = %v, want %d", leaves, tc.leaves)
			}
		})
	}
}

// R2: the bridge records its bot's passed chat check, with the echo's
// sender, on the event's record when the check passes. A restarted bridge
// that reads its own bot's pass there applies no chat-check deadline: with
// every roster and chat read failing it keeps the bot, with no leave and no
// failed chat_check. A pass recorded for another bot is no pass of this one,
// so the deadline holds and the bot leaves within the reads' 10 s and R2's
// 10 s.
func TestARestartAfterARecordedPassKeepsTheBotWhileItsReadsFail(t *testing.T) {
	for _, otherBot := range []bool{false, true} {
		name := "the pass recorded for this bot"
		if otherBot {
			name = "a pass recorded for another bot"
		}
		t.Run(name, func(t *testing.T) {
			h, id := joinOneBot(t)
			h.inCall()
			if got, want := h.cal.event("ev1").Pending, (calendar.Pending{Passed: id, Sender: "bot-device"}); got != want {
				t.Fatalf("the record after the passed check = %+v, want %+v", got, want)
			}
			h.advance(5 * time.Minute)
			if otherBot {
				h.cal.change("ev1", func(e *calendar.Event) { e.Pending.Passed = "bot_other" })
			}
			h.api.fail(func(a *fakeAttendee) { a.participantFails, a.chatReadFails = 1000, 1000 })
			n := len(h.link.sentOf(call.TypeChatCheck))
			restartBridge(h)
			h.linkUp(botPiHello(botTexts()))
			h.advance(time.Minute)
			var failed int
			for _, c := range h.link.sentOf(call.TypeChatCheck)[n:] {
				if c.ChatCheck.Result == "failed" {
					failed++
				}
			}
			leaves := len(h.api.callsOf("Leave"))
			switch {
			case !otherBot && (leaves != 0 || failed != 0):
				t.Fatalf("after the restart with every read failing: Leave calls %d, failed chat_checks %d; want none: the pass is recorded", leaves, failed)
			case otherBot && (leaves != 1 || failed != 1):
				t.Fatalf("after the restart with every read failing: Leave calls %d, failed chat_checks %d; want one each: no pass is recorded for this bot", leaves, failed)
			}
		})
	}
}
