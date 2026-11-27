package calendar

import (
	"fmt"
	"testing"
	"time"
)

// endNames and resultNames name the values in failure messages.
var (
	endNames = map[BotEnd]string{
		EndLeaveRule:         "EndLeaveRule",
		EndChatCheckFailed:   "EndChatCheckFailed",
		EndMeetingEnded:      "EndMeetingEnded",
		EndRemoved:           "EndRemoved",
		EndCouldNotJoin:      "EndCouldNotJoin",
		EndAutoLeave:         "EndAutoLeave",
		EndFatalError:        "EndFatalError",
		EndLinkLost:          "EndLinkLost",
		EndSocketFailed:      "EndSocketFailed",
		EndInstanceRestarted: "EndInstanceRestarted",
	}
	resultNames = map[Result]string{Done: "Done", NewBot: "NewBot"}
)

func resultName(r Result) string {
	if name, ok := resultNames[r]; ok {
		return name
	}
	return fmt.Sprintf("Result(%d)", int(r))
}

// TestEachEndIsDoneOrANewBot walks the spec's table of ends ("The calendar:
// the watcher and the bridge", "Ending"): each row's ways a bot ends, during
// the call, give the row's result; a new bot comes only while fewer than 4
// bots were made, and any end at or after the event's end is done.
func TestEachEndIsDoneOrANewBot(t *testing.T) {
	start := time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC)
	during := time.Date(2026, 12, 1, 17, 20, 0, 0, time.UTC)
	rows := []struct {
		row   string // the table's "How the bot ended" cell
		kinds []BotEnd
		want  Result
	}{
		{"The bridge's leave by R1's leave rule, or after a failed chat check", []BotEnd{EndLeaveRule, EndChatCheckFailed}, Done},
		{"The meeting ended, or a participant removed the bot", []BotEnd{EndMeetingEnded, EndRemoved}, Done},
		{"Nobody admitted the bot, someone denied it, or a new bot would meet the same refusal", []BotEnd{EndCouldNotJoin}, Done},
		{"Attendee's own automatic leave", []BotEnd{EndAutoLeave}, Done},
		{"A crash or fault of the bot, also one that upkeep ended", []BotEnd{EndFatalError}, NewBot},
		{"The bridge's leave after 2 minutes with the link down, or after Attendee's socket failed 30 times", []BotEnd{EndLinkLost, EndSocketFailed}, NewBot},
		{"The instance restarted", []BotEnd{EndInstanceRestarted}, NewBot},
	}
	covered := map[BotEnd]bool{}
	for _, r := range rows {
		for _, kind := range r.kinds {
			covered[kind] = true
			for _, joins := range []int{1, 3} {
				e := Event{ID: "call-1", Start: start, End: end, Joins: joins}
				if got := EndResult(kind, e, during); got != r.want {
					t.Errorf("row %q: %s with %d joins during the call gives %s, want %s",
						r.row, endNames[kind], joins, resultName(got), resultName(r.want))
				}
			}
			if r.want == NewBot {
				e := Event{ID: "call-1", Start: start, End: end, Joins: MaxJoins}
				if got := EndResult(kind, e, during); got != Done {
					t.Errorf("row %q: %s with %d joins during the call gives %s, want Done (no fifth bot)",
						r.row, endNames[kind], MaxJoins, resultName(got))
				}
			}
		}
	}
	if len(covered) != len(endNames) {
		t.Fatalf("the rows cover %d ways a bot ends, want all %d", len(covered), len(endNames))
	}

	const lastRow = "Any end after the event's end"
	for kind := EndLeaveRule; kind <= EndInstanceRestarted; kind++ {
		for _, now := range []time.Time{end, end.Add(time.Second), end.Add(3 * time.Hour)} {
			e := Event{ID: "call-1", Start: start, End: end, Joins: 1}
			if got := EndResult(kind, e, now); got != Done {
				t.Errorf("row %q: %s at %s gives %s, want Done", lastRow, endNames[kind], now.Format(time.RFC3339), resultName(got))
			}
		}
	}
}

