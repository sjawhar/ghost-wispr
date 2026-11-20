package voice

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests here are the Supervisor's side of joined agents (R17, R10): a
// raised hand plays the hand sound, speaks only on a person's invitation, and
// drops when it expires; "Wispr, who has a hand up?" says the agents' keys;
// and the room's final segments reach the joined agents' roster.

// handOutcomes records the HandDone calls.
type handOutcomes struct {
	mu   sync.Mutex
	done []string // hand id:outcome
}

func (h *handOutcomes) record(hand *Hand, outcome HandOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = append(h.done, hand.ID+":"+outcome.String())
}

func (h *handOutcomes) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.done)
}

// newHandsRig is a rig whose Supervisor reports each hand's outcome.
func newHandsRig(t *testing.T) (*rig, *handOutcomes) {
	t.Helper()
	outcomes := &handOutcomes{}
	r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, _ *rig) { cfg.HandDone = outcomes.record })
	return r, outcomes
}

func testHand(id, key string, expires time.Duration) *Hand {
	return &Hand{
		ID: id, SessionID: "sess-1", Agent: "agent-" + id, Membership: "m-" + id, IssueKey: key, Name: key,
		Text: key + "'s agent says: the deploy moved to Friday.", Expires: time.Now().Add(expires),
	}
}

// TestALowerTakesOnlyItsMembershipsHands fails if lowering one membership's
// hands takes down a hand of another membership of the same agent session,
// as a late leave of the agent's earlier membership would.
func TestALowerTakesOnlyItsMembershipsHands(t *testing.T) {
	r, outcomes := newHandsRig(t)
	earlier, later := testHand("h1", "ABC-12", time.Minute), testHand("h2", "ABC-12", time.Minute)
	later.Agent = earlier.Agent // one Envoy session, two memberships
	for _, h := range []*Hand{earlier, later} {
		if err := r.s.RaiseHand(h); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
	}
	r.s.LowerHands(earlier.Membership)
	eventually(t, "the earlier membership's hand withdrawn", func() bool { return slices.Equal(outcomes.all(), []string{"h1:withdrawn"}) })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != later.Text {
		t.Fatalf("said %q, want the later membership's words %q", got, later.Text)
	}
}

// TestAHandWhoseMembershipEndedIsWithdrawn fails if the Supervisor speaks, or
// names on "Wispr, who has a hand up?", a hand whose membership is no longer
// current, whether it ended before the invitation or between the invitation
// and the words; or if its agent is not told it was withdrawn. The Supervisor
// asks the hand (Current) at the moment of each effect.
func TestAHandWhoseMembershipEndedIsWithdrawn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// endAt is when the membership ends: before the go-ahead, or as the
		// voice opens to say the words, after the invitation was accepted.
		endAt string
	}{{"before the invitation", "before"}, {"between the invitation and the words", "open"}} {
		t.Run(tc.name, func(t *testing.T) {
			r, outcomes := newHandsRig(t)
			var ended atomic.Bool
			var asked atomic.Int32
			hand := testHand("h1", "ABC-12", time.Minute)
			hand.Current = func() bool { asked.Add(1); return !ended.Load() }
			if err := r.s.RaiseHand(hand); err != nil {
				t.Fatalf("RaiseHand: %v", err)
			}
			eventually(t, "the hand sound", func() bool { return slices.Equal(r.speaker.soundsPlayed(), []string{"hand"}) })
			switch tc.endAt {
			case "before":
				ended.Store(true)
				r.heard("Wispr, who has a hand up?", true, true)
				if got, want := waitSaid(t, r.waitSession()), "Nobody has a hand up."; got != want {
					t.Fatalf("said %q, want %q: an ended membership's hand was named", got, want)
				}
				r.speaker.finish("voice")
				time.Sleep(100 * time.Millisecond)
			case "open":
				r.voice.mu.Lock()
				r.voice.onOpen = func() { ended.Store(true) }
				r.voice.mu.Unlock()
			}
			r.heard("Wispr, go ahead.", true, true)
			eventually(t, "the hand reported withdrawn", func() bool { return slices.Equal(outcomes.all(), []string{"h1:withdrawn"}) })
			r.voice.mu.Lock()
			sessions := slices.Clone(r.voice.sessions)
			r.voice.mu.Unlock()
			for _, s := range sessions {
				s.mu.Lock()
				says := slices.Clone(s.says)
				s.mu.Unlock()
				if slices.Contains(says, hand.Text) {
					t.Fatalf("the ended membership's words were spoken: %q", says)
				}
			}
			if asked.Load() == 0 {
				t.Fatal("control: the Supervisor never asked the hand whether its membership is current")
			}
		})
	}
}

