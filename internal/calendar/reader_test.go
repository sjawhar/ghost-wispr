package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// The names of the reader tests' calendar: the bot user, the member domain,
// and the people who organize its events.
const (
	botUser = "ghost-wispr@meet.example.org"
	alice   = "alice@example.com"
	carol   = "carol@example.com"
	dave    = "dave@example.com"
	mallory = "mallory@example.net"
)

var memberFilter = Filter{OrgDomains: []string{"example.com"}, BotUser: botUser}

// calendarFake is the Calendar API's events of one calendar, "primary" of the
// bot user, served over HTTP as the API serves them (calendar-api.json in
// google.golang.org/api/calendar/v3): a list expands a recurring event into
// its occurrences only with singleEvents=true, orders by start only with
// orderBy=startTime and refuses that order without singleEvents, keeps an
// event whose end is after timeMin and whose start is before timeMax, and
// answers in pages; a get and a patch reach one event or one occurrence.
type calendarFake struct {
	t       *testing.T
	mu      sync.Mutex
	entries []calendarFakeEntry
	private map[string]map[string]string // extendedProperties.private by event or occurrence id
	lists   []url.Values
	patches []calendarFakePatch
}

// calendarFakeEntry is one stored event. A recurring event holds its first
// occurrence's times and repeats count times, every apart.
type calendarFakeEntry struct {
	ev    *calendarapi.Event
	every time.Duration
	count int
}

// calendarFakePatch is one patch the fake took: the id in its path, its query,
// and its body as JSON keys.
type calendarFakePatch struct {
	id    string
	query url.Values
	body  map[string]json.RawMessage
}

// calendarFakePageSize is small, so a list of a few events takes more than one
// page; the API's own default is 250.
const calendarFakePageSize = 2

func newCalendarFake(t *testing.T) (*calendarFake, *calendarapi.Service) {
	t.Helper()
	f := &calendarFake{t: t, private: map[string]map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	svc, err := calendarapi.NewService(context.Background(), option.WithEndpoint(srv.URL+"/calendar/v3/"), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("calendar service: %v", err)
	}
	return f, svc
}

func (f *calendarFake) add(ev *calendarapi.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, calendarFakeEntry{ev: ev})
}

func (f *calendarFake) addRecurring(ev *calendarapi.Event, every time.Duration, count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev.Recurrence = []string{fmt.Sprintf("RRULE:FREQ=DAILY;INTERVAL=%d;COUNT=%d", int(every.Hours()/24), count)}
	f.entries = append(f.entries, calendarFakeEntry{ev: ev, every: every, count: count})
}

func (f *calendarFake) setPrivate(id string, private map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.private[id] = private
}

func fakeTime(edt *calendarapi.EventDateTime) time.Time {
	t, err := time.Parse(time.RFC3339, edt.DateTime)
	if err != nil {
		t, err = time.Parse("2006-01-02", edt.Date)
		if err != nil {
			panic(err)
		}
	}
	return t
}

// occurrences gives the entry as a list with singleEvents set or unset gives
// it: each occurrence with its own id, or the recurring event itself.
func (e calendarFakeEntry) occurrences(single bool) []*calendarapi.Event {
	if e.count == 0 || !single {
		return []*calendarapi.Event{e.ev}
	}
	start, end := fakeTime(e.ev.Start), fakeTime(e.ev.End)
	var out []*calendarapi.Event
	for i := range e.count {
		s := start.Add(time.Duration(i) * e.every)
		occ := *e.ev
		occ.Id = e.ev.Id + "_" + s.UTC().Format("20060102T150405Z")
		occ.RecurringEventId = e.ev.Id
		occ.Recurrence = nil
		occ.OriginalStartTime = &calendarapi.EventDateTime{DateTime: s.Format(time.RFC3339)}
		occ.Start = &calendarapi.EventDateTime{DateTime: s.Format(time.RFC3339)}
		occ.End = &calendarapi.EventDateTime{DateTime: end.Add(time.Duration(i) * e.every).Format(time.RFC3339)}
		out = append(out, &occ)
	}
	return out
}

