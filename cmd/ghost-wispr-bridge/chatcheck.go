package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
	"github.com/sjawhar/ghost-wispr/internal/call"
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
// texts give the notice, and the roster and the lines are read, and fails the
// check only then: without the texts no bot was made, or a restarted bridge
// waits for them.
func (b *bridge) judgeCheck(lb *liveBot, now time.Time) bool {
	if lb.check.state != checkPending || lb.admitted.IsZero() || lb.needRoster || lb.needChat {
		return false
	}
	texts := b.texts()
	if texts == nil {
		return false
	}
	deadline := lb.admitted.Add(chatCheckWindow)
	for _, l := range lb.check.lines {
		if l.text == texts.Consent && l.sender != "" && !lb.seen[l.sender] &&
			!lb.echoFrom.IsZero() && !l.at.Before(lb.echoFrom) && !l.at.After(deadline) {
			lb.check.state, lb.check.sender, lb.check.lines = checkPassed, l.sender, nil
			return true
		}
	}
	if now.Before(deadline) {
		return false
	}
	lb.check.state, lb.check.lines = checkFailed, nil
	return true
}

// evalCheck decides a pending check and acts on the result.
func (b *bridge) evalCheck(ctx context.Context, lb *liveBot, now time.Time) {
	if b.judgeCheck(lb, now) {
		b.announceCheck(ctx, lb)
	}
}

// announceCheck acts on a decided check: the log line, the link's chat_check
// (kept until the link takes it, so the Pi can publish call_not_recorded) and
// call messages, and on a failure the leave (R2). A passed check under
// long_lines posts the test lines.
func (b *bridge) announceCheck(ctx context.Context, lb *liveBot) {
	result := "passed"
	if lb.check.state == checkFailed {
		result = "failed"
	}
	b.log.Info(fmt.Sprintf("CHAT-CHECK result=%s bot=%s", result, lb.check.sender))
	b.keep(&call.Control{Type: call.TypeChatCheck, ChatCheck: &call.ChatCheck{Result: result, Event: lb.event.ID}})
	if lb.check.state == checkFailed {
		b.leave(ctx, calendar.EndChatCheckFailed)
		return
	}
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
