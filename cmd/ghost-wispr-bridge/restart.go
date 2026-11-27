package main

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
)

// restore rebuilds the bridge's state from Attendee and the calendar at its
// start, as it keeps none on disk (spec, "A restart of the bridge"): each
// bot's event and socket secret, the end of an event whose newest bot ended,
// and for a bot in a call its roster and whether its chat check passed. Each
// event's pending record gives the controls the Pi has not received, sent
// once the link is up, and why the bridge asked its bot to leave, which
// decides that bot's end (pending.go). It posts no notice before the Pi's
// texts. On a failed read it tries again after joinEvery, and makes no bot
// before.
func (b *bridge) restore(ctx context.Context) {
	b.lastRestore = b.clock.Now()
	cctx, cancel := context.WithTimeout(ctx, callLimit)
	defer cancel()
	bots, err := b.api.Bots(cctx)
	if err != nil {
		b.log.Warn("RESTART-READ-FAILED source=attendee", "error", errorClass(err))
		return
	}
	now := b.clock.Now()
	evs, err := b.cal.Events(cctx, now.Add(-calendar.PoolHold), now.Add(calendar.PoolLead))
	if err != nil {
		b.log.Warn("RESTART-READ-FAILED source=calendar", "error", errorClass(err))
		return
	}
	b.restored = true
	events := make(map[string]calendar.Event, len(evs))
	for i := range evs {
		events[evs[i].ID] = evs[i]
		b.restoreRecord(ctx, &evs[i])
	}
	// Attendee lists the bots oldest first: an event's newest bot is its
	// last.
	newest := map[string]int{}
	for i := range bots {
		newest[bots[i].EventID] = i
	}
	held := map[string]bool{} // events with a bot in Attendee's list
	var current *bot          // the record of b.bot
	acted := 0                // the bots this restart acts on, each with its RESTART-BOT line
	for i := range bots {
		bt := &bots[i]
		if bt.EventID == "" {
			continue // not a bot of ours
		}
		held[bt.EventID] = true
		ev, found := events[bt.EventID]
		if !found {
			ev = calendar.Event{ID: bt.EventID} // no longer a call's event: its end has passed, or it is gone
		}
		// The newest bot of an event in the calendar decides the event, also
		// when upkeep deleted its data: Attendee keeps its events. An older
		// bot's end was decided when the newer bot was made. A data_deleted
		// bot that decides nothing needs nothing more, and gets no line:
		// Attendee keeps every bot of the day until the instance stops.
		superseded := newest[bt.EventID] != i || !found || ev.Done
		if superseded && bt.State == stateDataDeleted {
			continue
		}
		acted++
		lb := newLiveBot(bt.ID, bt.Nonce, &ev, now)
		lb.adopted, lb.state, lb.leaving = true, bt.State, leavingState(bt.State)
		b.restoreLeave(lb)
		// A bot whose record holds no event yet (one Attendee scheduled for
		// its join_at) counts from the bridge's start, the first time this
		// bridge knows.
		lb.created = b.startedAt
		if len(bt.Events) > 0 && !bt.Events[0].At.IsZero() {
			lb.created = bt.Events[0].At
		}
		b.log.Info("RESTART-BOT bot=" + bt.ID + " event=" + bt.EventID + " state=" + bt.State)
		if finalState(bt.State) {
			lb.superseded = superseded
			b.others[bt.ID] = lb
			b.endBot(ctx, lb, bt)
			continue
		}
		switch {
		case b.bot == nil:
			b.bot, current = lb, bt
		case !joinedState(b.bot.state) && joinedState(bt.State):
			b.addExtra(ctx, b.bot)
			b.bot, current = lb, bt
		default:
			b.addExtra(ctx, lb)
		}
	}
	b.log.Info(fmt.Sprintf("RESTART-COUNT bots=%d acted=%d", len(bots), acted))
	if lb := b.bot; lb != nil {
		if lb.nonce != "" {
			b.sockets.Register(lb.nonce)
		}
		if joinedState(lb.state) {
			b.adoptInCall(ctx, lb, current, now)
		}
	}
	// A2: an event in progress whose bot went with the instance gets a new
	// bot through the pass, or at its join limit is done, so the pool can
	// stop. A leave the bridge had asked of that bot still decides by its
	// reason on record.
	for i := range evs {
		e := &evs[i]
		if e.Done || b.done[e.ID] || e.Joins == 0 || held[e.ID] || now.Before(e.Start) || !now.Before(e.End) {
			continue
		}
		kind := calendar.EndInstanceRestarted
		if r := b.records[e.ID]; r != nil && r.held.Leave != 0 {
			kind = r.held.Leave
		}
		if calendar.EndResult(kind, *e, now) == calendar.Done {
			b.log.Info("BOT-END event=" + e.ID + " end=" + endName(kind) + " result=done")
			b.markDone(ctx, e.ID)
		}
	}
}

