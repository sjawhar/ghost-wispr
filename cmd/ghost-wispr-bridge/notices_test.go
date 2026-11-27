package main

import (
	"slices"
	"testing"
	"time"
)

// noticeTimes gives the seconds after admitted of each consent notice the
// bridge posted.
func noticeTimes(h *botHarness, admitted time.Time) []int {
	var out []int
	for _, c := range h.api.callsOf("Chat") {
		if c.Text == botTexts().Consent {
			out = append(out, int(c.At.Sub(admitted)/time.Second))
		}
	}
	return out
}

// R3: at the bot's join and after each later join; at once with none in the
// last 10 s, else one more at the end of those 10 s.
func TestNoticesFollowTheJoinWindow(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	admitted := h.clk.Now()
	h.chat(id, "bot-device", botTexts().Consent)
	h.advance(3 * time.Second)
	h.join(id, "p1", "Person One") // 3 s: within the window of 0 s
	h.advanceTo(admitted.Add(25 * time.Second))
	h.join(id, "p2", "Person Two") // 25 s: none since 10 s
	h.advance(2 * time.Second)
	h.join(id, "p3", "Person Three") // 27 s: within the window of 25 s
	h.advance(2 * time.Second)
	h.join(id, "p4", "Person Four") // 29 s: the same window
	h.advanceTo(admitted.Add(60 * time.Second))
	if got, want := noticeTimes(h, admitted), []int{0, 10, 25, 35}; !slices.Equal(got, want) {
		t.Fatalf("consent notices at %v s after the admission, want %v", got, want)
	}
}

func TestNoticeJoinOnlyPostsOnlyAtTheBotsJoin(t *testing.T) {
	h, id := joinOneBotWith(t, faultSet{NoticeJoinOnly: true})
	h.admit(id)
	admitted := h.clk.Now()
	h.chat(id, "bot-device", botTexts().Consent)
	h.advance(15 * time.Second)
	h.join(id, "p1", "Person One")
	h.advance(30 * time.Second)
	if got := noticeTimes(h, admitted); !slices.Equal(got, []int{0}) {
		t.Fatalf("consent notices with notice_join_only at %v s, want [0]", got)
	}
}

// R3 asks that each person who joins sees a notice that came after their
// join. The people already in the call when the bot is admitted get their
// joins from Attendee's first UsersUpdate, timed when the worker processes
// it (bots/web_bot_adapter/web_bot_adapter.py:243-245,516-519), so a join
// timed at or before the last consent notice needs no new notice; a later
// join gets its own.
func TestPeoplePresentAtTheAdmissionGetOneNotice(t *testing.T) {
	h, id := joinOneBot(t)
	h.admit(id)
	admitted := h.clk.Now()
	h.chat(id, "bot-device", botTexts().Consent)
	h.advance(2 * time.Second) // the webhooks of the first UsersUpdate come after the notice
	h.participant(id, participantEvent{ID: "p1", Name: "Person One", Join: true, At: admitted.Add(-time.Second)})
	h.participant(id, participantEvent{ID: "p2", Name: "Person Two", Join: true, At: admitted})
	h.advanceTo(admitted.Add(15 * time.Second))
	if got := noticeTimes(h, admitted); !slices.Equal(got, []int{0}) {
		t.Fatalf("consent notices with two people present at the admission at %v s, want [0]", got)
	}
	h.join(id, "p3", "Person Three") // 15 s: a join after the notice
	h.advance(30 * time.Second)
	if got := noticeTimes(h, admitted); !slices.Equal(got, []int{0, 15}) {
		t.Fatalf("consent notices after a later join at %v s, want [0 15]", got)
	}
}

// R3: a consent notice that Attendee did not post stays due, and the bridge
// posts it at the next second: the notice of the admission and the one more
// at a window's end alike. Only a post that Attendee took moves the window.
func TestAFailedConsentPostStaysPending(t *testing.T) {
	t.Run("the notice of the admission", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.api.fail(func(a *fakeAttendee) { a.chatPostFails = 1 })
		h.admit(id)
		admitted := h.clk.Now()
		h.advance(2 * time.Second)
		if got := noticeTimes(h, admitted); !slices.Equal(got, []int{1}) {
			t.Fatalf("consent notices after a failed post at the admission at %v s, want [1]", got)
		}
		h.chat(id, "bot-device", botTexts().Consent)
		h.advance(40 * time.Second)
		if got := noticeTimes(h, admitted); !slices.Equal(got, []int{1}) {
			t.Fatalf("consent notices at %v s, want only the retried one at 1 s", got)
		}
		if n := len(h.api.callsOf("Leave")); n != 0 {
			t.Fatalf("the bot left %d times after the echo of the retried notice", n)
		}
	})
	t.Run("the notice at a window's end", func(t *testing.T) {
		h, id := joinOneBot(t)
		h.admit(id)
		admitted := h.clk.Now()
		h.chat(id, "bot-device", botTexts().Consent)
		h.advance(3 * time.Second)
		h.join(id, "p1", "Person One") // 3 s: one more at the end of the window of 0 s
		h.api.fail(func(a *fakeAttendee) { a.chatPostFails = 1 })
		h.advanceTo(admitted.Add(15 * time.Second))
		if got := noticeTimes(h, admitted); !slices.Equal(got, []int{0, 11}) {
			t.Fatalf("consent notices after a failed post at 10 s at %v s, want [0 11]", got)
		}
		h.advanceTo(admitted.Add(40 * time.Second))
		if got := noticeTimes(h, admitted); !slices.Equal(got, []int{0, 11}) {
			t.Fatalf("consent notices at %v s, want [0 11] and no more", got)
		}
	})
}
