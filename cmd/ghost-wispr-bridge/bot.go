package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// faultSet is the test pool's faults that change the bot's life (C8). It is
// zero in the release build; the callfault build's applyFaults sets it from
// the run spec.
type faultSet struct {
	ChatCheckAlwaysPasses bool      // chat_check_always_passes: the check passes at the admission, with no echo
	NoticeJoinOnly        bool      // notice_join_only: the consent notice only at the bot's own join
	LongLines             bool      // long_lines: after the chat check, post lines of 400, 499, 500, 501 and 2,000 characters
	LeaveAt               time.Time // the run spec's end_time: leave then whatever the roster, then idle
}

// audioSockets is the audio socket (audiosocket.go) as runBridge uses it. It
// takes the socket of one bot at a time, the call's bot's.
type audioSockets interface {
	Register(nonce string) // take only the socket of the bot whose metadata holds this nonce; any other bot's is retired
	Retire(nonce string)   // take that bot's socket no more, and close it
	Connected() bool       // the taken bot's socket is open now
}

// bridgeDeps is what runBridge works with, so each test gives it fakes.
type bridgeDeps struct {
	API      attendeeAPI
	Link     call.PiLink // nil: no link (the release build until T4.4's client); the bridge then makes no bot and sends nothing
	Calendar calendar.Reader
	Clock    clock
	Log      *slog.Logger
	Faults   faultSet
	Webhooks <-chan webhookEvent // Attendee's webhooks (webhook.go)
	Audio    <-chan audioIn      // Attendee's audio (audiosocket.go)
	Sockets  audioSockets        // required: the audio socket, or a test's fake of it
}

const (
	// tickEvery is the loop's beat: each time bound below is checked once a
	// second.
	tickEvery = time.Second
	// pollEvery is the read of the bot during a call, so the bridge sees
	// ended within 30 s when the webhook is lost (spec, "Deletion").
	pollEvery = 30 * time.Second
	// socketGiveUp is how long a joined bot's audio socket may stay closed:
	// Attendee tries 30 times, 10 s apart, then stops
	// (bots/bot_controller/bot_websocket_client.py:39-40,90-127).
	socketGiveUp = 300 * time.Second
	// callLimit bounds each call of Attendee or the calendar that runs
	// beside the loop, and each the loop makes while no bot is in a call.
	callLimit = 10 * time.Second
	// loopCallLimit bounds each call the loop waits for while a bot is in
	// the call: a chat post, a leave, a removal. Attendee answers within
	// about 1 s (attendeeTimeout).
	loopCallLimit = 5 * time.Second
	// doneWait is how long a Done end's delete_data waits for the done mark
	// at most from the moment the bridge sees the end; the mark is tried
	// again after it.
	doneWait = 30 * time.Second
	// deleteWithin is the spec's bound on delete_data after a bot's end
	// (Limits, "Deletion"; R7), from the end in the bot's record: the bridge
	// may see the end later, by a lost webhook's poll and its read, and then
	// waits for the done mark only for what is left of it, less a tick.
	deleteWithin = 60 * time.Second
	// deleteDataLimit bounds delete_data's tries: R7's 10 minutes, after
	// which upkeep's loop deletes the rows.
	deleteDataLimit = 10 * time.Minute
)

// Attendee's bot states, as its API names them (bots/models.py,
// BotStates.state_to_api_code).
const (
	stateScheduled   = "scheduled"
	stateEnded       = "ended"
	stateFatalError  = "fatal_error"
	stateDataDeleted = "data_deleted"
)

// joinedState: joined_not_recording, joined_recording,
// joined_recording_paused, joined_recording_permission_denied.
func joinedState(s string) bool { return strings.HasPrefix(s, "joined_") }

// endedState is a state delete_data takes (bots/models.py:1868-1871).
func endedState(s string) bool { return s == stateEnded || s == stateFatalError }

// finalState is the end of a bot's record: ended, fatal_error, or
// data_deleted, which upkeep's delete_data may reach before the bridge's.
func finalState(s string) bool { return endedState(s) || s == stateDataDeleted }

// The bot's states on the link (C2's call message).
const (
	botJoining  = "joining"
	botAdmitted = "admitted"
	botInCall   = "in_call"
	botLeft     = "left"
)