// leavingState: Attendee took a leave of the bot, which it is carrying out.
func leavingState(s string) bool { return s == "leaving" || s == "post_processing" }

// addExtra holds a second live bot that a restarted bridge found: one call
// at a time, so it leaves, and its socket secret is not taken (PC14).
func (b *bridge) addExtra(ctx context.Context, lb *liveBot) {
	lb.extra = true
	b.others[lb.id] = lb
	if !lb.leaving {
		b.departOther(ctx, lb)
	}
}

// adoptInCall rebuilds a joined bot's call: its admission from its record,
// then its roster from Attendee's join and leave events (A3) and its chat
// lines for the check, read beside the loop until each read succeeds.
func (b *bridge) adoptInCall(ctx context.Context, lb *liveBot, bt *bot, now time.Time) {
	lb.admitted = admission(bt.Events, now)
	lb.lastSocket = now
	// Chat times have whole seconds: the echo of the notice posted after the
	// admission may carry the admission's second.
	lb.echoFrom = lb.admitted.Truncate(time.Second)
	lb.needRoster, lb.needChat = true, true
	b.readAdopted(ctx, lb)
}

// readAdopted reads, beside the loop, the roster and the chat lines that a
// found bot's call still needs. A failed read is tried again at the next
// tick; until both are read, the check is not judged and the leave rule does
// not run (A3, R2).
func (b *bridge) readAdopted(ctx context.Context, lb *liveBot) {
	api, id := b.api, lb.id
	if lb.needRoster && !lb.rosterRead {
		lb.rosterRead = true
		b.beside(ctx, callLimit, func(cctx context.Context) func() {
			ps, err := api.ParticipantEvents(cctx, id)
			return func() {
				lb.rosterRead = false
				if lb != b.bot || !lb.needRoster {
					return
				}
				if err != nil {
					b.log.Warn("ROSTER-READ-FAILED bot="+lb.id, "error", errorClass(err))
					return
				}
				lb.rebuildRoster(ps)
				b.evalCheck(ctx, lb, b.clock.Now())
			}
		})
	}
	if lb.needChat && !lb.chatRead {
		lb.chatRead = true
		b.beside(ctx, callLimit, func(cctx context.Context) func() {
			msgs, err := api.ChatMessages(cctx, id)
			return func() {
				lb.chatRead = false
				if lb != b.bot || !lb.needChat {
					return
				}
				if err != nil {
					b.log.Warn("CHAT-READ-FAILED bot="+lb.id, "error", errorClass(err))
					return
				}
				for _, m := range msgs {
					lb.noteLine(m, m.At)
				}
				lb.needChat = false
				b.evalCheck(ctx, lb, b.clock.Now())
			}
		})
	}
}

// participantKey names one join or leave, so the roster read and a webhook
// of the same event count once.
type participantKey struct {
	id   string
	join bool
	ms   int64
}

// rebuildRoster takes the roster read from Attendee, oldest first, then the
// joins and leaves that came by webhook while the read ran and that it does
// not hold.
func (lb *liveBot) rebuildRoster(read []participantEvent) {
	clear(lb.roster)
	inRead := make(map[participantKey]bool, len(read))
	for _, p := range read {
		lb.takeParticipant(p)
		inRead[participantKey{p.ID, p.Join, p.At.UnixMilli()}] = true
	}
	for _, p := range lb.liveJoins {
		if !inRead[participantKey{p.ID, p.Join, p.At.UnixMilli()}] {
			lb.takeParticipant(p)
		}
	}
	lb.liveJoins, lb.needRoster = nil, false
}