// withRecord gives the event as the API answers it: with the private
// properties this calendar holds for it.
func (f *calendarFake) withRecord(ev *calendarapi.Event) *calendarapi.Event {
	out := *ev
	if p, ok := f.private[ev.Id]; ok {
		out.ExtendedProperties = &calendarapi.EventExtendedProperties{Private: p}
	}
	return &out
}

func (f *calendarFake) find(id string) *calendarapi.Event {
	for _, e := range f.entries {
		for _, occ := range e.occurrences(true) {
			if occ.Id == id {
				return occ
			}
		}
	}
	return nil
}

func (f *calendarFake) fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (f *calendarFake) answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode the answer: %v", err)
	}
}

func (f *calendarFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const events = "/calendar/v3/calendars/primary/events"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == events:
		f.list(w, r.URL.Query())
	case strings.HasPrefix(r.URL.Path, events+"/"):
		id := strings.TrimPrefix(r.URL.Path, events+"/")
		ev := f.find(id)
		if ev == nil {
			f.fail(w, http.StatusNotFound, "Not Found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.answer(w, f.withRecord(ev))
		case http.MethodPatch:
			f.patch(w, r, id, ev)
		default:
			f.fail(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	default:
		f.fail(w, http.StatusNotFound, "Not Found: "+r.Method+" "+r.URL.Path)
	}
}

func (f *calendarFake) list(w http.ResponseWriter, q url.Values) {
	f.lists = append(f.lists, q)
	single := q.Get("singleEvents") == "true"
	if q.Get("orderBy") == "startTime" && !single {
		f.fail(w, http.StatusBadRequest, "The requested ordering is not available for the particular query.")
		return
	}
	var timeMin, timeMax time.Time
	if v := q.Get("timeMin"); v != "" {
		timeMin, _ = time.Parse(time.RFC3339, v)
	}
	if v := q.Get("timeMax"); v != "" {
		timeMax, _ = time.Parse(time.RFC3339, v)
	}
	var items []*calendarapi.Event
	for _, e := range f.entries {
		for _, occ := range e.occurrences(single) {
			start, end := fakeTime(occ.Start), fakeTime(occ.End)
			if e.count > 0 && !single {
				end = end.Add(time.Duration(e.count-1) * e.every)
			}
			if (!timeMin.IsZero() && !end.After(timeMin)) || (!timeMax.IsZero() && !start.Before(timeMax)) {
				continue
			}
			items = append(items, f.withRecord(occ))
		}
	}
	if q.Get("orderBy") == "startTime" {
		sort.SliceStable(items, func(i, j int) bool { return fakeTime(items[i].Start).Before(fakeTime(items[j].Start)) })
	}
	offset, _ := strconv.Atoi(q.Get("pageToken"))
	page := calendarapi.Events{Kind: "calendar#events"}
	if offset < len(items) {
		page.Items = items[offset:min(offset+calendarFakePageSize, len(items))]
	}
	if offset+calendarFakePageSize < len(items) {
		page.NextPageToken = strconv.Itoa(offset + calendarFakePageSize)
	}
	f.answer(w, page)
}

func (f *calendarFake) patch(w http.ResponseWriter, r *http.Request, id string, ev *calendarapi.Event) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, http.StatusBadRequest, "bad body")
		return
	}
	f.patches = append(f.patches, calendarFakePatch{id: id, query: r.URL.Query(), body: body})
	if raw, ok := body["extendedProperties"]; ok {
		var props calendarapi.EventExtendedProperties
		if err := json.Unmarshal(raw, &props); err != nil {
			f.fail(w, http.StatusBadRequest, "bad extendedProperties")
			return
		}
		// The strictest reading of a patch: the private map sent replaces the
		// one held, so a patch that sends one key loses the other.
		if props.Private != nil {
			f.private[id] = props.Private
		}
	}
	f.answer(w, f.withRecord(ev))
}

