package main

import (
	"context"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
)

// What the bridge owes for an event lives on the event's record, its private
// property ghostWisprPending (calendar.Pending), so a restart of the bridge
// does not forget it: the reason it asked its bot to leave, which decides
// the bot's end by the table of ends, and each busy or failed chat_check a
// link has not kept, from which the Pi publishes call_not_recorded (R2,
// R14).

const (
	// recordLimit bounds the write of a leave's reason, which the loop
	// waits for before it asks Attendee for the leave.
	recordLimit = 2 * time.Second
	// leaveRecordWait is how long a leave waits for its reason to be on
	// record, its write tried again at each tick, before the bridge leaves
	// without it: a lost Pi's 2 minutes and a failed socket's 300 s are the
	// spec's bounds. A leave after a failed chat check waits only while R2's
	// 10 s leave room for the leave's own call.
	leaveRecordWait = 10 * time.Second
	// confirmAfter is how long a link stays up after it took an owed
	// control before the bridge counts the control received. A link's send
	// queue takes a control before its writer writes it (call.SendQueue),
	// so a link that drops first may lose it; a side counts a dead link down
	// after 750 ms with no message (spec, Limits, "Keepalive"), and the
	// bridge checks once a tick. A control whose link went down sooner is
	// sent again on the next link, and the Pi drops a repeat by its event.
	confirmAfter = 2 * time.Second
)

// eventRecord is the bridge's ghostWisprPending of one event: the record it
// wants the calendar to hold, and the one the calendar holds as far as the
// bridge knows.
type eventRecord struct{ want, held calendar.Pending }

// owesMore reports whether the wanted record owes a control the held one
// lacks: it goes on record before the event's done mark.
func (r *eventRecord) owesMore() bool {
	return (r.want.Busy && !r.held.Busy) || (r.want.ChatCheckFailed && !r.held.ChatCheckFailed)
}

// record gives the event's record; a new one owes nothing.
func (b *bridge) record(id string) *eventRecord {
	r := b.records[id]
	if r == nil {
		r = &eventRecord{}
		b.records[id] = r
	}
	return r
}

// writeRecord writes, beside the loop, what the event's record still lacks:
// first a control it owes, then the done mark, then the rest of its pending
// record. One write of an event's record runs at a time, as each sends the
// event's whole map of private properties and must not undo another's key.
// A failed write is tried again at each pass.
func (b *bridge) writeRecord(ctx context.Context, id string) {
	if b.writing[id] {
		return
	}
	r := b.records[id]
	switch {
	case r != nil && r.owesMore():
		b.writePending(ctx, id, r.want)
	case b.markOwed[id]:
		b.writeDone(ctx, id)
	case r != nil && r.want != r.held:
		b.writePending(ctx, id, r.want)
	}
}

func (b *bridge) writePending(ctx context.Context, id string, p calendar.Pending) {
	b.writing[id] = true
	cal := b.cal
	b.beside(ctx, callLimit, func(cctx context.Context) func() {
		err := cal.SetPending(cctx, id, p)
		return func() {
			delete(b.writing, id)
			// An event the calendar no longer has keeps no record: nothing
			// is left to write for it.
			if err != nil && !eventGone(err) {
				b.log.Warn("PENDING-WRITE-FAILED event="+id, "error", errorClass(err))
				return
			}
			b.record(id).held = p
			b.recordWritten(ctx, id)
		}
	})
}

// writeDone writes the event's done mark. Once it lands, or the calendar no
// longer has the event, the delete_data that waited for it runs, and the
// last bot's leave reason and passed check, which decide nothing more, are
// cleared.
func (b *bridge) writeDone(ctx context.Context, id string) {
	b.writing[id] = true
	cal := b.cal
	b.beside(ctx, callLimit, func(cctx context.Context) func() {
		err := cal.MarkDone(cctx, id)
		return func() {
			delete(b.writing, id)
			switch {
			case eventGone(err):
				b.log.Info("MARK-DONE-GONE event=" + id)
			case err != nil:
				b.log.Warn("MARK-DONE-FAILED event="+id, "error", errorClass(err))
				return
			}
			delete(b.markOwed, id)
			for _, d := range b.deleteOwed[id] {
				b.deleteData(ctx, d.bot)
			}
			delete(b.deleteOwed, id)
			b.clearBot(id)
			b.recordWritten(ctx, id)
		}
	})
}