// runBridge serves one call at a time until ctx ends: it makes each call's
// bot from the calendar, checks its chat, posts its notices, makes it leave,
// and deletes Attendee's copies after it. Its loop keeps every time bound:
// the reads of the calendar and of Attendee run beside it, and each call it
// waits for has its own bound.
//
//nolint:gocritic // hugeParam: the contract passes settings and bridgeDeps by value
func runBridge(ctx context.Context, s settings, deps bridgeDeps) error {
	b := newBridge(&s, &deps)
	defer b.bg.Wait()
	b.start(ctx)
	var recv <-chan call.Message
	if b.link != nil {
		recv = b.link.Recv()
	}
	tick := b.clock.After(tickEvery)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			b.tick(ctx)
			tick = b.clock.After(tickEvery)
		case apply := <-b.results:
			b.take(apply)
		case ev := <-b.webhooks:
			b.onWebhook(ctx, ev)
		case a := <-b.audio:
			b.onAudio(ctx, &a)
		case m, ok := <-recv:
			if !ok {
				recv = nil
				continue
			}
			b.onLink(ctx, m)
		}
	}
}

// bridge is runBridge's state. One goroutine runs its handlers and changes
// it; the calls that run beside it hand their results back through results.
type bridge struct {
	s        *settings
	api      attendeeAPI
	link     call.PiLink
	cal      calendar.Reader
	clock    clock
	log      *slog.Logger
	faults   faultSet
	sockets  audioSockets
	webhooks <-chan webhookEvent
	audio    <-chan audioIn

	startedAt   time.Time
	restored    bool      // Attendee's bots were read; no bot is made before
	lastRestore time.Time // the last try of the restart's reads
	answered    uint64    // the link-up whose Pi's hello the bridge answered (call.PiLink's Generation); 0: none
	bot         *liveBot  // the bot of this instance; nil when none
	others      map[string]*liveBot
	lastPass    time.Time
	reading     bool           // a pass's calendar read runs beside the loop
	passAgain   bool           // a pass was asked for while one read
	endedBot    *liveBot       // the call's bot that ended since the last pass, for its busy check
	attempt     *createAttempt // a CreateBot that failed: its bot may exist
	kept        []call.Control // busy and chat_check messages the link has not taken yet
	sent        []sentOwed     // owed controls a link took and has not kept long enough (pending.go)
	done        map[string]bool
	markOwed    map[string]bool         // events whose done mark has not landed, tried again at each pass
	writing     map[string]bool         // events whose record a write beside the loop changes: one at a time (pending.go)
	records     map[string]*eventRecord // events' pending records (pending.go)
	deleteOwed  map[string][]owedDelete // Done ends' delete_data waiting for the done mark
	idle        bool                    // the run spec's end_time passed: no more bots
	rate        rateCount

	// passAfterWrite: a pass's next bot waits for a write of its event's
	// record, whose end makes the pass again.
	passAfterWrite bool

	results  chan func() // results of the calls beside the loop, applied on it
	inflight int         // calls beside the loop whose result the loop has not applied
	bg       sync.WaitGroup
}

// owedDelete is a Done end's delete_data that waits for its done mark until
// due at most.
type owedDelete struct {
	bot string
	due time.Time
}

// createAttempt is a CreateBot that failed. Attendee may have made its bot,
// whose metadata holds the nonce, so the bridge looks for it before it makes
// another.
type createAttempt struct {
	event calendar.Event
	nonce string
	joins int
	at    time.Time
}

// liveBot is one bot from its creation to the end of its record.
type liveBot struct {
	id, nonce  string
	state      string // Attendee's last state of the bot that the bridge read
	event      calendar.Event
	created    time.Time // the bridge's CreateBot; for a bot a restarted bridge found, its record's first event, else the bridge's start; never zero
	adopted    bool      // a restarted bridge found the bot in Attendee's list
	extra      bool      // a second live bot a restarted bridge found: it leaves, and only an end the bridge did not ask for decides its event
	superseded bool      // an older bot of its event, or one whose event is done or gone: its end decides nothing
	cancelled  bool      // removed before its admission, as its event no longer asks for it: its end decides nothing

	admitted   time.Time // Attendee's admission; zero before
	check      chatCheck
	echoFrom   time.Time       // the earliest time of a line that may be the echo: the first consent post, or a found bot's admission
	roster     map[string]bool // Meet participant ids in the call, not the bot's
	seen       map[string]bool // every participant that was ever in the roster: a line of one is never the echo
	needRoster bool            // a restarted bridge has not read the roster yet: no leave rule and no passed check before
	needChat   bool            // a restarted bridge has not read the chat lines yet: no passed check before
	readsFrom  time.Time       // when a restarted bridge began those reads; zero for a bot it made
	rosterRead bool            // the roster read runs beside the loop
	chatRead   bool            // the chat read runs beside the loop
	liveJoins  []participantEvent
	notices    consentNotices

	leaveKind  calendar.BotEnd // why the bridge asked the bot to leave; zero when it has not
	leaveAsked time.Time       // when the bridge first asked for the leave
	leaving    bool            // Attendee took the leave
	departing  bool            // a leave of a bot beside the call's runs beside the loop
	lastPoll   time.Time
	polling    bool      // a read of the bot runs beside the loop
	repoll     bool      // read it again when the running read returns
	lastSocket time.Time // the last tick the audio socket was connected, from the admission
}