// joinNow is the time of the NextToJoin tests: 16:56 UTC.
var joinNow = time.Date(2026, 12, 1, 16, 56, 0, 0, time.UTC)

func joinEvent(id string, start, end string, joins int, done bool) Event {
	parse := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return t
	}
	return Event{ID: id, Start: parse(start), End: parse(end), Joins: joins, Done: done}
}

func TestNextToJoinTakesTheFirstEventDueWithin5Minutes(t *testing.T) {
	evs := []Event{
		joinEvent("ended", "2026-12-01T15:00:00Z", "2026-12-01T16:00:00Z", 0, false),
		joinEvent("done", "2026-12-01T16:30:00Z", "2026-12-01T17:30:00Z", 1, true),
		joinEvent("due", "2026-12-01T16:58:00Z", "2026-12-01T17:30:00Z", 0, false),
		joinEvent("due-too", "2026-12-01T16:58:00Z", "2026-12-01T17:45:00Z", 0, false),
	}
	got, ok := NextToJoin(evs, joinNow, true, true, true, false)
	if !ok || got.ID != "due" {
		t.Fatalf("NextToJoin at 16:56 = %q, %v; want \"due\", true", got.ID, ok)
	}

	started := []Event{joinEvent("started", "2026-12-01T16:00:00Z", "2026-12-01T17:00:00Z", 1, false)}
	if got, ok := NextToJoin(started, joinNow, true, true, true, false); !ok || got.ID != "started" {
		t.Errorf("an event in progress: NextToJoin = %q, %v; want \"started\", true", got.ID, ok)
	}

	later := []Event{joinEvent("later", "2026-12-01T17:01:00Z", "2026-12-01T17:30:00Z", 0, false)}
	for _, tc := range []struct {
		now  time.Time
		want bool
	}{
		{time.Date(2026, 12, 1, 16, 55, 59, 0, time.UTC), false}, // 5 minutes and 1 second before
		{time.Date(2026, 12, 1, 16, 56, 0, 0, time.UTC), true},   // 5 minutes before
	} {
		if _, ok := NextToJoin(later, tc.now, true, true, true, false); ok != tc.want {
			t.Errorf("an event at 17:01, NextToJoin at %s: %v, want %v", tc.now.Format(time.RFC3339), ok, tc.want)
		}
	}

	if got, ok := NextToJoin(evs[:2], joinNow, true, true, true, false); ok {
		t.Errorf("only an ended and a done event: NextToJoin = %q, want none", got.ID)
	}
}

func TestNextToJoinWaitsForTheLinkTheTextsTheBuildAndAFreeCall(t *testing.T) {
	evs := []Event{joinEvent("due", "2026-12-01T16:58:00Z", "2026-12-01T17:30:00Z", 0, false)}
	for _, tc := range []struct {
		name                                     string
		linkUp, textsGiven, piBuildOK, botInCall bool
		want                                     bool
	}{
		{"the link down", false, true, true, false, false},
		{"no texts from the Pi", true, false, true, false, false},
		{"the Pi on another build", true, true, false, false, false},
		{"another bot in a call", true, true, true, true, false},
		{"all ready", true, true, true, false, true},
	} {
		if _, ok := NextToJoin(evs, joinNow, tc.linkUp, tc.textsGiven, tc.piBuildOK, tc.botInCall); ok != tc.want {
			t.Errorf("%s: NextToJoin gives an event: %v, want %v", tc.name, ok, tc.want)
		}
	}
}

func TestNextToJoinMakesNoFifthBot(t *testing.T) {
	for _, tc := range []struct {
		joins int
		want  bool
	}{{0, true}, {3, true}, {4, false}} {
		evs := []Event{joinEvent("started", "2026-12-01T16:00:00Z", "2026-12-01T17:00:00Z", tc.joins, false)}
		if _, ok := NextToJoin(evs, joinNow, true, true, true, false); ok != tc.want {
			t.Errorf("an event with %d joins: NextToJoin gives it: %v, want %v", tc.joins, ok, tc.want)
		}
	}
}