// meetCall builds a timed event that start and end bound, organized by
// organizer, with attendees (the organizer is added first), and a Google Meet
// conference whose video entry point is meet, when meet is set.
func meetCall(id, organizer, start, end, meet string, attendees ...string) *calendarapi.Event {
	ev := &calendarapi.Event{
		Id:        id,
		Status:    "confirmed",
		EventType: "default",
		Organizer: &calendarapi.EventOrganizer{Email: organizer},
		Start:     &calendarapi.EventDateTime{DateTime: start},
		End:       &calendarapi.EventDateTime{DateTime: end},
		Attendees: []*calendarapi.EventAttendee{{Email: organizer, Organizer: true, ResponseStatus: "accepted"}},
	}
	for _, a := range attendees {
		ev.Attendees = append(ev.Attendees, &calendarapi.EventAttendee{Email: a, ResponseStatus: "needsAction"})
	}
	if meet != "" {
		ev.HangoutLink = meet
		ev.ConferenceData = &calendarapi.ConferenceData{
			ConferenceId:       strings.TrimPrefix(meet, "https://meet.google.com/"),
			ConferenceSolution: &calendarapi.ConferenceSolution{Key: &calendarapi.ConferenceSolutionKey{Type: "hangoutsMeet"}, Name: "Google Meet"},
			EntryPoints: []*calendarapi.EntryPoint{
				{EntryPointType: "phone", Uri: "tel:+1-555-0100", Label: "+1 555-0100"},
				{EntryPointType: "video", Uri: meet, Label: strings.TrimPrefix(meet, "https://")},
			},
		}
	}
	return ev
}

// eventLines gives each event as one line, for comparing and for a failure's
// message.
func eventLines(evs []Event) []string {
	var out []string
	for i := range evs {
		e := &evs[i]
		out = append(out, fmt.Sprintf("%s organizer=%s meet=%s start=%s end=%s done=%v joins=%d",
			e.ID, e.Organizer, e.MeetURL, e.Start.UTC().Format(time.RFC3339), e.End.UTC().Format(time.RFC3339), e.Done, e.Joins))
	}
	return out
}

