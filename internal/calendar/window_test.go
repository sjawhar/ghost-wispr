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
