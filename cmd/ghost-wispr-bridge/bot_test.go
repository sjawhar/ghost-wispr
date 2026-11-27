package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// botT0 is the bridge's start in the bridge's tests.
var botT0 = time.Date(2026, 12, 1, 16, 0, 0, 0, time.UTC)

// botMeetURL is the Meet link of every test event. No log line may hold it.
const botMeetURL = "https://meet.google.com/abc-defg-hij"

// botTexts are neutral notice texts, as fakePi gives them.
func botTexts() *call.Texts {
	return &call.Texts{
		BotName:      "Ghost Wispr test (recording)",
		Consent:      "Ghost Wispr test is recording this meeting. To pause it, type 'wispr pause' here.",
		PausedOpen:   "Paused (open).",
		PausedClosed: "Paused (closed).",
		StartsPaused: "Starts paused.",
		StartsClosed: "Starts closed.",
		ResumeHint:   "Type 'wispr resume' to resume.",
		OfficeOnly:   "Only the office can open its microphone.",
		Resumed:      "Recording again.",
		Lost:         "Lost the office.",
	}
}

// botCalEvent is a call's event of the bot's calendar.
func botCalEvent(id string, start, end time.Time) calendar.Event {
	return calendar.Event{ID: id, Organizer: "organizer@example.com", MeetURL: botMeetURL, Start: start, End: end}
}

var errBotStopped = errors.New("the bridge's process stopped")

// botStopper stops the fakes at the first of MarkDone and DeleteData, as a
// bridge that dies between the two calls: that first call takes effect, and
// every later call of either fake fails and changes nothing.
type botStopper struct {
	mu    sync.Mutex
	armed bool
	dead  bool
}

func (s *botStopper) isDead() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

// took tells the stopper that a MarkDone or a DeleteData took effect.
func (s *botStopper) took() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed {
		s.dead = true
	}
}

// revive lets the fakes take calls again, as the restarted bridge's process
// does: the stopped one made no call after the stop.
func (s *botStopper) revive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.dead = false, false
}

// botCall is one call a fake took.
type botCall struct {
	Op, ID, Text string
	At           time.Time
}

// fakeAttendee is Attendee's API as the bridge sees it: bots, their states
// and events, their chat lines, and every call with its time. A test sets
// the failures of the next calls of each kind.
type fakeAttendee struct {
	mu           sync.Mutex
	clock        clock
	stop         *botStopper
	bots         []*bot
	gone         map[string]bool // bots Remove deleted
	created      []botSettings
	calls        []botCall
	chats        map[string][]chatMessage
	participants map[string][]participantEvent

	createErrs       []string       // the next CreateBot calls' failures: "made" makes the bot and fails, "none" fails and makes none
	chatPostFails    int            // the next Chat calls that fail
	botsFails        int            // the next Bots calls that fail
	participantFails int            // the next ParticipantEvents calls that fail
	chatReadFails    int            // the next ChatMessages calls that fail
	leaveFails       map[string]int // the next Leave calls of a bot that fail
}

var errFakeAttendeeDown = errors.New("attendee did not answer")

func newFakeAttendee(c clock) *fakeAttendee {
	return &fakeAttendee{clock: c, gone: map[string]bool{}, chats: map[string][]chatMessage{},
		participants: map[string][]participantEvent{}, leaveFails: map[string]int{}}
}

func (a *fakeAttendee) record(op, id, text string) {
	a.calls = append(a.calls, botCall{Op: op, ID: id, Text: text, At: a.clock.Now()})
}

func (a *fakeAttendee) find(id string) *bot {
	for _, b := range a.bots {
		if b.ID == id && !a.gone[b.ID] {
			return b
		}
	}
	return nil
}

// CreateBot makes a bot as Attendee does: scheduled when it has a join_at,
// else joining.
//
//nolint:gocritic // hugeParam: attendeeAPI passes botSettings by value
func (a *fakeAttendee) CreateBot(_ context.Context, s botSettings) (bot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return bot{}, errBotStopped
	}
	var fail string
	if len(a.createErrs) > 0 {
		fail, a.createErrs = a.createErrs[0], a.createErrs[1:]
	}
	if fail == "none" {
		a.record("CreateBot", "", s.EventID)
		return bot{}, errFakeAttendeeDown
	}
	state := "joining"
	if !s.JoinAt.IsZero() {
		state = "scheduled"
	}
	b := &bot{ID: fmt.Sprintf("bot_%d", len(a.bots)+1), State: state, EventID: s.EventID, Nonce: s.Nonce,
		Events: []botEvent{{Type: "join_requested", At: a.clock.Now()}}}
	a.bots = append(a.bots, b)
	a.created = append(a.created, s)
	a.record("CreateBot", b.ID, s.EventID)
	if fail == "made" {
		return bot{}, errFakeAttendeeDown
	}
	return *b, nil
}