func newLiveBot(id, nonce string, ev *calendar.Event, now time.Time) *liveBot {
	return &liveBot{id: id, nonce: nonce, event: *ev, lastPoll: now, roster: map[string]bool{}, seen: map[string]bool{}}
}

func newBridge(s *settings, d *bridgeDeps) *bridge {
	return &bridge{
		s: s, api: d.API, link: d.Link, cal: d.Calendar, clock: d.Clock, log: d.Log, faults: d.Faults,
		sockets: d.Sockets, webhooks: d.Webhooks, audio: d.Audio,
		others: map[string]*liveBot{}, done: map[string]bool{}, markOwed: map[string]bool{},
		writing: map[string]bool{}, records: map[string]*eventRecord{}, deleteOwed: map[string][]owedDelete{},
		results: make(chan func()),
	}
}

// beside runs do's call beside the loop, bounded by limit, and hands the
// function do gives back to the loop, which applies it: so a slow calendar
// or Attendee holds no time bound of the loop. do must touch no state of the
// bridge; the function it gives runs on the loop.
func (b *bridge) beside(ctx context.Context, limit time.Duration, do func(ctx context.Context) func()) {
	b.inflight++
	b.bg.Add(1)
	go func() {
		defer b.bg.Done()
		cctx, cancel := context.WithTimeout(ctx, limit)
		apply := do(cctx)
		cancel()
		select {
		case b.results <- apply:
		case <-ctx.Done():
		}
	}()
}

// take applies a result of a call beside the loop.
func (b *bridge) take(apply func()) {
	b.inflight--
	apply()
}

// start reads Attendee's bots and makes a first pass. startBridge logged
// BRIDGE-START before it.
func (b *bridge) start(ctx context.Context) {
	b.startedAt = b.clock.Now()
	b.restore(ctx)
	b.joinPass(ctx)
}

