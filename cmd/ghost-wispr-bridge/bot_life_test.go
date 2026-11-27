package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

func TestDeleteDataWithin60sOfTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		webhook     bool
		record      []string
	}{
		{"ended, by webhook", "ended", true, []string{"meeting_ended", "post_processing_completed"}},
		{"fatal_error, by webhook", "fatal_error", true, []string{"fatal_error/process_terminated"}},
		{"ended, by the poll", "ended", false, []string{"meeting_ended", "post_processing_completed"}},
		{"fatal_error, by the poll", "fatal_error", false, []string{"fatal_error/heartbeat_timeout"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, id := joinOneBot(t)
			h.inCall()
			h.advance(7 * time.Second)
			endedAt := h.clk.Now()
			if tc.webhook {
				h.end(id, tc.state, botRecord(endedAt, tc.record...)...)
			} else {
				h.api.setBot(id, tc.state, botRecord(endedAt, tc.record...)...)
			}
			h.advance(60 * time.Second)
			var deletes []botCall
			for _, c := range h.api.callsOf("DeleteData") {
				if c.ID == id {
					deletes = append(deletes, c)
				}
			}
			if len(deletes) != 1 {
				t.Fatalf("DeleteData calls for the ended bot in 60 s = %d, want 1", len(deletes))
			}
			if d := deletes[0].At.Sub(endedAt); d > 60*time.Second {
				t.Fatalf("DeleteData came %v after the end", d)
			}
		})
	}
}

func TestTheLeaveRuleWaitsForTheEndAndAnEmptyRoster(t *testing.T) {
	h, id := joinOneBot(t)
	end := botT0.Add(32 * time.Minute)
	h.inCall()
	h.join(id, "p1", "Person One")
	h.advanceTo(botT0.Add(20 * time.Minute))
	h.leave(id, "p1", "Person One")
	h.advance(time.Minute) // the roster is empty, but the end has not passed
	h.join(id, "p1", "Person One")
	h.advanceTo(end.Add(time.Minute)) // the end passed, but the roster holds p1
	if leaves := h.api.callsOf("Leave"); len(leaves) != 0 {
		t.Fatalf("the bot left at %v, before the end with an empty roster", leaves[0].At)
	}
	h.leave(id, "p1", "Person One")
	emptyAt := h.clk.Now()
	h.advance(5 * time.Second)
	leaves := h.api.callsOf("Leave")
	if len(leaves) != 1 {
		t.Fatalf("Leave calls after the end with an empty roster = %d, want 1", len(leaves))
	}
	if d := leaves[0].At.Sub(emptyAt); d > 2*time.Second {
		t.Fatalf("the bot left %v after the roster emptied", d)
	}
}

func TestANewBotWhileJoinsIsUnder4(t *testing.T) {
	h, _ := joinOneBot(t)
	for i := 1; i <= 4; i++ {
		created := h.api.callsOf("CreateBot")
		if len(created) != i {
			t.Fatalf("after %d crashes, bots made = %d, want %d", i-1, len(created), i)
		}
		h.advance(10 * time.Second)
		h.end(created[i-1].ID, "fatal_error", botRecord(h.clk.Now(), "fatal_error/process_terminated")...)
	}
	h.advance(2 * time.Minute)
	if n := len(h.api.callsOf("CreateBot")); n != 4 {
		t.Fatalf("bots made = %d, want 4: the first and 3 new bots", n)
	}
	if ev := h.cal.event("ev1"); !ev.Done || ev.Joins != 4 {
		t.Fatalf("event after the fourth crash = done %v joins %d, want done with 4", ev.Done, ev.Joins)
	}
}

// The bridge's own leave after 2 minutes without the link (T4.4's lost.go
// calls leave with EndLinkLost) gives a new bot, once the link is back.
func TestANewBotAfterALostLinkWaitsForTheLink(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	h.linkDown()
	h.advance(2 * time.Minute)
	h.b.leave(h.ctx, calendar.EndLinkLost)
	h.advance(5 * time.Second)
	h.end(id, "ended", botRecord(h.clk.Now(), "leave_requested/user_requested", "left_meeting", "post_processing_completed")...)
	h.advance(3 * time.Minute)
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("bots made while the link was down = %d, want only the first", n)
	}
	if h.cal.event("ev1").Done {
		t.Fatal("the event was marked done after a leave for a lost link")
	}
	h.linkUp(botPiHello(botTexts()))
	if n := len(h.api.callsOf("CreateBot")); n != 2 {
		t.Fatalf("bots made at the link-up = %d, want 2", n)
	}
}

