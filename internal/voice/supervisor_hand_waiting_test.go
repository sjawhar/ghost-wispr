package voice

import (
	"slices"
	"testing"
	"time"
)

// The tests here join raised hands to #127's waiting light and to "Wispr,
// never mind": a hand waits for a person as a held answer does, so the light
// flashes while it is up; and never mind drops held answers, never a hand,
// which only its agent, its expiry or its meeting takes down.

// newHandsWaitingRig is a rig with a waiting light whose Supervisor reports
// each hand's outcome.
func newHandsWaitingRig(t *testing.T) (*rig, *fakeWaitingLight, *handOutcomes) {
	t.Helper()
	light, outcomes := &fakeWaitingLight{}, &handOutcomes{}
	r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, _ *rig) {
		cfg.WaitingLight = light
		cfg.HandDone = outcomes.record
	})
	return r, light, outcomes
}

// TestARaisedHandLightsTheWaitingLight fails if a raised hand does not turn
// the waiting light on, or if the light stays on once the hand came down, by
// its membership's end, by its expiry, or by being withdrawn; or stays off
// while another hand is still up.
func TestARaisedHandLightsTheWaitingLight(t *testing.T) {
	t.Run("lowered", func(t *testing.T) {
		r, light, _ := newHandsWaitingRig(t)
		first, second := testHand("h1", "ABC-12", time.Minute), testHand("h2", "XYZ-9", time.Minute)
		for _, h := range []*Hand{first, second} {
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatalf("RaiseHand: %v", err)
			}
		}
		eventually(t, "the waiting light on", light.lit)
		r.s.LowerHands(first.Membership)
		never(t, "the light went off with a hand still up", 200*time.Millisecond, func() bool { return !light.lit() })
		r.s.LowerHands(second.Membership)
		eventually(t, "the waiting light off", func() bool { return !light.lit() })
	})
	t.Run("expired", func(t *testing.T) {
		r, light, outcomes := newHandsWaitingRig(t)
		if err := r.s.RaiseHand(testHand("h1", "ABC-12", 150*time.Millisecond)); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		eventually(t, "the waiting light on", light.lit)
		eventually(t, "the hand expired", func() bool { return slices.Equal(outcomes.all(), []string{"h1:expired"}) })
		eventually(t, "the waiting light off", func() bool { return !light.lit() })
	})
	t.Run("withdrawn", func(t *testing.T) {
		r, light, outcomes := newHandsWaitingRig(t)
		hand := testHand("h1", "ABC-12", time.Minute)
		var ended bool
		hand.Current = func() bool { return !ended }
		if err := r.s.RaiseHand(hand); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		eventually(t, "the waiting light on", light.lit)
		r.s.mu.Lock()
		ended = true
		r.s.mu.Unlock()
		r.heard("Wispr, go ahead.", true, true)
		eventually(t, "the hand withdrawn", func() bool { return slices.Equal(outcomes.all(), []string{"h1:withdrawn"}) })
		eventually(t, "the waiting light off", func() bool { return !light.lit() })
	})
}

// TestNeverMindLeavesHandsUp fails if "Wispr, never mind" drops a raised
// hand, turns the waiting light off while a hand is up, or cuts the hand
// sound; with a held answer and a hand both waiting, it drops the answer and
// the hand is still spoken on "Wispr, go ahead".
func TestNeverMindLeavesHandsUp(t *testing.T) {
	t.Run("a hand alone", func(t *testing.T) {
		c := newFakeConcierge()
		light := &fakeWaitingLight{}
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.WaitingLight = light })
		hand := testHand("h1", "ABC-12", time.Minute)
		if err := r.s.RaiseHand(hand); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		eventually(t, "the hand sound", func() bool { return slices.Equal(r.speaker.soundsPlayed(), []string{"hand"}) })
		r.heard("Wispr, never mind.", true, true)
		eventually(t, "the cancel asked of the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
		never(t, "never mind turned the light off with a hand up", 300*time.Millisecond, func() bool { return !light.lit() })
		r.heard("Wispr, go ahead.", true, true)
		if got := waitSaid(t, r.waitSession()); got != hand.Text {
			t.Fatalf("said %q, want the hand's words %q: never mind dropped the hand", got, hand.Text)
		}
	})
	t.Run("a held answer and a hand", func(t *testing.T) {
		c := newFakeConcierge()
		light := &fakeWaitingLight{}
		r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.WaitingLight = light })
		holdAnswer(t, r, light, "t1", "It ships tomorrow.")
		hand := testHand("h1", "ABC-12", time.Minute)
		if err := r.s.RaiseHand(hand); err != nil {
			t.Fatalf("RaiseHand: %v", err)
		}
		r.heard("Wispr, never mind.", true, true)
		eventually(t, "the cancel asked of the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
		eventually(t, "the held answer dropped", func() bool {
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return len(r.s.pending) == 1 && r.s.pending[0].hand != nil
		})
		if !light.lit() {
			t.Fatal("the waiting light went off with the hand still up")
		}
		r.heard("Wispr, go ahead.", true, true)
		if got := waitSaid(t, r.waitSession()); got != hand.Text {
			t.Fatalf("said %q, want the hand's words %q", got, hand.Text)
		}
	})
}