func notIDRune(r rune) bool {
	return (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' && r != '_'
}

// logID is id for a log line when it is an id, and "invalid" when it holds
// anything else or is long: a log line holds ids, never a value of a body.
func logID(id string) string {
	if id == "" || len(id) > 64 || strings.IndexFunc(id, notIDRune) >= 0 {
		return "invalid"
	}
	return id
}

// tick runs once a second: the run's end time, the bot's time bounds, the
// rate count's windows, the kept messages and the owed ones a link took, the
// waiting deletions, and the pass every 30 s.
func (b *bridge) tick(ctx context.Context) {
	now := b.clock.Now()
	// A failed restart read waits for the join pass's period: each read of
	// the calendar is a delegated sign-in.
	if !b.restored && now.Sub(b.lastRestore) >= joinEvery {
		b.restore(ctx)
	}
	if !b.idle && !b.faults.LeaveAt.IsZero() && !now.Before(b.faults.LeaveAt) {
		b.idle = true
		b.log.Info("RUN-END-TIME passed: the bridge makes no more bots")
	}
	if lb := b.bot; lb != nil {
		b.readAdopted(ctx, lb)
		b.botTick(ctx, lb, now)
	}
	b.pollOthers(ctx, now)
	b.closeRate(now)
	b.confirmSent(ctx, now)
	b.flushKept()
	b.deleteDue(ctx, now)
	if now.Sub(b.lastPass) >= joinEvery {
		b.joinPass(ctx)
	}
}

// botTick checks the bot's time bounds: the chat check's 30 s first, then
// the poll, the notice window, the socket, the leave rule and the run's end
// time.
func (b *bridge) botTick(ctx context.Context, lb *liveBot, now time.Time) {
	b.evalCheck(ctx, lb, now)
	if now.Sub(lb.lastPoll) >= pollEvery {
		b.poll(ctx, lb, now)
	}
	if lb.notices.due(now) {
		b.postConsent(ctx, lb, now)
	}
	if b.sockets.Connected() {
		lb.lastSocket = now
	}
	switch {
	case lb.leaving:
	case lb.cancelled:
		b.cancel(ctx, lb)
	case lb.leaveKind != 0:
		b.leave(ctx, lb.leaveKind)
	case !lb.admitted.IsZero() && now.Sub(lb.lastSocket) >= socketGiveUp:
		b.log.Warn("SOCKET-LOST bot=" + lb.id)
		if texts := b.texts(); texts != nil {
			b.post(ctx, lb, texts.Lost, "lost")
		}
		b.leave(ctx, calendar.EndSocketFailed)
	case b.idle, !lb.needRoster && !now.Before(lb.event.End) && len(lb.roster) == 0:
		b.leave(ctx, calendar.EndLeaveRule)
	}
}

// holds tells whether lb is still a bot of this bridge: the call's, or one
// beside it.
func (b *bridge) holds(lb *liveBot) bool { return lb == b.bot || b.others[lb.id] == lb }

// poll reads the bot's record beside the loop, and takes its admission or
// its end.
func (b *bridge) poll(ctx context.Context, lb *liveBot, now time.Time) {
	lb.lastPoll = now
	if lb.polling {
		lb.repoll = true
		return
	}
	lb.polling = true
	api, id := b.api, lb.id
	b.beside(ctx, callLimit, func(cctx context.Context) func() {
		bt, err := api.Bot(cctx, id)
		return func() { b.takePoll(ctx, lb, &bt, err) }
	})
}

func (b *bridge) takePoll(ctx context.Context, lb *liveBot, bt *bot, err error) {
	lb.polling = false
	if !b.holds(lb) {
		return
	}
	if err != nil {
		b.log.Warn("BOT-READ-FAILED bot="+lb.id, "error", errorClass(err))
	} else {
		lb.state = bt.State
		switch {
		case finalState(bt.State):
			b.endBot(ctx, lb, bt)
			return
		case lb == b.bot && lb.admitted.IsZero() && joinedState(bt.State):
			b.admit(ctx, lb, admission(bt.Events, b.clock.Now()))
		case lb != b.bot && !lb.leaving:
			b.departOther(ctx, lb)
		}
	}
	if lb.repoll {
		lb.repoll = false
		b.poll(ctx, lb, b.clock.Now())
	}
}

// pollOthers reads, every pollEvery, each other bot of ours that Attendee
// still holds, asks it again to leave until Attendee takes the leave, and
// takes its end: botTick's poll reads only the call's bot, and an other bot's
// ended webhook may be lost or come while its record cannot be read.
func (b *bridge) pollOthers(ctx context.Context, now time.Time) {
	for _, lb := range b.others {
		if now.Sub(lb.lastPoll) >= pollEvery {
			b.poll(ctx, lb, now)
		}
	}
}

// departBot makes a bot leave its call. A bot still scheduled for its
// join_at cannot leave (bots/models.py:1846-1858): Attendee deletes it
// (DELETE /api/v1/bots/{id}, bots/bots_api_utils.py:463-488), and refuses
// that with 400 once it launched, when the bot leaves as a launched one does.
func departBot(ctx context.Context, api attendeeAPI, id string, scheduled bool) (removed bool, err error) {
	if scheduled {
		err := api.Remove(ctx, id)
		if err == nil {
			return true, nil
		}
		var se *attendeeStatusError
		if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
			return false, err
		}
	}
	return false, api.Leave(ctx, id)
}

// depart makes the call's bot leave within limit, and frees the call when
// Attendee deleted a bot that had not launched.
func (b *bridge) depart(ctx context.Context, lb *liveBot, limit time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, limit)
	removed, err := departBot(cctx, b.api, lb.id, lb.state == stateScheduled && lb.admitted.IsZero())
	cancel()
	if err != nil {
		return err
	}
	lb.leaving = true
	if removed {
		b.log.Info(fmt.Sprintf("BOT-REMOVED bot=%s event=%s", lb.id, lb.event.ID))
		b.release(ctx, lb)
	}
	return nil
}

