package main

import (
	"context"
	"testing"
	"time"
)

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
// restart long after the admission gives its reads one call's bound before
// the check fails. (Sol's review of #202 at c8f9db27, the first two cases:
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
