package main

import "time"

// noticeWindow is R3's window: at most one consent notice in each 10 s, and
// one more at the end of a window that had a join.
const noticeWindow = 10 * time.Second

// consentNotices is the consent notice's window for one bot. The other
// notices post at once and do not count (spec, Limits, "A notice"). Only a
// post that Attendee took moves the window.
type consentNotices struct {
	last    time.Time // the last consent notice Attendee took; zero before the first
	pending time.Time // when one more notice is due: a window's end that had a join, or a failed post's retry; zero when none
}

// join takes a join timed at, seen at now, and reports whether the notice
// posts at once. A join timed at or before the last notice needs none: that
// notice came after it, which is what R3 asks for each person. The people in
// the call at the bot's admission are such joins when the notice went first:
// Attendee times them when its worker reads Meet's first users list
// (bots/web_bot_adapter/web_bot_adapter.py:243-245,516-519). A later join
// posts at once when none posted in the last 10 s; otherwise one more posts
// at the end of those 10 s.
func (n *consentNotices) join(at, now time.Time) bool {
	if !n.last.IsZero() && !at.After(n.last) {
		return false
	}
	if n.last.IsZero() || now.Sub(n.last) >= noticeWindow {
		return true
	}
	if n.pending.IsZero() {
		n.pending = n.last.Add(noticeWindow)
	}
	return false
}

// due reports whether the one more notice posts now.
func (n *consentNotices) due(now time.Time) bool {
	return !n.pending.IsZero() && !now.Before(n.pending)
}

// posted takes a notice that Attendee took at at.
func (n *consentNotices) posted(at time.Time) {
	n.last, n.pending = at, time.Time{}
}

// failed keeps a notice that Attendee did not take due again at retry.
func (n *consentNotices) failed(retry time.Time) {
	n.pending = retry
}