func (a *fakeAttendee) Chat(_ context.Context, botID, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return errBotStopped
	}
	if a.chatPostFails > 0 {
		a.chatPostFails--
		a.record("ChatFailed", botID, text)
		return errFakeAttendeeDown
	}
	a.record("Chat", botID, text)
	return nil
}

func (a *fakeAttendee) Leave(_ context.Context, botID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return errBotStopped
	}
	if a.leaveFails[botID] > 0 {
		a.leaveFails[botID]--
		a.record("LeaveFailed", botID, "")
		return errFakeAttendeeDown
	}
	a.record("Leave", botID, "")
	if b := a.find(botID); b != nil {
		b.State = "leaving"
		b.Events = append(b.Events, botEvent{Type: "leave_requested", SubType: "user_requested", At: a.clock.Now()})
	}
	return nil
}

// Remove deletes a scheduled bot, as DELETE /api/v1/bots/{id} does, and
// refuses a bot in any other state.
func (a *fakeAttendee) Remove(_ context.Context, botID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return errBotStopped
	}
	b := a.find(botID)
	if b == nil {
		return &attendeeStatusError{Method: "DELETE", Path: "/bots/" + botID, Status: 404}
	}
	if b.State != "scheduled" {
		a.record("RemoveRefused", botID, b.State)
		return &attendeeStatusError{Method: "DELETE", Path: "/bots/" + botID, Status: 400}
	}
	a.record("Remove", botID, "")
	a.gone[botID] = true
	return nil
}

func (a *fakeAttendee) DeleteData(_ context.Context, botID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return errBotStopped
	}
	b := a.find(botID)
	if b == nil || (b.State != "ended" && b.State != "fatal_error") {
		a.record("DeleteDataRefused", botID, "")
		return fmt.Errorf("delete_data of %s in state %v", botID, b)
	}
	a.record("DeleteData", botID, "")
	b.State = "data_deleted"
	a.stop.took()
	return nil
}

func (a *fakeAttendee) Bot(_ context.Context, botID string) (bot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return bot{}, errBotStopped
	}
	a.record("Bot", botID, "")
	b := a.find(botID)
	if b == nil {
		return bot{}, &attendeeStatusError{Method: "GET", Path: "/bots/" + botID, Status: 404}
	}
	c := *b
	c.Events = slices.Clone(b.Events)
	return c, nil
}

func (a *fakeAttendee) Bots(context.Context) ([]bot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return nil, errBotStopped
	}
	if a.botsFails > 0 {
		a.botsFails--
		a.record("BotsFailed", "", "")
		return nil, errFakeAttendeeDown
	}
	a.record("Bots", "", "")
	out := make([]bot, 0, len(a.bots))
	for _, b := range a.bots {
		if a.gone[b.ID] {
			continue
		}
		c := *b
		c.Events = slices.Clone(b.Events)
		out = append(out, c)
	}
	return out, nil
}

func (a *fakeAttendee) ParticipantEvents(_ context.Context, botID string) ([]participantEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return nil, errBotStopped
	}
	if a.participantFails > 0 {
		a.participantFails--
		a.record("ParticipantEventsFailed", botID, "")
		return nil, errFakeAttendeeDown
	}
	a.record("ParticipantEvents", botID, "")
	return slices.Clone(a.participants[botID]), nil
}

func (a *fakeAttendee) ChatMessages(_ context.Context, botID string) ([]chatMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop.isDead() {
		return nil, errBotStopped
	}
	if a.chatReadFails > 0 {
		a.chatReadFails--
		a.record("ChatMessagesFailed", botID, "")
		return nil, errFakeAttendeeDown
	}
	a.record("ChatMessages", botID, "")
	return slices.Clone(a.chats[botID]), nil
}

// fail sets the failures of the next calls under the fake's lock.
func (a *fakeAttendee) fail(set func(a *fakeAttendee)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	set(a)
}