// A8: a joined bot whose audio socket stays closed for 300 s (Attendee's 30
// tries, 10 s apart) gets the lost notice, then leaves, and is replaced.
func TestABotWhoseSocketStaysClosedFor300sLeaves(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	h.sockets.setClosed(true)
	h.advance(time.Second)
	closedAt := h.clk.Now()
	h.advance(298 * time.Second)
	if n := len(h.api.callsOf("Leave")); n != 0 {
		t.Fatalf("the bot left %d times before 300 s without its socket", n)
	}
	h.advance(2 * time.Second)
	calls := h.api.allCalls()
	lost, leave := -1, -1
	for i, c := range calls {
		if c.Op == "Chat" && c.Text == botTexts().Lost {
			lost = i
		}
		if c.Op == "Leave" {
			leave = i
		}
	}
	if lost < 0 || leave < 0 || lost > leave {
		t.Fatalf("calls = %+v, want the lost notice, then Leave", calls)
	}
	if d := calls[leave].At.Sub(closedAt); d < 299*time.Second || d > 301*time.Second {
		t.Fatalf("the bot left %v after its socket closed, want 300 s", d)
	}
	h.sockets.setClosed(false)
	h.end(id, "ended", botRecord(h.clk.Now(), "leave_requested/user_requested", "left_meeting", "post_processing_completed")...)
	if n := len(h.api.callsOf("CreateBot")); n != 2 {
		t.Fatalf("bots made after the socket-failed leave = %d, want a new bot", n)
	}
	if h.cal.event("ev1").Done {
		t.Fatal("the event was marked done after a socket-failed leave")
	}
}

func TestTheBridgeLogsOnlyMarkedLines(t *testing.T) {
	const sentinel = "zebra-sentinel-7731"
	h, id := joinOneBot(t)
	h.admit(id)
	h.join(id, "p1", "Person "+sentinel)
	h.chat(id, "bot-device", botTexts().Consent)
	h.chat(id, "p1", "please wispr pause "+sentinel)
	rateFeed(h, 24000)
	h.leave(id, "p1", "Person "+sentinel)
	h.advanceTo(botT0.Add(33 * time.Minute))
	h.end(id, "ended", botRecord(h.clk.Now(), "leave_requested/user_requested", "left_meeting", "post_processing_completed")...)

	raw := strings.TrimSpace(h.logs.String())
	lines := strings.Split(raw, "\n")
	// An empty log would pass every test below: it must hold the life's lines.
	for _, want := range []string{"BRIDGE-START", "CHAT-CHECK", "RATE", "JOIN-TIME"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("the log has no %s line, so it cannot show what it leaves out:\n%s", want, raw)
		}
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a line is not one JSON object: %s", line)
		}
		if m[componentKey] != "bridge" {
			t.Errorf("a line has no %s=bridge: %s", componentKey, line)
		}
		for _, word := range []string{sentinel, botMeetURL, botTexts().Consent, "organizer@example.com"} {
			if strings.Contains(line, word) {
				t.Errorf("a line holds %q: %s", word, line)
			}
		}
	}
}

// botLifeMetadata is Cloud Run's metadata server for the instance id alone,
// and gives its URL.
func botLifeMetadata(t *testing.T, instance string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/computeMetadata/v1/instance/id" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, instance)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// botLifeStartLines counts the log's BRIDGE-START lines.
func botLifeStartLines(msgs []string) int {
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m, "BRIDGE-START") {
			n++
		}
	}
	return n
}