func wantLines(t *testing.T, got []Event, want ...string) {
	t.Helper()
	if lines := eventLines(got); !slices.Equal(lines, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

var (
	readFrom = time.Date(2026, 12, 1, 16, 0, 0, 0, time.UTC)
	readTo   = time.Date(2026, 12, 2, 16, 0, 0, 0, time.UTC)
)

func TestAnEventCountsOnlyWithAMemberOrganizerTheBotAndAMeetLink(t *testing.T) {
	f, svc := newCalendarFake(t)
	// Stored out of start order: the list orders them only when asked to.
	f.add(meetCall("overlap-call", dave, "2026-12-01T17:15:00Z", "2026-12-01T18:00:00Z", "https://meet.google.com/ddd-eeee-fff", botUser))
	f.add(meetCall("member-call", alice, "2026-12-01T09:00:00-08:00", "2026-12-01T09:30:00-08:00", "https://meet.google.com/aaa-bbbb-ccc", carol, botUser))
	f.add(meetCall("outside-call", mallory, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/mmm-nnnn-ooo", botUser))
	f.add(meetCall("subdomain-call", "bob@eng.example.com", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/mmm-nnnn-ooo", botUser))
	f.add(meetCall("suffix-call", "eve@notexample.com", "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/mmm-nnnn-ooo", botUser))
	// The domain is the part after the last "@".
	f.add(meetCall("quoted-call", `"alice@example.com"@example.net`, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/mmm-nnnn-ooo", botUser))
	f.add(meetCall("quoted-member-call", `"ops@example.net"@example.com`, "2026-12-01T20:00:00Z", "2026-12-01T20:30:00Z", "https://meet.google.com/qqq-rrrr-sss", botUser))
	f.add(meetCall("no-bot-call", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/mmm-nnnn-ooo", carol))
	noMeet := meetCall("no-meet-call", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "", botUser)
	noMeet.Location = "https://meet.google.com/mmm-nnnn-ooo"
	f.add(noMeet)
	other := meetCall("other-conference-call", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "", botUser)
	other.ConferenceData = &calendarapi.ConferenceData{
		ConferenceSolution: &calendarapi.ConferenceSolution{Key: &calendarapi.ConferenceSolutionKey{Type: "addOn"}, Name: "Example Video"},
		EntryPoints:        []*calendarapi.EntryPoint{{EntryPointType: "video", Uri: "https://video.example.org/room-1"}},
	}
	f.add(other)
	f.add(meetCall("case-call", "Frank@EXAMPLE.COM", "2026-12-01T19:00:00Z", "2026-12-01T19:30:00Z", "https://meet.google.com/ggg-hhhh-iii", "Ghost-Wispr@Meet.Example.org"))
	f.add(meetCall("tomorrow-call", alice, "2026-12-02T17:00:00Z", "2026-12-02T17:30:00Z", "https://meet.google.com/jjj-kkkk-lll", botUser))

	got, err := NewReader(svc, memberFilter).Events(context.Background(), readFrom, readTo)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantLines(t, got,
		"member-call organizer=alice@example.com meet=https://meet.google.com/aaa-bbbb-ccc start=2026-12-01T17:00:00Z end=2026-12-01T17:30:00Z done=false joins=0",
		"overlap-call organizer=dave@example.com meet=https://meet.google.com/ddd-eeee-fff start=2026-12-01T17:15:00Z end=2026-12-01T18:00:00Z done=false joins=0",
		"case-call organizer=Frank@EXAMPLE.COM meet=https://meet.google.com/ggg-hhhh-iii start=2026-12-01T19:00:00Z end=2026-12-01T19:30:00Z done=false joins=0",
		`quoted-member-call organizer="ops@example.net"@example.com meet=https://meet.google.com/qqq-rrrr-sss start=2026-12-01T20:00:00Z end=2026-12-01T20:30:00Z done=false joins=0`,
	)
	if len(f.lists) == 0 {
		t.Fatal("the fake calendar was never listed")
	}
	q := f.lists[0]
	if q.Get("timeMin") != "2026-12-01T16:00:00Z" || q.Get("timeMax") != "2026-12-02T16:00:00Z" {
		t.Errorf("the list asked for timeMin %q and timeMax %q, want the read's bounds", q.Get("timeMin"), q.Get("timeMax"))
	}
}

func TestOnlyListedOrganizersCountWhileTheListIsSet(t *testing.T) {
	f, svc := newCalendarFake(t)
	f.add(meetCall("alice-call", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/aaa-bbbb-ccc", botUser))
	f.add(meetCall("carol-call", carol, "2026-12-01T18:00:00Z", "2026-12-01T18:30:00Z", "https://meet.google.com/ccc-dddd-eee", botUser))
	f.add(meetCall("mallory-call", mallory, "2026-12-01T19:00:00Z", "2026-12-01T19:30:00Z", "https://meet.google.com/mmm-nnnn-ooo", botUser))

	for _, tc := range []struct {
		name    string
		testers []string
		want    []string
	}{
		{"no tester list", nil, []string{"alice-call", "carol-call"}},
		{"alice listed", []string{"Alice@example.com"}, []string{"alice-call"}},
		{"a listed organizer outside the member domains", []string{mallory}, nil},
	} {
		filter := memberFilter
		filter.Testers = tc.testers
		got, err := NewReader(svc, filter).Events(context.Background(), readFrom, readTo)
		if err != nil {
			t.Fatalf("%s: Events: %v", tc.name, err)
		}
		var ids []string
		for i := range got {
			ids = append(ids, got[i].ID)
		}
		if !slices.Equal(ids, tc.want) {
			t.Errorf("%s: events %q, want %q", tc.name, ids, tc.want)
		}
	}
}

func TestEachOccurrenceIsItsOwnEvent(t *testing.T) {
	f, svc := newCalendarFake(t)
	f.addRecurring(meetCall("weekly", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/www-xxxx-yyy", botUser), 7*24*time.Hour, 3)
	f.add(meetCall("one-off", carol, "2026-12-09T12:00:00Z", "2026-12-09T12:45:00Z", "https://meet.google.com/ooo-pppp-qqq", botUser))
	f.setPrivate("weekly_20261208T170000Z", map[string]string{"ghostWisprDone": "true", "ghostWisprJoins": "2"})

	got, err := NewReader(svc, memberFilter).Events(context.Background(),
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantLines(t, got,
		"weekly_20261201T170000Z organizer=alice@example.com meet=https://meet.google.com/www-xxxx-yyy start=2026-12-01T17:00:00Z end=2026-12-01T17:30:00Z done=false joins=0",
		"weekly_20261208T170000Z organizer=alice@example.com meet=https://meet.google.com/www-xxxx-yyy start=2026-12-08T17:00:00Z end=2026-12-08T17:30:00Z done=true joins=2",
		"one-off organizer=carol@example.com meet=https://meet.google.com/ooo-pppp-qqq start=2026-12-09T12:00:00Z end=2026-12-09T12:45:00Z done=false joins=0",
		"weekly_20261215T170000Z organizer=alice@example.com meet=https://meet.google.com/www-xxxx-yyy start=2026-12-15T17:00:00Z end=2026-12-15T17:30:00Z done=false joins=0",
	)
}

func TestARecordIsKeptOnTheBotsCopyOfTheOccurrence(t *testing.T) {
	f, svc := newCalendarFake(t)
	f.addRecurring(meetCall("weekly", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/www-xxxx-yyy", botUser), 7*24*time.Hour, 2)
	const second = "weekly_20261208T170000Z"
	r := NewReader(svc, memberFilter)
	ctx := context.Background()
	for want := 1; want <= 2; want++ {
		n, err := r.AddJoin(ctx, second)
		if err != nil {
			t.Fatalf("AddJoin %d: %v", want, err)
		}
		if n != want {
			t.Errorf("AddJoin %d gave the count %d, want %d", want, n, want)
		}
	}
	if err := r.MarkDone(ctx, second); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	got, err := r.Events(ctx, time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantLines(t, got,
		"weekly_20261201T170000Z organizer=alice@example.com meet=https://meet.google.com/www-xxxx-yyy start=2026-12-01T17:00:00Z end=2026-12-01T17:30:00Z done=false joins=0",
		"weekly_20261208T170000Z organizer=alice@example.com meet=https://meet.google.com/www-xxxx-yyy start=2026-12-08T17:00:00Z end=2026-12-08T17:30:00Z done=true joins=2",
	)
	if len(f.patches) != 3 {
		t.Fatalf("%d patches, want 3 (two joins and the done mark)", len(f.patches))
	}
	for i, p := range f.patches {
		if p.id != second {
			t.Errorf("patch %d went to %q, want the occurrence %q", i, p.id, second)
		}
		if got := p.query.Get("sendUpdates"); got != "none" {
			t.Errorf("patch %d: sendUpdates %q, want none (no guest hears of the bot's record)", i, got)
		}
		keys := slices.Sorted(maps.Keys(p.body))
		if !slices.Equal(keys, []string{"extendedProperties"}) {
			t.Errorf("patch %d sent the fields %q, want only extendedProperties", i, keys)
		}
	}
}

func TestAnAllDayEventIsNoCall(t *testing.T) {
	f, svc := newCalendarFake(t)
	allDay := meetCall("all-day", alice, "", "", "https://meet.google.com/aaa-bbbb-ccc", botUser)
	allDay.Start = &calendarapi.EventDateTime{Date: "2026-12-01"}
	allDay.End = &calendarapi.EventDateTime{Date: "2026-12-02"}
	f.add(allDay)
	f.add(meetCall("timed", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/ccc-dddd-eee", botUser))

	got, err := NewReader(svc, memberFilter).Events(context.Background(), readFrom, readTo)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantLines(t, got,
		"timed organizer=alice@example.com meet=https://meet.google.com/ccc-dddd-eee start=2026-12-01T17:00:00Z end=2026-12-01T17:30:00Z done=false joins=0",
	)
}

func TestAnUnreadableJoinsCountAllowsNoNewBot(t *testing.T) {
	f, svc := newCalendarFake(t)
	for i, joins := range []string{"many", "-1"} {
		id := fmt.Sprintf("call-%d", i)
		f.add(meetCall(id, alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/aaa-bbbb-ccc", botUser))
		f.setPrivate(id, map[string]string{"ghostWisprJoins": joins})
	}
	got, err := NewReader(svc, memberFilter).Events(context.Background(), readFrom, readTo)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d events, want 2", len(got))
	}
	for i := range got {
		if e := &got[i]; e.Joins != MaxJoins {
			t.Errorf("%s: joins %d, want %d, so no new bot comes", e.ID, e.Joins, MaxJoins)
		}
	}
}

// privateOf gives the private properties the fake holds for id.
func (f *calendarFake) privateOf(id string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.private[id])
}

// The bridge's pending record is written to, and cleared on, the bot user's
// own copy of the occurrence alone, telling no guest; each write keeps the
// record's other properties, and a done mark keeps the pending record.
func TestThePendingRecordIsWrittenAndClearedOnTheBotsCopyOfTheOccurrence(t *testing.T) {
	f, svc := newCalendarFake(t)
	f.addRecurring(meetCall("weekly", alice, "2026-12-01T17:00:00Z", "2026-12-01T17:30:00Z", "https://meet.google.com/www-xxxx-yyy", botUser), 7*24*time.Hour, 2)
	const first, second = "weekly_20261201T170000Z", "weekly_20261208T170000Z"
	r := NewReader(svc, memberFilter)
	ctx := context.Background()
	if _, err := r.AddJoin(ctx, second); err != nil {
		t.Fatalf("AddJoin: %v", err)
	}
	read := func() map[string]Event {
		t.Helper()
		got, err := r.Events(ctx, time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		out := map[string]Event{}
		for i := range got {
			out[got[i].ID] = got[i]
		}
		return out
	}
	const botID = "bot_Ab12Cd34Ef56Gh78"
	for _, p := range []Pending{
		{Leave: EndLeaveRule, Bot: botID},
		{Leave: EndChatCheckFailed, Bot: botID, ChatCheckFailed: true},
		{Leave: EndLinkLost, Bot: botID},
		{Leave: EndSocketFailed, Bot: botID, Busy: true},
		{Busy: true, ChatCheckFailed: true},
		{},
	} {
		if err := r.SetPending(ctx, second, p); err != nil {
			t.Fatalf("SetPending %+v: %v", p, err)
		}
		evs := read()
		if got := evs[second]; got.Pending != p || got.Joins != 1 || got.Done {
			t.Errorf("after SetPending %+v: the occurrence reads pending %+v, joins %d, done %v; want that record, joins 1, not done", p, got.Pending, got.Joins, got.Done)
		}
		if got := evs[first].Pending; got != (Pending{}) {
			t.Errorf("after SetPending %+v on %s: the other occurrence reads pending %+v, want none", p, second, got)
		}
		if v := f.privateOf(second)[pendingProperty]; len(v) > maxPropertyValue {
			t.Errorf("the stored record is %d characters, more than the Calendar API's %d", len(v), maxPropertyValue)
		}
	}
	if v := f.privateOf(second)[pendingProperty]; v != "{}" {
		t.Errorf("the cleared record is %q, want {}", v)
	}
	owed := Pending{Busy: true}
	if err := r.SetPending(ctx, second, owed); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
	if err := r.MarkDone(ctx, second); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if got := read()[second]; !got.Done || got.Joins != 1 || got.Pending != owed {
		t.Errorf("after the done mark: done %v, joins %d, pending %+v; want done, joins 1, the owed busy", got.Done, got.Joins, got.Pending)
	}
	if len(f.patches) != 9 {
		t.Fatalf("%d patches, want 9 (a join, seven records and the done mark)", len(f.patches))
	}
	for i, p := range f.patches {
		if p.id != second {
			t.Errorf("patch %d went to %q, want the occurrence %q", i, p.id, second)
		}
		if got := p.query.Get("sendUpdates"); got != "none" {
			t.Errorf("patch %d: sendUpdates %q, want none (no guest hears of the bot's record)", i, got)
		}
		if keys := slices.Sorted(maps.Keys(p.body)); !slices.Equal(keys, []string{"extendedProperties"}) {
			t.Errorf("patch %d sent the fields %q, want only extendedProperties", i, keys)
		}
	}
}

// A pending record that does not read as one, also one the Calendar API cut
// at its 1024 characters, is read fail-safe: no new bot comes for its event
// (its joins read as MaxJoins, so a bridge's leave ends it as Done), and it
// owes nothing, so nothing is replayed. A readable record beside them reads.
func TestAnUnreadablePendingRecordMakesNoNewBotAndReplaysNothing(t *testing.T) {
	f, svc := newCalendarFake(t)
	unreadable := []string{
		"",
		"null",
		"not json",
		`{"busy":true} {}`,
		`{"busy":"yes"}`,
		`{"busy":true,"note":"x"}`,
		`{"leave":"link_lost"}`,
		`{"bot":"bot_1"}`,
		`{"leave":"teleported","bot":"bot_1"}`,
		`{"leave":"meeting_ended","bot":"bot_1"}`,
		`{"leave":"link_lost","bot":"bot 1"}`,
		`{"leave":"link_lost","bot":"` + strings.Repeat("b", 65) + `"}`,
		`{"chat_check":"passed"}`,
		(`{"leave":"link_lost","bot":"bot_1","busy":true,"pad":"` + strings.Repeat("x", 2000) + `"}`)[:1024],
	}
	const readable = `{"leave":"link_lost","bot":"bot_1","busy":true}`
	values := map[string]string{}
	for i, v := range append(slices.Clone(unreadable), readable) {
		id := fmt.Sprintf("call-%02d", i)
		values[id] = v
		f.add(meetCall(id, alice, "2026-12-01T17:00:00Z", "2026-12-01T18:00:00Z", "https://meet.google.com/aaa-bbbb-ccc", botUser))
		f.setPrivate(id, map[string]string{"ghostWisprJoins": "1", "ghostWisprPending": v})
	}
	got, err := NewReader(svc, memberFilter).Events(context.Background(), readFrom, readTo)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != len(values) {
		t.Fatalf("%d events, want %d", len(got), len(values))
	}
	during := time.Date(2026, 12, 1, 17, 20, 0, 0, time.UTC)
	for i := range got {
		e := &got[i]
		v := values[e.ID]
		_, joinable := NextToJoin(got[i:i+1], during, true, true, true, false)
		end := EndResult(EndLinkLost, *e, during)
		if v == readable {
			if want := (Pending{Leave: EndLinkLost, Bot: "bot_1", Busy: true}); e.Pending != want || e.Joins != 1 || !joinable || end != NewBot {
				t.Errorf("the readable record %q: pending %+v, joins %d, NextToJoin %v, a lost link's end %s; want %+v, 1, true, NewBot",
					v, e.Pending, e.Joins, joinable, resultName(end), want)
			}
			continue
		}
		if e.Pending != (Pending{}) || e.Joins != MaxJoins || joinable || end != Done {
			t.Errorf("the record %q: pending %+v, joins %d, NextToJoin %v, a lost link's end %s; want nothing owed, %d, false, Done",
				v, e.Pending, e.Joins, joinable, resultName(end), MaxJoins)
		}
	}
}

// The longest pending record the bridge writes fits the Calendar API's bound
// on a property's value: "The maximum size of a property's value is 1024
// characters, and properties with longer values will be silently truncated"
// (Calendar API guide "Extended properties", Limits). A record that would not
// fit, or that is not one, is refused before it is written.
func TestTheLongestPendingRecordFitsTheCalendarsValueLimit(t *testing.T) {
	longest := Pending{Leave: EndChatCheckFailed, Bot: strings.Repeat("b", maxBotID), Busy: true, ChatCheckFailed: true}
	v, err := encodePending(longest)
	if err != nil {
		t.Fatalf("encodePending(the longest record): %v", err)
	}
	if len(v) > maxPropertyValue {
		t.Fatalf("the longest record is %d characters, more than %d", len(v), maxPropertyValue)
	}
	for _, p := range []Pending{
		{Leave: EndLinkLost, Bot: strings.Repeat("b", maxBotID+1)},
		{Leave: EndLinkLost, Bot: "bot 1"},
		{Leave: EndLinkLost},
		{Bot: "bot_1"},
		{Leave: EndMeetingEnded, Bot: "bot_1"},
	} {
		if v, err := encodePending(p); err == nil {
			t.Errorf("encodePending(%+v) = %q, want it refused", p, v)
		}
	}
}
