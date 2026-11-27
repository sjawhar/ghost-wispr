package calendar

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"
)

// primary is the calendar the reader reads: the delegated bot user's own.
const primary = "primary"

// apiReader is the Reader over the Calendar API (google.golang.org/api/calendar/v3).
type apiReader struct {
	svc    *calendarapi.Service
	filter Filter
}

// NewReader gives the Reader of calendar "primary" of the user svc signs in
// as (the bot user, by domain-wide delegation), keeping only a call's events
// by f.
func NewReader(svc *calendarapi.Service, f Filter) Reader {
	return &apiReader{svc: svc, filter: f}
}

// Events lists the bot user's events whose end is after from and whose start
// is before to, with singleEvents=true, so each occurrence of a recurring
// event is its own event, ordered by start; and keeps a call's events.
func (r *apiReader) Events(ctx context.Context, from, to time.Time) ([]Event, error) {
	var evs []Event
	err := r.svc.Events.List(primary).
		SingleEvents(true).
		OrderBy("startTime").
		TimeMin(from.UTC().Format(time.RFC3339)).
		TimeMax(to.UTC().Format(time.RFC3339)).
		Pages(ctx, func(page *calendarapi.Events) error {
			for _, item := range page.Items {
				e, ok, err := r.callEvent(item)
				if err != nil {
					return err
				}
				if ok {
					evs = append(evs, e)
				}
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list the bot user's events: %w", err)
	}
	return evs, nil
}

// callEvent gives item as an Event when it is a call's event: timed (an
// all-day event has no call time), organized by a member (Filter), inviting
// the bot user, and holding a Google Meet link.
func (r *apiReader) callEvent(item *calendarapi.Event) (Event, bool, error) {
	if item.Start == nil || item.Start.DateTime == "" || item.End == nil || item.End.DateTime == "" {
		return Event{}, false, nil
	}
	if item.Organizer == nil || !r.filter.organizerCounts(item.Organizer.Email) || !r.filter.invitesBot(item.Attendees) {
		return Event{}, false, nil
	}
	meet := meetLink(item.ConferenceData)
	if meet == "" {
		return Event{}, false, nil
	}
	start, err := time.Parse(time.RFC3339, item.Start.DateTime)
	if err != nil {
		return Event{}, false, fmt.Errorf("event %s: read its start: %w", item.Id, err)
	}
	end, err := time.Parse(time.RFC3339, item.End.DateTime)
	if err != nil {
		return Event{}, false, fmt.Errorf("event %s: read its end: %w", item.Id, err)
	}
	var private map[string]string
	if item.ExtendedProperties != nil {
		private = item.ExtendedProperties.Private
	}
	done, joins, pending := readRecord(private)
	return Event{
		ID:        item.Id,
		Organizer: item.Organizer.Email,
		MeetURL:   meet,
		Start:     start.UTC(),
		End:       end.UTC(),
		Done:      done,
		Joins:     joins,
		Pending:   pending,
	}, true, nil
}

// organizerCounts reports whether an event organized by email is a call's:
// its domain, the part after the last "@", is one of OrgDomains, and email is
// in Testers while that list is not empty. Domains and emails compare without
// case, as mail does; a subdomain of a member domain is not a member domain.
func (f Filter) organizerCounts(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	if !slices.ContainsFunc(f.OrgDomains, func(d string) bool { return strings.EqualFold(d, domain) }) {
		return false
	}
	return len(f.Testers) == 0 || slices.ContainsFunc(f.Testers, func(t string) bool { return strings.EqualFold(t, email) })
}

// invitesBot reports whether the attendees name the bot user.
func (f Filter) invitesBot(attendees []*calendarapi.EventAttendee) bool {
	return f.BotUser != "" && slices.ContainsFunc(attendees, func(a *calendarapi.EventAttendee) bool {
		return a != nil && strings.EqualFold(a.Email, f.BotUser)
	})
}

// meetLink gives the Google Meet link of an event's conference: the uri of
// the "video" entry point of a conference whose solution type is
// "hangoutsMeet". The Calendar API's event resource (calendar-api.json in
// google.golang.org/api/calendar/v3) says ConferenceSolutionKey.type
// "hangoutsMeet" is Google Meet, and EntryPoint.entryPointType "video" is
// "joining a conference over HTTP", of which a conference has zero or one.
// Another conference solution, or a link in the location or description, is
// no Meet link.
func meetLink(c *calendarapi.ConferenceData) string {
	if c == nil || c.ConferenceSolution == nil || c.ConferenceSolution.Key == nil || c.ConferenceSolution.Key.Type != "hangoutsMeet" {
		return ""
	}
	for _, ep := range c.EntryPoints {
		if ep != nil && ep.EntryPointType == "video" && ep.Uri != "" {
			return ep.Uri
		}
	}
	return ""
}

// MarkDone sets the record's ghostWisprDone on the bot user's own copy of
// the occurrence id.
func (r *apiReader) MarkDone(ctx context.Context, id string) error {
	private, err := r.private(ctx, id)
	if err != nil {
		return err
	}
	return r.patchPrivate(ctx, id, withDone(private))
}

// AddJoin adds one to the record's ghostWisprJoins on the bot user's own copy
// of the occurrence id, and gives the new count.
func (r *apiReader) AddJoin(ctx context.Context, id string) (int, error) {
	private, err := r.private(ctx, id)
	if err != nil {
		return 0, err
	}
	joins := readJoins(private) + 1
	if err := r.patchPrivate(ctx, id, withJoins(private, joins)); err != nil {
		return 0, err
	}
	return joins, nil
}

// SetPending sets the record's ghostWisprPending on the bot user's own copy
// of the occurrence id. The zero Pending is written as "{}", not deleted:
// the Calendar API's guide deletes a property only by a patch that sets it to
// null, and "{}" is the same under either reading of a patch's map.
func (r *apiReader) SetPending(ctx context.Context, id string, p Pending) error {
	v, err := encodePending(p)
	if err != nil {
		return fmt.Errorf("event %s: %w", id, err)
	}
	private, err := r.private(ctx, id)
	if err != nil {
		return err
	}
	return r.patchPrivate(ctx, id, withPending(private, v))
}

func (r *apiReader) private(ctx context.Context, id string) (map[string]string, error) {
	ev, err := r.svc.Events.Get(primary, id).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("read event %s: %w", id, err)
	}
	if ev.ExtendedProperties == nil {
		return nil, nil
	}
	return ev.ExtendedProperties.Private, nil
}

// patchPrivate patches only the occurrence's private properties, telling no
// guest (sendUpdates=none).
func (r *apiReader) patchPrivate(ctx context.Context, id string, private map[string]string) error {
	patch := &calendarapi.Event{ExtendedProperties: &calendarapi.EventExtendedProperties{Private: private}}
	if _, err := r.svc.Events.Patch(primary, id, patch).SendUpdates("none").Context(ctx).Do(); err != nil {
		return fmt.Errorf("patch event %s's record: %w", id, err)
	}
	return nil
}