// The bridge's first line is BRIDGE-START with Cloud Run's instance id, once,
// through main's own path, startBridge: in the release build before the
// bridge reads Attendee's bots, and in the callfault build also when it
// refuses its run spec (here none is set), which c-cloudrun-first reads.
func TestTheBridgeLogsItsInstanceAtStart(t *testing.T) {
	const instance = "0069c7a988e1d4fe5e2f00a1"
	clk := newFakeClock(botT0)
	api := newFakeAttendee(clk)
	logs := &botLogs{}
	s := botSettingsForTest()
	s.MetadataURL = botLifeMetadata(t, instance)
	deps := bridgeDeps{API: api, Calendar: newFakeCalendar(clk), Clock: clk, Log: newLogger(logs, "bridge", slog.LevelDebug), Sockets: &botSockets{}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- startBridge(ctx, &s, &deps) }()
	clockWaitFor(t, func() bool {
		return len(api.callsOf("Bots")) == 1 || strings.Contains(logs.String(), "RUN-SPEC-REFUSED")
	}, "the bridge's read of Attendee's bots, or its refusal of the run spec")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	msgs := logs.messages(t)
	if len(msgs) == 0 || msgs[0] != "BRIDGE-START instance="+instance || botLifeStartLines(msgs) != 1 {
		t.Fatalf("log = %q, want BRIDGE-START with the instance id first and once", msgs)
	}
}

// A8: JOIN-TIME gives the seconds from the bridge's start and from the bot's
// creation to its admission.
func TestJoinTimeLogsSecondsFromStartAndFromCreate(t *testing.T) {
	h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	h.start()
	h.advance(40 * time.Second)
	h.linkUp(botPiHello(botTexts()))
	created := h.api.callsOf("CreateBot")
	if len(created) != 1 {
		t.Fatalf("bots made = %d, want 1", len(created))
	}
	h.advance(60 * time.Second)
	h.admit(created[0].ID)
	if !slices.Contains(h.logs.messages(t), "JOIN-TIME seconds=100 create_seconds=60") {
		t.Fatalf("no JOIN-TIME line of 100 s and 60 s in %q", h.logs.messages(t))
	}
}

// A restarted bridge may find a bot whose record holds no event yet: a bot
// Attendee has scheduled for its join_at. JOIN-TIME then counts its
// create_seconds from the bridge's start, the first time this bridge knows,
// not from the zero time (about 292 years).
func TestJoinTimeOfAnAdoptedBotWithNoRecordCountsFromTheStart(t *testing.T) {
	h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	h.api.bots = append(h.api.bots, &bot{ID: "bot_1", State: "scheduled", EventID: "ev1", Nonce: "nonce-1"})
	h.start()
	h.linkUp(botPiHello(botTexts()))
	h.advance(90 * time.Second)
	h.admit("bot_1")
	if n := len(h.api.callsOf("CreateBot")); n != 0 {
		t.Fatalf("bots made = %d, want none beside the adopted one", n)
	}
	if !slices.Contains(h.logs.messages(t), "JOIN-TIME seconds=90 create_seconds=90") {
		t.Fatalf("no JOIN-TIME line of 90 s and 90 s in %q", h.logs.messages(t))
	}
}