// setBot changes a bot's state and adds events, as Attendee does when the bot
// moves.
func (a *fakeAttendee) setBot(id, state string, events ...botEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.find(id)
	if b == nil {
		panic("no bot " + id)
	}
	b.State = state
	b.Events = append(b.Events, events...)
}

// callsOf gives the calls of one operation, in order.
func (a *fakeAttendee) callsOf(op string) []botCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []botCall
	for _, c := range a.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

func (a *fakeAttendee) allCalls() []botCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

func (a *fakeAttendee) createdSettings() []botSettings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.created)
}

// fakeCalendar is the bot's calendar: Events gives the events that overlap
// the range, ordered by start; MarkDone and AddJoin change them.
type fakeCalendar struct {
	mu     sync.Mutex
	clock  clock
	stop   *botStopper
	events []calendar.Event
	calls  []botCall
}

func newFakeCalendar(c clock, evs ...calendar.Event) *fakeCalendar {
	return &fakeCalendar{clock: c, events: evs}
}

func (c *fakeCalendar) Events(_ context.Context, from, to time.Time) ([]calendar.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop.isDead() {
		return nil, errBotStopped
	}
	c.calls = append(c.calls, botCall{Op: "Events", At: c.clock.Now()})
	var out []calendar.Event
	for i := range c.events {
		if e := &c.events[i]; e.End.After(from) && e.Start.Before(to) {
			out = append(out, *e)
		}
	}
	slices.SortStableFunc(out, func(a, b calendar.Event) int { return a.Start.Compare(b.Start) })
	return out, nil
}

func (c *fakeCalendar) MarkDone(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop.isDead() {
		return errBotStopped
	}
	for i := range c.events {
		if c.events[i].ID == id {
			c.events[i].Done = true
			c.calls = append(c.calls, botCall{Op: "MarkDone", ID: id, At: c.clock.Now()})
			c.stop.took()
			return nil
		}
	}
	return fakeCalendarNoEvent(id)
}

func (c *fakeCalendar) AddJoin(_ context.Context, id string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop.isDead() {
		return 0, errBotStopped
	}
	for i := range c.events {
		if c.events[i].ID == id {
			c.events[i].Joins++
			c.calls = append(c.calls, botCall{Op: "AddJoin", ID: id, At: c.clock.Now()})
			return c.events[i].Joins, nil
		}
	}
	return 0, fakeCalendarNoEvent(id)
}

// SetPending sets the event's pending record, as the bot user's own copy
// keeps it.
func (c *fakeCalendar) SetPending(_ context.Context, id string, p calendar.Pending) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop.isDead() {
		return errBotStopped
	}
	for i := range c.events {
		if c.events[i].ID == id {
			c.events[i].Pending = p
			c.calls = append(c.calls, botCall{Op: "SetPending", ID: id, At: c.clock.Now()})
			return nil
		}
	}
	return fakeCalendarNoEvent(id)
}

// fakeCalendarNoEvent is the Calendar API's answer for an event it does not
// have, as calendar.NewReader passes it on.
func fakeCalendarNoEvent(id string) error {
	return fmt.Errorf("read event %s: %w", id, &googleapi.Error{Code: 404, Message: "Not Found"})
}

func (c *fakeCalendar) event(id string) calendar.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.events {
		if c.events[i].ID == id {
			return c.events[i]
		}
	}
	panic("no event " + id)
}

// change changes the event id in place, as an organizer's edit does; a nil
// change deletes it.
func (c *fakeCalendar) change(id string, edit func(e *calendar.Event)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.events {
		if c.events[i].ID == id {
			if edit == nil {
				c.events = slices.Delete(c.events, i, i+1)
			} else {
				edit(&c.events[i])
			}
			return
		}
	}
	panic("no event " + id)
}

func (c *fakeCalendar) callsOf(op string) []botCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []botCall
	for _, x := range c.calls {
		if x.Op == op {
			out = append(out, x)
		}
	}
	return out
}

// fakeLink is the bridge's link to the Pi: up or down, the Pi's last hello,
// and every control message the bridge sent while it was up.
type fakeLink struct {
	mu    sync.Mutex
	up    bool
	hello call.Hello
	sent  []call.Control
	recv  chan call.Message
}

func newFakeLink() *fakeLink { return &fakeLink{recv: make(chan call.Message, 16)} }

func (l *fakeLink) Hello() call.Hello {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hello
}

