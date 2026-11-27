// Package calendar holds the rules by which the cloud call bridge and its
// watcher read the bot user's calendar: which events are a call's, when an
// event keeps the call pool up, which event the bridge joins next, and what
// follows each way a bot ends. The calendar holds the whole schedule and each
// event's record, so no other store holds state.
package calendar

import (
	"context"
	"time"
)

// Event is one occurrence of a call's event on the bot user's calendar.
type Event struct {
	ID, Organizer, MeetURL string
	Start, End             time.Time
	Done                   bool    // extendedProperties.private ghostWisprDone
	Joins                  int     // extendedProperties.private ghostWisprJoins; 0 when unset
	Pending                Pending // extendedProperties.private ghostWisprPending; zero when unset
}

// Pending is what the bridge still owes for an event, on the calendar so a
// restart of the bridge does not forget it: the reason it asked the event's
// bot Bot to leave, and each control for the event that the Pi has not
// received, from which the Pi publishes call_not_recorded (R2, R14).
type Pending struct {
	Leave           BotEnd // EndLeaveRule, EndChatCheckFailed, EndLinkLost or EndSocketFailed; 0 when the bridge asked no bot to leave
	Bot             string // the bot Leave is for
	Busy            bool   // a busy for the event
	ChatCheckFailed bool   // a chat_check with the result failed for the event
}

// Owes reports whether p holds a control the Pi has not received.
func (p Pending) Owes() bool { return p.Busy || p.ChatCheckFailed }

// Reader reads the bot user's calendar. Events returns only a call's events
// (Filter applied), each occurrence of a recurring event its own Event,
// ordered by Start.
type Reader interface {
	Events(ctx context.Context, from, to time.Time) ([]Event, error)
	MarkDone(ctx context.Context, id string) error
	AddJoin(ctx context.Context, id string) (int, error) // the new count
	// SetPending writes p as the event's ghostWisprPending; the zero Pending
	// is the record that owes nothing.
	SetPending(ctx context.Context, id string, p Pending) error
}

// Filter says which events are a call's: an organizer whose email domain is
// one of OrgDomains, and who is in Testers while that list is not empty; the
// bot user BotUser among the attendees; and a Google Meet link.
type Filter struct {
	OrgDomains, Testers []string
	BotUser             string
}