func TestAttendeesRecordMapsToItsEnd(t *testing.T) {
	left := []string{"leave_requested/user_requested", "left_meeting", "post_processing_completed"}
	for _, tc := range []struct {
		name   string
		record []string
		own    calendar.BotEnd
		want   calendar.BotEnd
	}{
		{"the leave rule", left, calendar.EndLeaveRule, calendar.EndLeaveRule},
		{"a failed chat check", left, calendar.EndChatCheckFailed, calendar.EndChatCheckFailed},
		{"a lost link", left, calendar.EndLinkLost, calendar.EndLinkLost},
		{"a failed socket", left, calendar.EndSocketFailed, calendar.EndSocketFailed},
		{"a leave a restarted bridge did not ask for", left, 0, calendar.EndLeaveRule},
		{"Attendee's automatic leave", []string{"leave_requested/auto_leave_silence", "left_meeting"}, 0, calendar.EndAutoLeave},
		{"the meeting ended", []string{"meeting_ended", "post_processing_completed"}, 0, calendar.EndMeetingEnded},
		{"nobody admitted it", []string{"could_not_join_meeting/waiting_room_timeout_exceeded"}, 0, calendar.EndCouldNotJoin},
		{"a crash", []string{"fatal_error/process_terminated"}, 0, calendar.EndFatalError},
		{"a crash after the bridge's leave", []string{"leave_requested/user_requested", "fatal_error/process_terminated"}, calendar.EndLeaveRule, calendar.EndLeaveRule},
		{"a leave before the admission", []string{"leave_requested/user_requested", "could_not_join_meeting/leave_requested_before_bot_joined"}, calendar.EndLeaveRule, calendar.EndLeaveRule},
		{"no record of how", nil, 0, calendar.EndFatalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append(botRecord(botT0, "join_requested", "joined_meeting"), botRecord(botT0, tc.record...)...)
			if got := endOf(events, tc.own); got != tc.want {
				t.Fatalf("endOf = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLeaveAtLeavesWhateverTheRosterThenIdles(t *testing.T) {
	leaveAt := botT0.Add(10 * time.Minute)
	h := newBotHarness(t,
		botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)),
		botCalEvent("ev2", botT0.Add(40*time.Minute), botT0.Add(50*time.Minute)))
	h.faults = faultSet{LeaveAt: leaveAt}
	h.start()
	h.linkUp(botPiHello(botTexts()))
	id := h.inCall()
	h.join(id, "p1", "Person One")
	h.advanceTo(leaveAt.Add(-time.Second))
	if n := len(h.api.callsOf("Leave")); n != 0 {
		t.Fatalf("the bot left before the run's end_time: %d", n)
	}
	h.advance(2 * time.Second)
	leaves := h.api.callsOf("Leave")
	if len(leaves) != 1 || leaves[0].At.Before(leaveAt) {
		t.Fatalf("Leave calls = %+v, want one at the end_time with p1 still in the call", leaves)
	}
	h.end(id, "ended", botRecord(h.clk.Now(), "leave_requested/user_requested", "left_meeting", "post_processing_completed")...)
	h.advanceTo(botT0.Add(45 * time.Minute))
	if n := len(h.api.callsOf("CreateBot")); n != 1 {
		t.Fatalf("bots made after the end_time = %d, want none", n-1)
	}
}

// A1: each Pi hello gets the bridge's hello before any other message.
func TestTheBridgeAnswersEachPiHelloWithItsHelloFirst(t *testing.T) {
	h, _ := joinOneBot(t)
	first := h.link.sent[0]
	if first.Type != call.TypeHello || first.Hello == nil {
		t.Fatalf("first message = %+v, want the bridge's hello", first)
	}
	if hl := *first.Hello; hl.Build != bridgeBuild || !slices.Equal(hl.LinkVersions, call.Accepted) || hl.Event != nil || hl.BotState != "" {
		t.Fatalf("the bridge's hello with no bot = %+v", hl)
	}
	if h.link.sent[1].Type != call.TypeCall {
		t.Fatalf("second message = %+v, want call joining", h.link.sent[1])
	}
	h.inCall()
	h.linkDown()
	h.advance(5 * time.Second)
	n := len(h.link.sent)
	h.linkUp(botPiHello(botTexts()))
	again := h.link.sent[n]
	if again.Type != call.TypeHello || again.Hello.Event == nil || again.Hello.Event.ID != "ev1" || again.Hello.BotState != "in_call" {
		t.Fatalf("first message after the second hello = %+v, want the hello with ev1 in_call", again)
	}
}

// A1 and R15 (c-link-versions): with no common link version the bridge sends
// its hello alone and makes no bot.
func TestNoBotWithNoCommonVersion(t *testing.T) {
	h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
	h.start()
	hello := botPiHello(botTexts())
	hello.LinkVersions = []int{2}
	h.linkUp(hello)
	h.advance(3 * time.Minute)
	if len(h.link.sent) != 1 || h.link.sent[0].Type != call.TypeHello {
		t.Fatalf("messages sent with no common version = %+v, want the hello alone", h.link.sent)
	}
	if n := len(h.api.callsOf("CreateBot")); n != 0 {
		t.Fatalf("bots made with no common version = %d, want 0", n)
	}
}

// Upkeep may delete a bot's data before the bridge's delete_data: Attendee
// then reports data_deleted, which is an end as ended is. The bridge takes
// the end from the events Attendee keeps, calls no delete_data, and frees the
// call for the next event (R7, R14).
func TestADataDeletedBotIsAnEnd(t *testing.T) {
	for _, byWebhook := range []bool{true, false} {
		name := "by the poll"
		if byWebhook {
			name = "by webhook"
		}
		t.Run(name, func(t *testing.T) {
			startA := botT0.Add(2 * time.Minute)
			startB := startA.Add(10 * time.Minute)
			h := newBotHarness(t,
				botCalEvent("evA", startA, startA.Add(30*time.Minute)),
				botCalEvent("evB", startB, startB.Add(40*time.Minute)))
			h.start()
			h.linkUp(botPiHello(botTexts()))
			id := h.inCall()
			h.advance(time.Minute)
			record := botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed", "data_deleted")
			if byWebhook {
				h.end(id, "data_deleted", record...)
			} else {
				h.api.setBot(id, "data_deleted", record...)
			}
			h.advance(pollEvery)
			if !h.cal.event("evA").Done {
				t.Fatal("evA is not done after its meeting ended and upkeep deleted its bot's data")
			}
			for _, op := range []string{"DeleteData", "DeleteDataRefused"} {
				if n := len(h.api.callsOf(op)); n != 0 {
					t.Fatalf("%s calls = %d for a bot whose data is deleted", op, n)
				}
			}
			h.advanceTo(startB.Add(-5 * time.Minute).Add(31 * time.Second))
			if created := h.api.callsOf("CreateBot"); len(created) != 2 || created[1].Text != "evB" {
				t.Fatalf("bots made = %+v, want evB's after evA's bot ended", created)
			}
		})
	}
}

// Only the call's bot's socket secret is taken: at its end the bridge
// retires it, so the ended bot's reconnect is refused.
func TestABotsSocketIsRetiredAtItsEnd(t *testing.T) {
	h, id := joinOneBot(t)
	h.inCall()
	nonce := h.api.createdSettings()[0].Nonce
	if current, _ := h.sockets.taken(); current != nonce {
		t.Fatalf("the socket takes %q, want the call's bot's nonce %q", current, nonce)
	}
	h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
	if current, _ := h.sockets.taken(); current != "" {
		t.Fatalf("the socket still takes %q after its bot ended", current)
	}
}

// botLifeSentinel stands for words of a meeting inside an error's text.
const botLifeSentinel = "zebra-sentinel-8830"

// botLifeErrs is the errors of a fake's next calls, oldest first.
type botLifeErrs struct {
	mu   sync.Mutex
	errs []error
}

func (e *botLifeErrs) next() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.errs) == 0 {
		return nil
	}
	err := e.errs[0]
	e.errs = e.errs[1:]
	return err
}