//nolint:gocritic // hugeParam: call.PiLink passes the control by value
func (l *fakeLink) Send(c call.Control) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.up {
		return errors.New("the link is down")
	}
	l.sent = append(l.sent, c)
	return nil
}

func (l *fakeLink) SendAudio(call.Frame) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.up {
		return errors.New("the link is down")
	}
	return nil
}

func (l *fakeLink) Recv() <-chan call.Message { return l.recv }

func (l *fakeLink) Up() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.up
}

// set puts the link up or down with the Pi's hello, and gives the hello as
// the link's next message when the link comes up.
func (l *fakeLink) set(up bool, hello *call.Hello) *call.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.up = up
	l.hello = *hello
	if !up {
		return nil
	}
	h := *hello
	return &call.Message{Control: &call.Control{Type: call.TypeHello, Hello: &h}}
}

// sentOf gives the control messages of one type that the bridge sent.
func (l *fakeLink) sentOf(typ string) []call.Control {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []call.Control
	for i := range l.sent {
		if l.sent[i].Type == typ {
			out = append(out, l.sent[i])
		}
	}
	return out
}

// botPiHello is the Pi's release hello with texts.
func botPiHello(texts *call.Texts) *call.Hello {
	return &call.Hello{Build: call.BuildRelease, LinkVersions: []int{1}, Texts: texts}
}

// botSockets is the audio socket as the bridge sees it: the nonce whose
// bot's socket it takes now, and every nonce registered and retired.
type botSockets struct {
	mu      sync.Mutex
	nonces  []string // registered, in order
	current string   // the nonce whose socket is taken; "" when none
	retired []string
	closed  bool
}

func (s *botSockets) Register(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nonces = append(s.nonces, nonce)
	s.current = nonce
}

func (s *botSockets) Retire(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = append(s.retired, nonce)
	if s.current == nonce {
		s.current = ""
	}
}

// Connected reads true unless a test says the socket is closed.
func (s *botSockets) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// taken gives the nonce whose socket is taken now and every one registered.
func (s *botSockets) taken() (current string, registered []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, slices.Clone(s.nonces)
}

func (s *botSockets) setClosed(closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = closed
}

// botLogs is a log the tests read while the bridge writes it.
type botLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *botLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *botLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// lines gives each log line's message.
func (l *botLogs) messages(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, line)
		}
		msg, _ := m["message"].(string)
		out = append(out, msg)
	}
	return out
}

// botHarness drives one bridge's handlers as runBridge's loop does: one tick
// a second on the fake clock, and each webhook and link message as it comes.
// attendee and calendar, when set, stand between the bridge and the fakes.
type botHarness struct {
	t        *testing.T
	ctx      context.Context
	clk      *fakeClock
	api      *fakeAttendee
	cal      *fakeCalendar
	attendee attendeeAPI
	calendar calendar.Reader
	link     *fakeLink
	sockets  *botSockets
	logs     *botLogs
	faults   faultSet
	s        settings
	b        *bridge
}

func newBotHarness(t *testing.T, evs ...calendar.Event) *botHarness {
	t.Helper()
	clk := newFakeClock(botT0)
	return &botHarness{
		t:       t,
		ctx:     t.Context(),
		clk:     clk,
		api:     newFakeAttendee(clk),
		cal:     newFakeCalendar(clk, evs...),
		link:    newFakeLink(),
		sockets: &botSockets{},
		logs:    &botLogs{},
		s:       botSettingsForTest(),
	}
}

func botSettingsForTest() settings {
	return settings{AudioPort: 9443, WebhookPort: 9081, BridgeKey: []byte("bridge-key")}
}

// deps gives the bridge's deps on the harness's fakes.
func (h *botHarness) deps() bridgeDeps {
	d := bridgeDeps{
		API:      h.api,
		Link:     h.link,
		Calendar: h.cal,
		Clock:    h.clk,
		Log:      newLogger(h.logs, "bridge", slog.LevelDebug),
		Faults:   h.faults,
		Sockets:  h.sockets,
	}
	if h.attendee != nil {
		d.API = h.attendee
	}
	if h.calendar != nil {
		d.Calendar = h.calendar
	}
	return d
}

// start makes a new bridge on the fakes and runs its start, after the
// BRIDGE-START line that startBridge logs, as a process of the bridge does.
func (h *botHarness) start() {
	h.t.Helper()
	s := h.s
	d := h.deps()
	logBridgeStart(h.ctx, s.MetadataURL, d.Log)
	h.b = newBridge(&s, &d)
	h.b.start(h.ctx)
	h.settle()
}

