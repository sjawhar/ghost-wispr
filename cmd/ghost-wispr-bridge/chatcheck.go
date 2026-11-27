package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
)

// chatCheckWindow is R2's time for the consent notice's echo, from the
// admission, and chatLeaveWithin the time for the leave after a failed check.
const (
	chatCheckWindow = 30 * time.Second
	chatLeaveWithin = 10 * time.Second
)

type checkState int

const (
	checkPending checkState = iota
	checkPassed
	checkFailed
)

// chatCheck is R2's check of one bot: the consent notice's exact text back in
// the chat within 30 s of the admission, from a sender not in the roster.
type chatCheck struct {
	state  checkState
	sender string     // the echo's Meet sender id: the bot's own id once passed
	lines  []chatLine // the lines seen while the check is pending; dropped once it is decided
}

// chatLine is a chat line as the check reads it.
type chatLine struct {
	id, sender, text string
	at               time.Time // its arrival on the bridge's clock; a restarted bridge's read gives its chat time
}

// noteLine keeps a chat line for the pending check, timed at; a line it holds
// already, by webhook and by a restart's read, counts once.
func (lb *liveBot) noteLine(m chatMessage, at time.Time) {
	if m.ID != "" {
		for _, l := range lb.check.lines {
			if l.id == m.ID {
				return
			}
		}
	}
	lb.check.lines = append(lb.check.lines, chatLine{id: m.ID, sender: m.SenderID, text: m.Text, at: at})
}

// judgeCheck decides a pending check, and reports whether it did. The echo is
// a line of the notice's exact text from a sender never in the roster, timed
// from the first consent post (or, for a bot a restarted bridge found, its
// admission) to 30 s after the admission. It reads the lines once the Pi's
// texts give the notice: without the texts no bot was made, or a restarted
// bridge waits for them. A roster or chat lines a restarted bridge has not
// read yet stop a pass but not the deadline (checkDeadline): a check that
// has not passed by then fails (R2).
func (b *bridge) judgeCheck(lb *liveBot, now time.Time) bool {
	if lb.check.state != checkPending || lb.admitted.IsZero() {
		return false
	}
	texts := b.texts()
	if texts == nil {
		return false
	}
	if window := lb.admitted.Add(chatCheckWindow); !lb.needRoster && !lb.needChat {
		for _, l := range lb.check.lines {
			if l.text == texts.Consent && l.sender != "" && !lb.seen[l.sender] &&
				!lb.echoFrom.IsZero() && !l.at.Before(lb.echoFrom) && !l.at.After(window) {
				lb.check.state, lb.check.sender, lb.check.lines = checkPassed, l.sender, nil
				return true
			}
		}
	}
	if now.Before(lb.checkDeadline()) {
		return false
	}
	lb.check.state, lb.check.lines = checkFailed, nil
	return true
}

// checkDeadline is when a check that has not passed fails: 30 s after the
// admission, and for a bot a restarted bridge found in a call no sooner than
// one call's bound after the restart's reads began, so the reads can still
// show a check that passed before the restart.
func (lb *liveBot) checkDeadline() time.Time {
	d := lb.admitted.Add(chatCheckWindow)
	if reads := lb.readsFrom.Add(callLimit); !lb.readsFrom.IsZero() && reads.After(d) {
		return reads
	}
	return d
}

// evalCheck decides a pending check and acts on the result.
func (b *bridge) evalCheck(ctx context.Context, lb *liveBot, now time.Time) {
	if b.judgeCheck(lb, now) {
		b.announceCheck(ctx, lb)
	}
}

// announceCheck acts on a decided check: the log line, the link's chat_check
// (kept until the link takes it, so the Pi can publish call_not_recorded) and
// call messages, and on a failure the leave (R2), whose record holds the
// failed chat_check until a link keeps it. A passed check goes on the
// event's record with its bot and the echo's sender, so a restart applies
// that bot no deadline. A passed check under long_lines posts the test
// lines.
func (b *bridge) announceCheck(ctx context.Context, lb *liveBot) {
	result := checkResultPassed
	if lb.check.state == checkFailed {
		result = checkResultFailed
	}
	b.log.Info(fmt.Sprintf("CHAT-CHECK result=%s bot=%s", result, lb.check.sender))
	check := chatCheckControl(result, lb.event.ID)
	b.keep(&check)
	if lb.check.state == checkFailed {
		b.leave(ctx, calendar.EndChatCheckFailed)
		b.writeRecord(ctx, lb.event.ID) // the owed check, when the leave has not written it
		return
	}
	r := b.record(lb.event.ID)
	r.want.Passed, r.want.Sender = lb.id, lb.check.sender
	b.writeRecord(ctx, lb.event.ID)
	b.sendCall(&lb.event, botInCall)
	if b.faults.LongLines && !lb.adopted {
		for _, n := range longLineLengths {
			b.post(ctx, lb, longLine(n), fmt.Sprintf("long_line_%d", n))
		}
	}
}

// longLineLengths are long_lines' lengths: below, at and above Meet's
// 500-character limit (spec, Limits, "A notice").
var longLineLengths = []int{400, 499, 500, 501, 2000}

// longLine is a test line of exactly n characters.
func longLine(n int) string {
	head := fmt.Sprintf("Ghost Wispr long line test, %d characters: ", n)
	return head + strings.Repeat("x", n-len(head))
}