// TestRaisedHandWaitsForAnInvitation fails if a raised hand plays no hand
// sound, speaks by itself on a free floor, or is not spoken, as its agent's
// words, on "Wispr, go ahead".
func TestRaisedHandWaitsForAnInvitation(t *testing.T) {
	r, outcomes := newHandsRig(t)
	hand := testHand("h1", "ABC-12", time.Minute)
	if err := r.s.RaiseHand(hand); err != nil {
		t.Fatalf("RaiseHand: %v", err)
	}
	eventually(t, "the hand sound", func() bool { return slices.Equal(r.speaker.soundsPlayed(), []string{"hand"}) })
	never(t, "a raised hand spoke with nobody inviting it", 600*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	r.heard("Wispr, go ahead.", true, true)
	if got := waitSaid(t, r.waitSession()); got != hand.Text {
		t.Fatalf("said %q, want the agent's words %q", got, hand.Text)
	}
	r.speaker.finish("voice")
	eventually(t, "the hand reported spoken", func() bool { return slices.Equal(outcomes.all(), []string{"h1:spoken"}) })
}

// TestRaisedHandExpires fails if a hand nobody invited before it expired is
// still spoken on "Wispr, go ahead", or its agent is not told that it expired.
func TestRaisedHandExpires(t *testing.T) {
	r, outcomes := newHandsRig(t)
	if err := r.s.RaiseHand(testHand("h1", "ABC-12", 150*time.Millisecond)); err != nil {
		t.Fatalf("RaiseHand: %v", err)
	}
	eventually(t, "the hand reported expired", func() bool { return slices.Equal(outcomes.all(), []string{"h1:expired"}) })
	r.heard("Wispr, go ahead.", true, true)
	never(t, "an expired hand was spoken", 400*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	if err := r.s.RaiseHand(testHand("h2", "ABC-12", -time.Second)); err == nil {
		t.Fatal("RaiseHand took a hand that had already expired")
	}
}

// TestGoAheadPicksAHandByItsKey fails if "Wispr, go ahead, KEY" does not
// speak the hand of the agent on KEY when another came first.
func TestGoAheadPicksAHandByItsKey(t *testing.T) {
	r, _ := newHandsRig(t)
	first, second := testHand("h1", "ABC-12", time.Minute), testHand("h2", "XYZ-9", time.Minute)
	for _, h := range []*Hand{first, second} {
		if err := r.s.RaiseHand(h); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
	}
	r.heard("Wispr, go ahead, XYZ-9.", true, true)
	if got := waitSaid(t, r.waitSession()); got != second.Text {
		t.Fatalf("said %q, want XYZ-9's agent's words", got)
	}
}

// TestWhoHasAHandUp fails if "Wispr, who has a hand up?" does not say the
// keys of the agents whose hands are up, or says a lowered or expired one.
func TestWhoHasAHandUp(t *testing.T) {
	t.Run("two hands", func(t *testing.T) {
		r, _ := newHandsRig(t)
		for _, h := range []*Hand{testHand("h1", "ABC-12", time.Minute), testHand("h2", "XYZ-9", time.Minute), testHand("h3", "OLD-1", 100*time.Millisecond)} {
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatalf("RaiseHand: %v", err)
			}
		}
		time.Sleep(200 * time.Millisecond)
		r.heard("Wispr, who has a hand up?", true, true)
		if got, want := waitSaid(t, r.waitSession()), "ABC-12 and XYZ-9 have a hand up."; got != want {
			t.Fatalf("said %q, want %q", got, want)
		}
	})
	t.Run("one hand", func(t *testing.T) {
		r, _ := newHandsRig(t)
		if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		r.heard("Wispr, who has a hand up?", true, true)
		if got, want := waitSaid(t, r.waitSession()), "ABC-12 has a hand up."; got != want {
			t.Fatalf("said %q, want %q", got, want)
		}
	})
	t.Run("no hand", func(t *testing.T) {
		r, _ := newHandsRig(t)
		if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		r.s.LowerHands("m-h1")
		r.heard("Wispr, who has a hand up?", true, true)
		if got, want := waitSaid(t, r.waitSession()), "Nobody has a hand up."; got != want {
			t.Fatalf("said %q, want %q", got, want)
		}
	})
}

// TestHandsComeDownWithTheirMeeting fails if a hand stays up after its agent
// left or its meeting ended, or its agent is told it was spoken or expired.
func TestHandsComeDownWithTheirMeeting(t *testing.T) {
	r, outcomes := newHandsRig(t)
	for _, h := range []*Hand{testHand("h1", "ABC-12", time.Minute), testHand("h2", "XYZ-9", time.Minute)} {
		if err := r.s.RaiseHand(h); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
	}
	r.s.LowerHands("m-h1")
	r.s.SessionEnded("sess-1")
	eventually(t, "both hands withdrawn", func() bool {
		got := outcomes.all()
		slices.Sort(got)
		return slices.Equal(got, []string{"h1:withdrawn", "h2:withdrawn"})
	})
	r.heard("Wispr, go ahead.", true, true)
	never(t, "a withdrawn hand was spoken", 400*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
}

// TestHandRefusedOffTheMeeting fails if a hand is taken for a meeting that is
// not the one recording, or while the voice is off.
func TestHandRefusedOffTheMeeting(t *testing.T) {
	r, _ := newHandsRig(t)
	other := testHand("h1", "ABC-12", time.Minute)
	other.SessionID = "sess-0"
	if err := r.s.RaiseHand(other); err == nil {
		t.Fatal("RaiseHand took a hand for a meeting that is not recording")
	}
	r.setEnabled(false)
	if err := r.s.RaiseHand(testHand("h2", "ABC-12", time.Minute)); err == nil {
		t.Fatal("RaiseHand took a hand with the voice off")
	}
}

// TestSegmentsReachTheRoster fails if a final segment of the room does not
// reach the Segment hook with its meeting, its words and its times, if a call
// to Wispr is not marked as one, or if an interim result or Wispr's own words
// reach it.
func TestSegmentsReachTheRoster(t *testing.T) {
	var mu sync.Mutex
	var got []Segment
	c := newFakeConcierge("The voice issue is in review.")
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.Segment = func(s *Segment) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, *s)
		}
	})
	segments := func() []Segment {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	before := time.Now()
	r.heard("the release of ABC-12 moved to Friday", false, false)
	r.heard("the release of ABC-12 moved to Friday", true, true)
	eventually(t, "the room's final segment", func() bool { return len(segments()) == 1 })
	s := segments()[0]
	if s.SessionID != "sess-1" || s.Text != "the release of ABC-12 moved to Friday" || s.Call {
		t.Fatalf("segment %+v, want the room's words in sess-1, not a call", s)
	}
	if s.From.Before(before.Add(-2*time.Second)) || s.To.Before(s.From) || s.To.After(time.Now()) {
		t.Fatalf("segment times %v to %v, want a range ending when it arrived", s.From, s.To)
	}

	sess := r.ask("ask the ABC-12 agent what is left?")
	eventually(t, "the call's segment", func() bool { return len(segments()) == 2 })
	if call := segments()[1]; !call.Call || !strings.Contains(call.Text, "ABC-12") {
		t.Fatalf("segment %+v, want the call marked as one", call)
	}
	waitSaid(t, sess)
	r.waitSpeaking()
	r.heard("The voice issue is in review.", true, true) // Wispr's own words, heard back
	never(t, "Wispr's own words reached the roster", 300*time.Millisecond, func() bool { return len(segments()) > 2 })
}