// botLifeErrCalendar fails the calendar's next reads and done writes.
type botLifeErrCalendar struct {
	calendar.Reader
	events, marks botLifeErrs
}

func (c *botLifeErrCalendar) Events(ctx context.Context, from, to time.Time) ([]calendar.Event, error) {
	if err := c.events.next(); err != nil {
		return nil, err
	}
	return c.Reader.Events(ctx, from, to)
}

func (c *botLifeErrCalendar) MarkDone(ctx context.Context, id string) error {
	if err := c.marks.next(); err != nil {
		return err
	}
	return c.Reader.MarkDone(ctx, id)
}

// botLifeErrAttendee fails Attendee's next reads of a bot.
type botLifeErrAttendee struct {
	*fakeAttendee
	reads botLifeErrs
}

func (a *botLifeErrAttendee) Bot(ctx context.Context, id string) (bot, error) {
	if err := a.reads.next(); err != nil {
		return bot{}, err
	}
	return a.fakeAttendee.Bot(ctx, id)
}

// botLifeStderr takes what goes through the standard logger, which net/http
// and any other library write to the process's stderr, until the test ends.
func botLifeStderr(t *testing.T) *botLogs {
	t.Helper()
	l := &botLogs{}
	stdlog.SetOutput(l)
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })
	return l
}

