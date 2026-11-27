package calendar

import "time"

// NextToJoin gives the event the bridge makes a bot for at now: the first of
// evs (ordered by start) that keeps the pool up, is not done, has not ended,
// has started or starts within CreateLead, and has had fewer than MaxJoins
// bots; and only when the link is up (linkUp), the Pi's first message has
// given the texts (textsGiven), the Pi runs a build the bridge joins for
// (piBuildOK), and no other bot of this instance is in a call (!botInCall).
// The caller passes piBuildOK = (the Pi's hello.Build == call.BuildRelease ||
// bridgeBuild == call.BuildCallfault).
func NextToJoin(evs []Event, now time.Time, linkUp, textsGiven, piBuildOK, botInCall bool) (Event, bool) {
	if !linkUp || !textsGiven || !piBuildOK || botInCall {
		return Event{}, false
	}
	for i := range evs {
		e := &evs[i]
		if KeepsPoolUp(*e, now) && !e.Done && now.Before(e.End) && e.Start.Sub(now) <= CreateLead && e.Joins < MaxJoins {
			return *e, true
		}
	}
	return Event{}, false
}

// BotEnd is one way a bot ends: one value per row of the spec's table of ends
// ("The calendar: the watcher and the bridge", "Ending"), with a value for
// each way a row names. Attendee's names are its bots/models.py BotEventTypes
// and BotEventSubTypes.
type BotEnd int

const (
	EndLeaveRule         BotEnd = iota + 1 // the bridge's leave by R1's leave rule (LEAVE_REQUESTED)
	EndChatCheckFailed                     // the bridge's leave after a failed chat check (LEAVE_REQUESTED)
	EndMeetingEnded                        // MEETING_ENDED
	EndRemoved                             // MEETING_ENDED with a remover
	EndCouldNotJoin                        // COULD_NOT_JOIN, any Google Meet subtype
	EndAutoLeave                           // LEAVE_REQUESTED with a LEAVE_REQUESTED_AUTO_LEAVE_ subtype
	EndFatalError                          // FATAL_ERROR, any subtype
	EndLinkLost                            // the bridge's leave after 2 minutes with the link down
	EndSocketFailed                        // the bridge's leave after Attendee's socket failed 30 times
	EndInstanceRestarted                   // no record: the bot went with the instance
)

// Result is what follows a bot's end.
type Result int

const (
	// Done: the bridge marks the event done, then deletes the bot's data.
	Done Result = iota + 1
	// NewBot: the bridge makes a new bot for the event, by NextToJoin's
	// rules (so, after a lost link, once the link is back).
	NewBot
)

// EndResult gives the table of ends' result for a bot of e that ended by kind
// at now. NewBot comes only while e.Joins < MaxJoins and now is before e.End;
// every other end is Done.
//
//nolint:gocritic // hugeParam: the contract passes Event by value
func EndResult(kind BotEnd, e Event, now time.Time) Result {
	if !now.Before(e.End) || e.Joins >= MaxJoins {
		return Done
	}
	switch kind {
	case EndFatalError, EndLinkLost, EndSocketFailed, EndInstanceRestarted:
		return NewBot
	default:
		return Done
	}
}