// leave asks Attendee to make the call's bot leave, for kind's reason, once
// the reason is on the event's record, so a restart of the bridge before the
// bot's end reads why it left (spec, the table of ends). T4.4's lost link
// calls it with EndLinkLost. After a failed chat check the record's write
// and the leave are bounded by what is left of its 10 s (R2).
func (b *bridge) leave(ctx context.Context, kind calendar.BotEnd) {
	lb := b.bot
	if lb == nil || lb.leaving {
		return
	}
	if lb.leaveKind == 0 {
		lb.leaveKind, lb.leaveAsked = kind, b.clock.Now()
	}
	failedCheck := lb.leaveKind == calendar.EndChatCheckFailed
	recordBy := lb.leaveAsked.Add(leaveRecordWait)
	if failedCheck {
		recordBy = lb.checkDeadline().Add(chatLeaveWithin - loopCallLimit)
	}
	if !b.recordLeave(ctx, lb, recordBy) {
		if b.clock.Now().Before(recordBy) {
			return // asked again at the next tick, or when the running write lands
		}
		b.log.Warn(fmt.Sprintf("LEAVE-UNRECORDED bot=%s reason=%s", lb.id, endName(lb.leaveKind)))
	}
	limit := loopCallLimit
	if failedCheck {
		if left := lb.checkDeadline().Add(chatLeaveWithin).Sub(b.clock.Now()); left > 0 && left < limit {
			limit = left
		}
	}
	if err := b.depart(ctx, lb, limit); err != nil {
		b.log.Warn("LEAVE-FAILED bot="+lb.id, "error", errorClass(err))
		return
	}
	b.log.Info(fmt.Sprintf("LEAVE bot=%s reason=%s", lb.id, endName(lb.leaveKind)))
	b.writeRecord(ctx, lb.event.ID) // the record the leave did not wait for
}

// cancel removes the call's bot before its admission, as its event no
// longer asks for it; a failed removal is tried again at the next tick.
func (b *bridge) cancel(ctx context.Context, lb *liveBot) {
	if err := b.depart(ctx, lb, loopCallLimit); err != nil {
		b.log.Warn("CANCEL-FAILED bot="+lb.id, "error", errorClass(err))
	}
}

// departOther makes a bot beside the call's leave, beside the loop; a failed
// leave is tried again at the bot's next poll.
func (b *bridge) departOther(ctx context.Context, lb *liveBot) {
	if lb.departing {
		return
	}
	lb.departing = true
	api, id, scheduled := b.api, lb.id, lb.state == stateScheduled
	b.beside(ctx, callLimit, func(cctx context.Context) func() {
		removed, err := departBot(cctx, api, id, scheduled)
		return func() {
			lb.departing = false
			if !b.holds(lb) {
				return
			}
			if err != nil {
				b.log.Warn("LEAVE-FAILED bot="+lb.id, "error", errorClass(err))
				return
			}
			lb.leaving = true
			if removed {
				b.log.Info(fmt.Sprintf("BOT-REMOVED bot=%s event=%s", lb.id, lb.event.ID))
				delete(b.others, lb.id)
			}
		}
	})
}

// onWebhook takes one of Attendee's webhooks for a bot of ours.
func (b *bridge) onWebhook(ctx context.Context, ev webhookEvent) {
	lb := b.bot
	if lb == nil || lb.id != ev.BotID {
		lb = b.others[ev.BotID]
	}
	if lb == nil {
		return
	}
	now := b.clock.Now()
	switch {
	case ev.State != nil:
		lb.state = ev.State.New
		if lb == b.bot && lb.admitted.IsZero() && joinedState(ev.State.New) {
			// The admission is the state change's own time: a webhook that
			// came late takes nothing from the check's 30 s.
			at := ev.State.At
			if at.IsZero() || at.After(now) {
				at = now
			}
			b.admit(ctx, lb, at)
		}
		if finalState(ev.State.New) {
			// The webhook does not say why the bot ended; its record does.
			b.poll(ctx, lb, now)
		}
	case ev.Chat != nil && lb == b.bot:
		b.onChat(ctx, lb, *ev.Chat, now)
	case ev.Participant != nil && lb == b.bot:
		b.onParticipant(ctx, lb, *ev.Participant, now)
	}
}

// admit starts the call at the admission at: the consent notice and the
// chat check's 30 s.
func (b *bridge) admit(ctx context.Context, lb *liveBot, at time.Time) {
	lb.admitted, lb.lastSocket = at, at
	b.log.Info(fmt.Sprintf("JOIN-TIME seconds=%d create_seconds=%d",
		int(at.Sub(b.startedAt)/time.Second), int(at.Sub(lb.created)/time.Second)))
	b.sendCall(&lb.event, botAdmitted)
	now := b.clock.Now()
	b.consentAtJoin(ctx, lb, now, now)
	if b.faults.ChatCheckAlwaysPasses {
		lb.check.state = checkPassed
		b.announceCheck(ctx, lb)
	}
}

// onParticipant keeps the roster, and posts the consent notice after each
// join (R3).
func (b *bridge) onParticipant(ctx context.Context, lb *liveBot, p participantEvent, now time.Time) {
	if lb.needRoster {
		lb.liveJoins = append(lb.liveJoins, p)
	}
	lb.takeParticipant(p)
	if !p.Join || p.ID == "" || p.ID == lb.check.sender {
		return
	}
	if !lb.admitted.IsZero() && !b.faults.NoticeJoinOnly {
		at := p.At
		if at.IsZero() {
			at = now
		}
		b.consentAtJoin(ctx, lb, at, now)
	}
}

