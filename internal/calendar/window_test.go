package calendar

import (
	"testing"
	"time"
)

// windowStart and windowEnd are the times of the event the window tests read:
// a 30-minute call at 17:00 UTC.
var (
	windowStart = time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 12, 1, 17, 30, 0, 0, time.UTC)
)

func TestThePoolIsUpFrom10MinutesBeforeTheStart(t *testing.T) {
	e := Event{ID: "call-1", Start: windowStart, End: windowEnd}
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"a day before the start", time.Date(2026, 11, 30, 17, 0, 0, 0, time.UTC), false},
		{"10 minutes and 1 second before the start", time.Date(2026, 12, 1, 16, 49, 59, 0, time.UTC), false},
		{"10 minutes before the start", time.Date(2026, 12, 1, 16, 50, 0, 0, time.UTC), true},
		{"5 minutes before the start", time.Date(2026, 12, 1, 16, 55, 0, 0, time.UTC), true},
		{"at the start", windowStart, true},
		{"during the call", time.Date(2026, 12, 1, 17, 15, 0, 0, time.UTC), true},
	} {
		if got := KeepsPoolUp(e, tc.now); got != tc.want {
			t.Errorf("%s (%s): KeepsPoolUp = %v, want %v", tc.name, tc.now.Format(time.RFC3339), got, tc.want)
		}
	}
}

func TestThePoolStaysUpUntilDoneOr4HoursAfterTheEnd(t *testing.T) {
	open := Event{ID: "call-1", Start: windowStart, End: windowEnd, Joins: 1}
	done := Event{ID: "call-1", Start: windowStart, End: windowEnd, Joins: 1, Done: true}
	for _, tc := range []struct {
		name string
		e    Event
		now  time.Time
		want bool
	}{
		{"not done, at the end", open, windowEnd, true},
		{"not done, 2 hours after the end", open, time.Date(2026, 12, 1, 19, 30, 0, 0, time.UTC), true},
		{"not done, 4 hours less 1 second after the end", open, time.Date(2026, 12, 1, 21, 29, 59, 0, time.UTC), true},
		{"not done, 4 hours after the end", open, time.Date(2026, 12, 1, 21, 30, 0, 0, time.UTC), false},
		{"not done, 5 hours after the end", open, time.Date(2026, 12, 1, 22, 30, 0, 0, time.UTC), false},
		{"done, 10 minutes before the start", done, time.Date(2026, 12, 1, 16, 50, 0, 0, time.UTC), false},
		{"done, during the call", done, time.Date(2026, 12, 1, 17, 15, 0, 0, time.UTC), false},
		{"done, 1 minute after the end", done, time.Date(2026, 12, 1, 17, 31, 0, 0, time.UTC), false},
	} {
		if got := KeepsPoolUp(tc.e, tc.now); got != tc.want {
			t.Errorf("%s (%s): KeepsPoolUp = %v, want %v", tc.name, tc.now.Format(time.RFC3339), got, tc.want)
		}
	}
}

// An event that owes the Pi a control (a busy, or a failed chat_check, that
// the link has not taken) keeps the pool up also while it is done, until 4
// hours after its end, so the bridge can send it once the link is back. A
// done event that owes nothing, a leave reason alone included, does not, and
// NextToJoin never gives a done event.
func TestAnEventThatOwesThePiAControlKeepsThePoolUpUntilItsHold(t *testing.T) {
	owesBusy := Event{ID: "call-1", Start: windowStart, End: windowEnd, Done: true, Pending: Pending{Busy: true}}
	owesCheck := Event{ID: "call-2", Start: windowStart, End: windowEnd, Joins: 1, Done: true,
		Pending: Pending{Leave: EndChatCheckFailed, Bot: "bot_1", ChatCheckFailed: true}}
	leaveOnly := Event{ID: "call-3", Start: windowStart, End: windowEnd, Joins: 1, Done: true,
		Pending: Pending{Leave: EndLeaveRule, Bot: "bot_1"}}
	for _, tc := range []struct {
		name string
		e    Event
		now  time.Time
		want bool
	}{
		{"a busy owed, during the call", owesBusy, time.Date(2026, 12, 1, 17, 15, 0, 0, time.UTC), true},
		{"a busy owed, 1 minute after the end", owesBusy, time.Date(2026, 12, 1, 17, 31, 0, 0, time.UTC), true},
		{"a busy owed, 4 hours less 1 second after the end", owesBusy, time.Date(2026, 12, 1, 21, 29, 59, 0, time.UTC), true},
		{"a busy owed, 4 hours after the end", owesBusy, time.Date(2026, 12, 1, 21, 30, 0, 0, time.UTC), false},
		{"a failed chat check owed, 1 minute after the end", owesCheck, time.Date(2026, 12, 1, 17, 31, 0, 0, time.UTC), true},
		{"a failed chat check owed, 4 hours after the end", owesCheck, time.Date(2026, 12, 1, 21, 30, 0, 0, time.UTC), false},
		{"a leave reason alone, 1 minute after the end", leaveOnly, time.Date(2026, 12, 1, 17, 31, 0, 0, time.UTC), false},
	} {
		if got := KeepsPoolUp(tc.e, tc.now); got != tc.want {
			t.Errorf("%s (%s): KeepsPoolUp = %v, want %v", tc.name, tc.now.Format(time.RFC3339), got, tc.want)
		}
	}
	during := time.Date(2026, 12, 1, 17, 15, 0, 0, time.UTC)
	if got, ok := NextToJoin([]Event{owesBusy, owesCheck}, during, true, true, true, false); ok {
		t.Errorf("NextToJoin over done events that owe a control = %q, want none", got.ID)
	}
}
