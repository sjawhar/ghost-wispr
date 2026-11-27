package main

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// joinEvery is the bridge's read of the calendar between link-ups (spec,
// "Joining").
const joinEvery = 30 * time.Second

// joinPass reads the calendar at the bridge's start, every 30 s, at each
// link-up and at each bot's end, beside the loop: then it marks an event busy
// whose end passed during another call, removes a bot whose event no longer
// asks for it, and makes the next event's bot (R14). It first tries again
// each done mark that has not landed.
func (b *bridge) joinPass(ctx context.Context) {
	now := b.clock.Now()
	b.lastPass = now
	for id := range b.markOwed {
		b.markDone(ctx, id)
	}
	if !b.restored || b.idle {
		return
	}
	if b.reading {
		b.passAgain = true
		return
	}
	b.reading = true
	cal := b.cal
	b.beside(ctx, callLimit, func(cctx context.Context) func() {
		evs, err := cal.Events(cctx, now.Add(-calendar.PoolHold), now.Add(calendar.PoolLead))
		return func() { b.takePass(ctx, evs, err) }
	})
}

// takePass acts on a pass's read of the calendar.
func (b *bridge) takePass(ctx context.Context, evs []calendar.Event, err error) {
	b.reading = false
	ended := b.endedBot
	b.endedBot = nil
	defer func() {
		if b.passAgain {
			b.passAgain = false
			b.joinPass(ctx)
		}
	}()
	if err != nil {
		b.log.Warn("JOIN-READ-FAILED", "error", errorClass(err))
		return
	}
	now := b.clock.Now()
	for i := range evs {
		if b.done[evs[i].ID] {
			evs[i].Done = true
		}
	}
	busyBot := b.bot
	if busyBot == nil {
		busyBot = ended
	}
	b.markBusy(ctx, evs, now, busyBot)
	if lb := b.bot; lb != nil {
		b.reconcile(ctx, lb, evs, now)
	}
	if b.bot == nil && b.attempt != nil && !b.resolveAttempt(ctx) {
		return
	}
	if !b.linkReady() {
		return
	}
	hello := b.link.Hello()
	piBuildOK := hello.Build == call.BuildRelease || bridgeBuild == call.BuildCallfault
	ev, ok := calendar.NextToJoin(evs, now, true, hello.Texts != nil, piBuildOK, b.bot != nil)
	if !ok {
		return
	}
	b.createBot(ctx, &ev, hello.Texts, now)
}

// markBusy marks each event done whose end passed while this bot was on, and
// keeps its busy message until the link takes it: one call at a time.
func (b *bridge) markBusy(ctx context.Context, evs []calendar.Event, now time.Time, lb *liveBot) {
	if lb == nil {
		return
	}
	for _, e := range evs {
		if e.Done || e.ID == lb.event.ID || !e.End.After(lb.created) || now.Before(e.End) {
			continue
		}
		b.log.Info("BUSY event=" + e.ID)
		b.markDone(ctx, e.ID)
		b.keep(&call.Control{Type: call.TypeBusy, Busy: &call.Busy{Event: callEvent(&e)}})
	}
}

// reconcile removes the call's bot before its admission when its event no
// longer asks for it: the event is no longer among the calendar's call
// events, is done, or starts more than 5 minutes from now (R14). Otherwise
// the bot keeps the event as the calendar now has it.
func (b *bridge) reconcile(ctx context.Context, lb *liveBot, evs []calendar.Event, now time.Time) {
	if !lb.admitted.IsZero() || lb.cancelled || lb.leaving {
		return
	}
	reason := "gone"
	for i := range evs {
		if evs[i].ID != lb.event.ID {
			continue
		}
		switch {
		case evs[i].Done:
			reason = "done"
		case evs[i].Start.Sub(now) > calendar.CreateLead:
			reason = "moved"
		default:
			lb.event = evs[i]
			return
		}
	}
	lb.cancelled = true
	b.log.Info(fmt.Sprintf("BOT-CANCEL bot=%s event=%s reason=%s", lb.id, lb.event.ID, reason))
	b.cancel(ctx, lb)
}

// resolveAttempt settles a CreateBot that failed before the pass makes
// another: it reads Attendee's bots and takes the one whose metadata holds the
// failed call's nonce. It reports whether the pass may make a bot: not when
// the read failed, which is no proof that none exists, nor when it found the
// bot.
func (b *bridge) resolveAttempt(ctx context.Context) bool {
	at := b.attempt
	cctx, cancel := context.WithTimeout(ctx, callLimit)
	bots, err := b.api.Bots(cctx)
	cancel()
	if err != nil {
		b.log.Warn("BOT-CREATE-UNRESOLVED event="+at.event.ID, "error", errorClass(err))
		return false
	}
	b.attempt = nil
	for i := range bots {
		bt := &bots[i]
		if bt.Nonce != at.nonce || bt.EventID != at.event.ID {
			continue
		}
		lb := newLiveBot(bt.ID, at.nonce, &at.event, at.at)
		lb.created, lb.state = at.at, bt.State
		lb.lastPoll = time.Time{} // its record is read at the next tick
		b.bot = lb
		b.log.Info(fmt.Sprintf("BOT-FOUND bot=%s event=%s joins=%d", bt.ID, at.event.ID, at.joins))
		b.sendCall(&at.event, botJoining)
		return false
	}
	b.sockets.Retire(at.nonce)
	if at.joins >= calendar.MaxJoins {
		b.markDone(ctx, at.event.ID) // no bot is left to make: the pool can stop
		return false
	}
	return true
}

// createBot makes the event's bot: its socket secret registered and its join
// counted before Attendee makes it. A CreateBot that fails is kept, since
// Attendee may have made the bot.
func (b *bridge) createBot(ctx context.Context, ev *calendar.Event, texts *call.Texts, now time.Time) {
	nonce, err := newNonce()
	if err != nil {
		b.log.Error("BOT-CREATE-FAILED event="+ev.ID, "error", errorClass(err))
		return
	}
	b.sockets.Register(nonce)
	cctx, cancel := context.WithTimeout(ctx, callLimit)
	joins, err := b.cal.AddJoin(cctx, ev.ID)
	cancel()
	if err != nil {
		b.log.Warn("JOIN-COUNT-FAILED event="+ev.ID, "error", errorClass(err))
		b.sockets.Retire(nonce)
		return
	}
	ev.Joins = joins
	socketURL := fmt.Sprintf("wss://localhost:%d%s%s", b.s.AudioPort, audioSocketPath, socketSecret(b.s.BridgeKey, nonce))
	webhookURL := fmt.Sprintf("http://localhost:%d%s", b.s.WebhookPort, webhookPath)
	cctx, cancel = context.WithTimeout(ctx, callLimit)
	bt, err := b.api.CreateBot(cctx, botSettingsFor(*ev, *texts, nonce, socketURL, webhookURL, now))
	cancel()
	if err != nil {
		b.log.Warn(fmt.Sprintf("BOT-CREATE-FAILED event=%s joins=%d", ev.ID, joins), "error", errorClass(err))
		b.attempt = &createAttempt{event: *ev, nonce: nonce, joins: joins, at: now}
		return
	}
	lb := newLiveBot(bt.ID, nonce, ev, now)
	lb.created, lb.state = now, bt.State
	b.bot = lb
	b.log.Info(fmt.Sprintf("BOT-CREATED bot=%s event=%s joins=%d", bt.ID, ev.ID, joins))
	b.sendCall(ev, botJoining)
}