// takeParticipant applies a join or a leave to the roster. Attendee leaves
// the bot's own events out (bots/bots_api_views.py:1567); the bot's echo
// sender is left out too.
func (lb *liveBot) takeParticipant(p participantEvent) {
	if p.ID == "" || p.ID == lb.check.sender {
		return
	}
	if p.Join {
		lb.roster[p.ID] = true
		lb.seen[p.ID] = true
	} else {
		delete(lb.roster, p.ID)
	}
}

// onChat takes a chat line. Until the check is decided, it may be the
// notice's echo. After it, the bot's own lines are never commands; the
// others are the pause and resume work's (T7).
func (b *bridge) onChat(ctx context.Context, lb *liveBot, m chatMessage, now time.Time) {
	if lb.check.state != checkPending {
		return
	}
	lb.noteLine(m, now)
	b.evalCheck(ctx, lb, now)
}

// consentAtJoin posts the consent notice for a join timed at and seen at now,
// or leaves it to the end of the notice window.
func (b *bridge) consentAtJoin(ctx context.Context, lb *liveBot, at, now time.Time) {
	if b.texts() == nil {
		return
	}
	if lb.notices.join(at, now) {
		b.postConsent(ctx, lb, now)
	}
}

// postConsent posts the consent notice. Only a post that Attendee took moves
// the notice's window; a failed one stays due and is posted at the next tick
// (R3).
func (b *bridge) postConsent(ctx context.Context, lb *liveBot, now time.Time) {
	texts := b.texts()
	if texts == nil {
		return
	}
	if !b.post(ctx, lb, texts.Consent, "consent") {
		lb.notices.failed(now.Add(tickEvery))
		return
	}
	lb.notices.posted(now)
	if lb.echoFrom.IsZero() {
		lb.echoFrom = now
	}
}

// post sends a line to the call's chat, and reports whether Attendee took
// it. The log names its kind, never its text.
func (b *bridge) post(ctx context.Context, lb *liveBot, text, kind string) bool {
	ctx, cancel := context.WithTimeout(ctx, loopCallLimit)
	defer cancel()
	if err := b.api.Chat(ctx, lb.id, text); err != nil {
		b.log.Warn(fmt.Sprintf("CHAT-POST-FAILED kind=%s bot=%s", kind, lb.id), "error", errorClass(err))
		return false
	}
	b.log.Info(fmt.Sprintf("CHAT-POST kind=%s bot=%s", kind, lb.id))
	return true
}

// onAudio counts the call's mixed audio against the bridge's clock (R10's
// rate count). Audio from a connection of another bot than the call's is
// not the call's. Carrying it to the Pi is T4.4's call mix.
func (b *bridge) onAudio(_ context.Context, a *audioIn) {
	lb := b.bot
	if !a.Mixed || lb == nil || a.Nonce != lb.nonce {
		return
	}
	at := a.At
	if at.IsZero() {
		at = b.clock.Now()
	}
	b.reportRate(b.rate.add(at, len(a.PCM)))
}

// closeRate closes the rate count's windows that ended a grace before now,
// so a window with no audio reports too.
func (b *bridge) closeRate(now time.Time) {
	b.reportRate(b.rate.close(now.Add(-rateGrace)))
}

// reportRate logs each closed window's rate, and tells the Pi of a fault.
func (b *bridge) reportRate(windows []int) {
	for _, sps := range windows {
		b.log.Info(fmt.Sprintf("RATE samples_per_second=%d", sps))
		if rateFault(sps) {
			b.log.Warn(fmt.Sprintf("RATE-FAULT samples_per_second=%d", sps))
			b.send(&call.Control{Type: call.TypeRateFault, RateFault: &call.RateFault{SamplesPerSecond: sps}})
		}
	}
}

// onLink takes the Pi's messages. Each hello is a link-up: the bridge answers
// it first with its own hello (A1), then sends what waited, the owed controls
// the link before it took and may have lost included, then makes a pass. The
// Pi's other messages are the link's later work (T4.4 on).
func (b *bridge) onLink(ctx context.Context, m call.Message) {
	if m.Control == nil || m.Control.Type != call.TypeHello {
		return
	}
	b.resendSent()
	now := b.clock.Now()
	lb := b.bot
	decided := lb != nil && b.judgeCheck(lb, now)
	b.sendHello()
	if decided {
		b.announceCheck(ctx, lb)
	}
	b.flushKept()
	b.joinPass(ctx)
}