// botLifeNoSentinel fails the test when stdout lacks a line it wants, which
// would make the check empty, or when stdout or stderr holds the sentinel.
func botLifeNoSentinel(t *testing.T, stdout, stderr *botLogs, want ...string) {
	t.Helper()
	out := stdout.String()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("the log has no %s line, so it cannot show the error's text left out:\n%s", w, out)
		}
	}
	if strings.Contains(out, botLifeSentinel) {
		t.Errorf("stdout holds the error's text:\n%s", out)
	}
	if s := stderr.String(); strings.Contains(s, botLifeSentinel) {
		t.Errorf("stderr holds the error's text: %s", s)
	}
}

// C9 and R7: a failure's line holds its operation, ids and the error's
// class, never the error's text, which can repeat what an API's answer, a
// malformed time or a malformed JSON body held.
func TestTheBridgeLogsNoErrorText(t *testing.T) {
	_, timeErr := time.Parse(time.RFC3339, botLifeSentinel)
	t.Run("an API error and a malformed time", func(t *testing.T) {
		stderr := botLifeStderr(t)
		h := newBotHarness(t, botCalEvent("ev1", botT0.Add(2*time.Minute), botT0.Add(32*time.Minute)))
		cal := &botLifeErrCalendar{Reader: h.cal}
		cal.events.errs = []error{
			fmt.Errorf("list the bot user's events: %w", &googleapi.Error{Code: 503, Message: botLifeSentinel, Body: botLifeSentinel}),
			fmt.Errorf("event ev1: read its start: %w", timeErr),
		}
		cal.marks.errs = []error{fmt.Errorf("patch event ev1's record: %w", &googleapi.Error{Code: 403, Message: botLifeSentinel})}
		api := &botLifeErrAttendee{fakeAttendee: h.api}
		api.reads.errs = []error{fmt.Errorf("attendee: GET /bots/bot_1: %w", errors.New(botLifeSentinel))}
		h.calendar, h.attendee = cal, api
		h.start()            // the calendar's 503
		h.advance(joinEvery) // the malformed time
		h.advance(joinEvery)
		h.linkUp(botPiHello(botTexts()))
		id := h.inCall()
		h.advance(pollEvery) // the read of the bot fails
		h.end(id, "ended", botRecord(h.clk.Now(), "meeting_ended", "post_processing_completed")...)
		botLifeNoSentinel(t, h.logs, stderr, "RESTART-READ-FAILED source=calendar", "BOT-READ-FAILED", "MARK-DONE-FAILED")
	})
	t.Run("a malformed JSON body", func(t *testing.T) {
		stderr := botLifeStderr(t)
		f := newFakeAttendeeServer(t)
		f.on(http.MethodGet, "/bots", attendeeTestReply(http.StatusOK,
			`{"next":null,"results":[{"id":"bot_1","state":"ended","metadata":{},"events":[{"type":"joined_meeting","created_at":"`+botLifeSentinel+`"}]}]}`))
		h := newBotHarness(t)
		h.attendee = f.client()
		h.start()
		botLifeNoSentinel(t, h.logs, stderr, "RESTART-READ-FAILED source=attendee")
	})
	t.Run("a webhook the bridge cannot read", func(t *testing.T) {
		stderr := botLifeStderr(t)
		logs := &botLogs{}
		r, err := newWebhookReceiver(webhookTestSecret, make(chan webhookEvent, 1), newLogger(logs, "bridge", slog.LevelDebug))
		if err != nil {
			t.Fatal(err)
		}
		// Already in sign_payload's form: sorted keys, no spaces, ASCII.
		body := []byte(`{"bot_id":"bot_1","data":{},"idempotency_key":"k1","trigger":"` + botLifeSentinel + `"}`)
		secret, err := base64.StdEncoding.DecodeString(webhookTestSecret)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		if code := webhookPost(t, r, body, base64.StdEncoding.EncodeToString(mac.Sum(nil))); code != http.StatusOK {
			t.Fatalf("status %d, want 200 for a signed webhook the bridge cannot read", code)
		}
		botLifeNoSentinel(t, logs, stderr, "WEBHOOK-UNREAD")
	})
}