// recordWritten follows a write of the event's record that landed: a leave
// that waited for its reason goes, the record's next write starts, and a
// pass that waited for the write runs.
func (b *bridge) recordWritten(ctx context.Context, id string) {
	if r := b.records[id]; r != nil && r.want == r.held && r.held == (calendar.Pending{}) {
		delete(b.records, id)
	}
	if lb := b.bot; lb != nil && lb.event.ID == id && lb.leaveKind != 0 && !lb.leaving {
		b.leave(ctx, lb.leaveKind)
	}
	b.writeRecord(ctx, id)
	if b.passAfterWrite {
		b.passAfterWrite = false
		b.joinPass(ctx)
	}
}

// clearBot takes the event's last bot off its wanted record, its leave
// reason and its passed check, once the end that bot's reason decides has
// taken effect: the event's done mark landed, or its next bot was made. It
// reports whether the record changed; the caller writes it.
func (b *bridge) clearBot(id string) bool {
	r := b.records[id]
	if r == nil || (r.want.Leave == 0 && r.want.Passed == "") {
		return false
	}
	r.want.Leave, r.want.Bot, r.want.Passed, r.want.Sender = 0, "", "", ""
	return true
}

// recordLeave puts lb's leave reason, with what else the record owes, on its
// event's record before the bridge asks Attendee for the leave, writing on
// the loop within recordLimit and before until. It reports whether the
// reason is on record; while a write of the record runs beside the loop it
// writes nothing, and that write's end asks for the leave again.
func (b *bridge) recordLeave(ctx context.Context, lb *liveBot, until time.Time) bool {
	id := lb.event.ID
	r := b.record(id)
	r.want.Leave, r.want.Bot = lb.leaveKind, lb.id
	if r.want == r.held {
		return true
	}
	limit := min(recordLimit, until.Sub(b.clock.Now()))
	if b.writing[id] || limit <= 0 {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	err := b.cal.SetPending(cctx, id, r.want)
	cancel()
	if err != nil && !eventGone(err) {
		b.log.Warn("PENDING-WRITE-FAILED event="+id, "error", errorClass(err))
		return false
	}
	r.held = r.want
	return true
}

// restoreRecord takes an event's pending record at the bridge's start. Each
// control it owes waits for the link, after the bridge's hello, as one no
// link has kept. An event that owes one is done, as a busy and a failed chat
// check each end it, so a pass does not find its busy again; and a done
// event's last bot, which decides nothing more, is cleared.
func (b *bridge) restoreRecord(ctx context.Context, e *calendar.Event) {
	p := e.Pending
	if p == (calendar.Pending{}) {
		return
	}
	b.records[e.ID] = &eventRecord{want: p, held: p}
	if p.Busy {
		b.kept = append(b.kept, busyControl(e))
	}
	if p.ChatCheckFailed {
		b.kept = append(b.kept, chatCheckControl(checkResultFailed, e.ID))
	}
	switch {
	case p.Owes() && !e.Done:
		b.markDone(ctx, e.ID)
	case e.Done && b.clearBot(e.ID):
		b.writeRecord(ctx, e.ID)
	}
}

// restoreBot gives a bot a restarted bridge found what its event's record
// names for it. A passed chat check: the check stays passed, with its echo's
// sender, and no deadline applies to it. A leave: the bridge asked the bot
// to leave before the restart, for that reason, so the bot's end is decided
// by it, the leave is asked again until Attendee takes it, and a failed chat
// check is not judged again.
func (b *bridge) restoreBot(lb *liveBot) {
	r := b.records[lb.event.ID]
	if r == nil {
		return
	}
	if r.held.Passed == lb.id {
		lb.check.state, lb.check.sender = checkPassed, r.held.Sender
	}
	if r.held.Leave == 0 || r.held.Bot != lb.id {
		return
	}
	lb.leaveKind, lb.leaveAsked = r.held.Leave, b.clock.Now()
	if lb.leaveKind == calendar.EndChatCheckFailed {
		lb.check.state = checkFailed
	}
}

// The chat check's results on the link (C2's chat_check).
const (
	checkResultPassed = "passed"
	checkResultFailed = "failed"
)

func busyControl(e *calendar.Event) call.Control {
	return call.Control{Type: call.TypeBusy, Busy: &call.Busy{Event: callEvent(e)}}
}

func chatCheckControl(result, eventID string) call.Control {
	return call.Control{Type: call.TypeChatCheck, ChatCheck: &call.ChatCheck{Result: result, Event: eventID}}
}

// owed gives the event of a control that the event's record keeps until a
// link kept it, and the record that owes it alone: a busy, or a failed
// chat_check.
func owed(c *call.Control) (string, calendar.Pending, bool) {
	switch {
	case c.Type == call.TypeBusy && c.Busy != nil:
		return c.Busy.Event.ID, calendar.Pending{Busy: true}, true
	case c.Type == call.TypeChatCheck && c.ChatCheck != nil && c.ChatCheck.Result == checkResultFailed:
		return c.ChatCheck.Event, calendar.Pending{ChatCheckFailed: true}, true
	}
	return "", calendar.Pending{}, false
}

// sentOwed is an owed control a link took at, which its event's record owes
// until the link has stayed up confirmAfter since.
type sentOwed struct {
	c  call.Control
	at time.Time
}

// keep sends a control message that must reach the Pi, a busy or a
// chat_check, now or once the link is ready again: the Pi publishes
// call_not_recorded from a busy or a failed check. Either of those is owed
// on its event's record until a link kept it (confirmSent), and the caller
// writes the record: markBusy with the done mark, a failed check with its
// leave.
func (b *bridge) keep(c *call.Control) {
	if id, o, ok := owed(c); ok {
		r := b.record(id)
		r.want.Busy = r.want.Busy || o.Busy
		r.want.ChatCheckFailed = r.want.ChatCheckFailed || o.ChatCheckFailed
	}
	b.kept = append(b.kept, *c)
	b.flushKept()
}

// flushKept sends the kept messages the link has not taken yet, in order.
// The link took each owed one at now; confirmSent counts it received.
func (b *bridge) flushKept() {
	now := b.clock.Now()
	for len(b.kept) > 0 && b.linkReady() {
		c := b.kept[0]
		if err := b.link.Send(c); err != nil {
			b.log.Warn("LINK-SEND-FAILED type="+c.Type, "error", errorClass(err))
			return
		}
		b.kept = b.kept[1:]
		if _, _, ok := owed(&c); ok {
			b.sent = append(b.sent, sentOwed{c: c, at: now})
		}
	}
}

// confirmSent counts each owed control received that a link took
// confirmAfter ago and has kept up since, and takes it off its event's
// record. While the link is not ready, each goes back to the kept messages,
// sent again on the next link after the bridge's hello.
func (b *bridge) confirmSent(ctx context.Context, now time.Time) {
	if !b.linkReady() {
		b.resendSent()
		return
	}
	waiting := b.sent[:0]
	for i := range b.sent {
		s := &b.sent[i]
		if now.Sub(s.at) < confirmAfter {
			waiting = append(waiting, *s)
			continue
		}
		id, o, _ := owed(&s.c)
		if r := b.records[id]; r != nil {
			r.want.Busy = r.want.Busy && !o.Busy
			r.want.ChatCheckFailed = r.want.ChatCheckFailed && !o.ChatCheckFailed
			b.writeRecord(ctx, id)
		}
	}
	b.sent = waiting
}

// resendSent puts the owed controls a link took and has not kept long
// enough back before the kept messages, in their order: that link may have
// lost them. Each link-up's hello ends the link before it.
func (b *bridge) resendSent() {
	if len(b.sent) == 0 {
		return
	}
	kept := make([]call.Control, 0, len(b.sent)+len(b.kept))
	for i := range b.sent {
		kept = append(kept, b.sent[i].c)
	}
	b.kept, b.sent = append(kept, b.kept...), nil
}