// sendHello answers the Pi's hello: the build, the link versions, the pause
// state and the call (A1). The bridge keeps no pause state before the pause
// work (T7), so it reports none. It answers the link-up the link is on
// before the send: a link that comes back during the send is a new link-up,
// whose hello the bridge answers when it takes it.
func (b *bridge) sendHello() {
	if b.link == nil {
		return
	}
	gen := b.link.Generation()
	h := call.Hello{Build: bridgeBuild, LinkVersions: call.Accepted, BotState: b.botState()}
	if b.bot != nil {
		ev := callEvent(&b.bot.event)
		h.Event = &ev
	}
	if err := b.link.Send(call.Control{Type: call.TypeHello, Hello: &h}); err != nil {
		b.log.Warn("LINK-SEND-FAILED type=hello", "error", errorClass(err))
		return
	}
	b.answered = gen
}

// linkReady: the link is up on the link-up whose hello the bridge answered,
// and both sides share a link version (R15). Only then does the bridge make
// a bot or send more than its hello. A link that dropped and came back is
// not ready before the bridge answers the new hello, which may still wait on
// Recv while a tick runs, so the dropped link's owed controls are not counted
// received on the new one (confirmSent).
func (b *bridge) linkReady() bool {
	if b.link == nil || !b.link.Up() || b.link.Generation() != b.answered {
		return false
	}
	_, ok := call.Choose(call.Accepted, b.link.Hello().LinkVersions)
	return ok
}

// send sends a control message while the link is ready, and drops it
// otherwise: the bridge sends the Pi nothing while the link is down (R1).
func (b *bridge) send(c *call.Control) {
	if !b.linkReady() {
		return
	}
	if err := b.link.Send(*c); err != nil {
		b.log.Warn("LINK-SEND-FAILED type="+c.Type, "error", errorClass(err))
	}
}

// texts are the Pi's texts from its last hello; nil before.
func (b *bridge) texts() *call.Texts {
	if b.link == nil {
		return nil
	}
	return b.link.Hello().Texts
}

func (b *bridge) botState() string {
	switch lb := b.bot; {
	case lb == nil:
		return ""
	case lb.admitted.IsZero():
		return botJoining
	case lb.check.state == checkPassed:
		return botInCall
	default:
		return botAdmitted
	}
}

func callEvent(e *calendar.Event) call.Event {
	return call.Event{ID: e.ID, Organizer: e.Organizer, Start: e.Start, End: e.End}
}

// sendCall tells the Pi the bot's state in the call of ev (C2's call).
func (b *bridge) sendCall(ev *calendar.Event, state string) {
	b.send(&call.Control{Type: call.TypeCall, Call: &call.Call{Event: callEvent(ev), BotState: state}})
}

// endBot takes the end of a bot's record by the table of ends. A Done end
// marks the event done, and its delete_data waits for the mark at most
// doneWait, and never past deleteWithin after the bot's end; the call's
// bot's end frees the call. A bot whose data upkeep deleted first gets no
// delete_data.
func (b *bridge) endBot(ctx context.Context, lb *liveBot, bt *bot) {
	now := b.clock.Now()
	kind := endOf(bt.Events, lb.leaveKind)
	result := "none"
	switch {
	case lb.superseded, lb.cancelled:
	case lb.extra && kind == calendar.EndLeaveRule:
		// The bridge's leave of a second bot is not R1's leave rule (R14).
	case calendar.EndResult(kind, lb.event, now) == calendar.Done:
		result = "done"
	default:
		result = "new_bot"
	}
	b.log.Info(fmt.Sprintf("BOT-END bot=%s event=%s end=%s result=%s", lb.id, lb.event.ID, endName(kind), result))
	if result == "done" {
		b.markDone(ctx, lb.event.ID)
	}
	if bt.State != stateDataDeleted {
		due := now.Add(doneWait)
		if last := endedAt(bt.Events, now).Add(deleteWithin - tickEvery); last.Before(due) {
			due = last
		}
		if result == "done" && b.markOwed[lb.event.ID] && now.Before(due) {
			b.deleteOwed[lb.event.ID] = append(b.deleteOwed[lb.event.ID], owedDelete{bot: lb.id, due: due})
		} else {
			b.deleteData(ctx, lb.id)
		}
	}
	if lb != b.bot {
		delete(b.others, lb.id)
		return
	}
	b.release(ctx, lb)
}

// release frees the call of its bot: its socket secret is retired, the Pi
// hears that it left, and the next pass may make the next event's bot.
func (b *bridge) release(ctx context.Context, lb *liveBot) {
	b.sockets.Retire(lb.nonce)
	b.sendCall(&lb.event, botLeft)
	b.bot = nil
	b.rate = rateCount{}
	b.endedBot = lb
	b.joinPass(ctx)
}