// settle applies the results of the calls that run beside the bridge's
// loop, as runBridge's loop does, until none runs, then waits for delete_data.
func (h *botHarness) settle() {
	for h.b.inflight > 0 {
		h.b.take(<-h.b.results)
	}
	h.b.bg.Wait()
}

// advance moves the clock d forward, one tick a second.
func (h *botHarness) advance(d time.Duration) {
	h.t.Helper()
	for end := h.clk.Now().Add(d); h.clk.Now().Before(end); {
		h.clk.Advance(tickEvery)
		h.b.tick(h.ctx)
		h.settle()
	}
}

// advanceTo moves the clock to t, one tick a second.
func (h *botHarness) advanceTo(t time.Time) { h.advance(t.Sub(h.clk.Now())) }

// linkUp puts the link up with the Pi's hello and gives the bridge the hello.
func (h *botHarness) linkUp(hello *call.Hello) {
	h.t.Helper()
	m := h.link.set(true, hello)
	h.b.onLink(h.ctx, *m)
	h.settle()
}

func (h *botHarness) linkDown() {
	hello := h.link.Hello()
	h.link.set(false, &hello)
}

func (h *botHarness) webhook(ev webhookEvent) {
	h.t.Helper()
	h.b.onWebhook(h.ctx, ev)
	h.settle()
}

// admit moves the bot into Attendee's joined state, the admission.
func (h *botHarness) admit(botID string) {
	h.t.Helper()
	h.api.setBot(botID, "joined_recording", botEvent{Type: "joined_meeting", At: h.clk.Now()})
	h.webhook(webhookEvent{Trigger: "bot.state_change", BotID: botID,
		State: &stateChange{New: "joined_not_recording", Old: "joining", EventType: "joined_meeting"}})
}

func (h *botHarness) chat(botID, sender, text string) {
	h.t.Helper()
	h.api.mu.Lock()
	m := chatMessage{ID: fmt.Sprintf("chat_%d", len(h.api.chats[botID])+1), SenderID: sender, Text: text, At: h.clk.Now()}
	h.api.chats[botID] = append(h.api.chats[botID], m)
	h.api.mu.Unlock()
	h.webhook(webhookEvent{Trigger: "chat_messages.update", BotID: botID, Chat: &m})
}

func (h *botHarness) join(botID, id, name string) {
	h.t.Helper()
	h.participant(botID, participantEvent{ID: id, Name: name, Join: true, At: h.clk.Now()})
}

func (h *botHarness) leave(botID, id, name string) {
	h.t.Helper()
	h.participant(botID, participantEvent{ID: id, Name: name, Join: false, At: h.clk.Now()})
}

// participant records a join or a leave in Attendee and sends its webhook.
func (h *botHarness) participant(botID string, p participantEvent) {
	h.t.Helper()
	h.api.mu.Lock()
	h.api.participants[botID] = append(h.api.participants[botID], p)
	h.api.mu.Unlock()
	h.webhook(webhookEvent{Trigger: "participant_events.join_leave", BotID: botID, Participant: &p})
}

// end moves the bot to state (ended or fatal_error) with its record's
// events, and sends the state change by webhook.
func (h *botHarness) end(botID, state string, events ...botEvent) {
	h.t.Helper()
	h.api.setBot(botID, state, events...)
	h.webhook(webhookEvent{Trigger: "bot.state_change", BotID: botID, State: &stateChange{New: state, Old: "post_processing"}})
}

// inCall makes the first bot, admits it and passes its chat check, and gives
// its id.
func (h *botHarness) inCall() string {
	h.t.Helper()
	created := h.api.callsOf("CreateBot")
	if len(created) == 0 {
		h.t.Fatal("no bot to put in the call")
	}
	id := created[len(created)-1].ID
	h.admit(id)
	h.chat(id, "bot-device", botTexts().Consent)
	if len(h.link.sentOf(call.TypeChatCheck)) == 0 {
		h.t.Fatalf("the chat check did not pass; link sent %+v", h.link.sent)
	}
	return id
}

// botEvents is the end of a bot's record, as Attendee writes it.
func botRecord(at time.Time, types ...string) []botEvent {
	var out []botEvent
	for _, t := range types {
		typ, sub, _ := strings.Cut(t, "/")
		out = append(out, botEvent{Type: typ, SubType: sub, At: at})
	}
	return out
}