// errEventGone is a calendar's answer for an event it does not have.
var errEventGone = errors.New("the calendar has no such event")

// eventGone tells whether err says the calendar no longer has the event:
// the Calendar API's 404 or 410, or a run spec calendar's other id. Such an
// event is done at once, since no bot can join it again.
func eventGone(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == http.StatusNotFound || gerr.Code == http.StatusGone
	}
	return errors.Is(err, errEventGone)
}

// markDone records the event done, first in this bridge, then in the
// calendar beside the loop, after any control its record owes (writeRecord);
// a failed write is tried again at each pass. Once the mark lands, or the
// calendar no longer has the event, the delete_data that waited for it runs.
func (b *bridge) markDone(ctx context.Context, id string) {
	b.done[id] = true
	b.markOwed[id] = true
	b.writeRecord(ctx, id)
}

// deleteDue runs each delete_data whose wait for its done mark is over:
// delete_data comes within 60 s of the end whatever the calendar says (spec,
// "Deletion"), and the mark is tried again after it.
func (b *bridge) deleteDue(ctx context.Context, now time.Time) {
	for id, owed := range b.deleteOwed {
		kept := owed[:0]
		for _, d := range owed {
			if !now.Before(d.due) {
				b.deleteData(ctx, d.bot)
				continue
			}
			kept = append(kept, d)
		}
		if len(kept) == 0 {
			delete(b.deleteOwed, id)
		} else {
			b.deleteOwed[id] = kept
		}
	}
}

// deleteData calls delete_data beside the loop: Attendee's client tries
// again until Attendee accepts, or for R7's 10 minutes.
func (b *bridge) deleteData(ctx context.Context, id string) {
	b.bg.Add(1)
	api := b.api
	go func() {
		defer b.bg.Done()
		ctx, cancel := context.WithTimeout(ctx, deleteDataLimit)
		defer cancel()
		if err := api.DeleteData(ctx, id); err != nil {
			b.log.Warn("DELETE-DATA-FAILED bot="+id, "error", errorClass(err))
			return
		}
		b.log.Info("DATA-DELETED bot=" + id)
	}()
}

// endOf reads how a bot ended from its record, oldest event first, by the
// spec's table of ends. own is the bridge's reason when it asked the bot to
// leave; a restarted bridge reads it from the event's record (restoreBot),
// and takes the leave rule when the record names none for the bot.
// MEETING_ENDED's remover is only in the webhook's event metadata, and a
// removal is Done as an end of the meeting is.
func endOf(events []botEvent, own calendar.BotEnd) calendar.BotEnd {
	for _, e := range events {
		switch e.Type {
		case "leave_requested":
			if strings.HasPrefix(e.SubType, "auto_leave_") {
				return calendar.EndAutoLeave
			}
			if own != 0 {
				return own
			}
			return calendar.EndLeaveRule
		case "meeting_ended":
			return calendar.EndMeetingEnded
		case "could_not_join_meeting":
			return calendar.EndCouldNotJoin
		case "fatal_error":
			return calendar.EndFatalError
		}
	}
	return calendar.EndFatalError
}

// admission is the time of the bot's join in its record.
func admission(events []botEvent, fallback time.Time) time.Time {
	for _, e := range events {
		if e.Type == "joined_meeting" && !e.At.IsZero() {
			return e.At
		}
	}
	return fallback
}

// endedAt is the time of a bot's end in its record: its newest event's, the
// one that ended it. A record with no time, or one later than now, gives
// now.
func endedAt(events []botEvent, now time.Time) time.Time {
	if n := len(events); n > 0 && !events[n-1].At.IsZero() && !events[n-1].At.After(now) {
		return events[n-1].At
	}
	return now
}

// endName names a bot's end in a log line.
func endName(k calendar.BotEnd) string {
	switch k {
	case calendar.EndLeaveRule:
		return "leave_rule"
	case calendar.EndChatCheckFailed:
		return "chat_check_failed"
	case calendar.EndMeetingEnded:
		return "meeting_ended"
	case calendar.EndRemoved:
		return "removed"
	case calendar.EndCouldNotJoin:
		return "could_not_join"
	case calendar.EndAutoLeave:
		return "auto_leave"
	case calendar.EndFatalError:
		return "fatal_error"
	case calendar.EndLinkLost:
		return "link_lost"
	case calendar.EndSocketFailed:
		return "socket_failed"
	case calendar.EndInstanceRestarted:
		return "instance_restarted"
	default:
		return "none"
	}
}
