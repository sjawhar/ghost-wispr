// Sol's twenty-eighth review of #191 at 37103b6
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6090659290):
// Sol's probes, kept as regression tests. Two changes, each as Main directed: the drifted finals of
// TestSolR28ShorterCorrectionWithStartDrift and TestSolR28PlainFinalWithStartDrift
// keep their result's start at 5 s while their first word moves
// (r28HearDrift), as Deepgram's do, where Sol's moved both; and
// TestSolR28OlderCallDuringHold keeps only its 10 s row, since its 2 s row, an
// older call during a hold, behaves as at the baseline, which Main kept.

package voice

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

func TestSolR28ShorterCorrectionWithStartDrift(t *testing.T) {
	for _, delta := range []time.Duration{0, time.Millisecond, time.Millisecond + time.Nanosecond, 1414020 * time.Nanosecond, 10 * time.Millisecond, 25 * time.Millisecond} {
		t.Run(delta.String(), func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = 200 * time.Millisecond })
			capture := time.Now().Add(-time.Minute)
			r27Hear(r, capture, "Whisper", 0, false, false, 3)
			sess := r.waitSession()
			r27Hear(r, capture, "Whisper. No. Oh, yeah,", 0, true, true, 3)
			r24HeldExchange(t, r)
			interim := r27Hear(r, capture, "Wispr, send these notes", 5, false, false, 4)
			final := r28HearDrift(r, capture, "Whisper. No. Wait,", 5, 5+delta.Seconds(), true, true, 4)
			if !final.endsAt().Before(interim.Words[len(interim.Words)-1].At) {
				t.Fatal("control: final did not shorten its interim")
			}
			r.s.mu.Lock()
			ex := r.s.ex
			pending, held := r.s.turnOf(ex), ex.misheardHold
			r.s.mu.Unlock()
			if pending != "" || !held {
				t.Errorf("drift=%v left withdrawn words: pending=%q held=%v", delta, pending, held)
			}
			r.s.OnUtteranceEnd(0, true, final.endsAt(), monoclock.Now())
			select {
			case got := <-c.asked:
				t.Fatalf("drift=%v timer answered withdrawn request %q", delta, got)
			case <-time.After(350 * time.Millisecond):
				if !closedNow(sess) {
					t.Fatal("the corrected call did not expire")
				}
			}
		})
	}
}

func TestSolR28PlainFinalWithStartDrift(t *testing.T) {
	for _, delta := range []time.Duration{0, 1414020 * time.Nanosecond} {
		t.Run(delta.String(), func(t *testing.T) {
			r, _, _, capture := r27Open(t)
			r27Hear(r, capture, "Wispr, send these notes", 5, false, false, 4)
			final := r28HearDrift(r, capture, "Send the report.", 5, 5+delta.Seconds(), true, true, 4)
			r.s.mu.Lock()
			ex := r.s.ex
			pending := r.s.turnOf(ex)
			r.s.mu.Unlock()
			if pending != "Send the report." {
				t.Errorf("corrected final kept obsolete interim: %q", pending)
			}
			r.s.OnUtteranceEnd(0, true, final.endsAt(), monoclock.Now())
			r.s.turnWaited(ex)
			if got := r.waitTurn(); got != "Send the report." {
				t.Errorf("handed=%q", got)
			}
		})
	}
}

func TestSolR28TwoExplicitCalls(t *testing.T) {
	for _, late := range []bool{false, true} {
		for _, speaker := range []int{4, 9} {
			t.Run(fmt.Sprintf("late_%v/speaker_%d", late, speaker), func(t *testing.T) {
				r, _, _, capture := r27Open(t)
				older := func() { r27Hear(r, capture, "Wispr, what is the budget?", 2, true, false, speaker) }
				if !late {
					older()
				}
				r27Hear(r, capture, "Wispr, can you send these notes?", 10, false, false, 4)
				r.s.mu.Lock()
				ex := r.s.ex
				beforeTurn := ex.x.Turn
				r.s.mu.Unlock()
				if late {
					older()
				}
				q := r27Hear(r, capture, "Wispr, can you send these notes?", 10, true, true, 4)
				_ = q
				got := r.waitTurn()
				r.s.mu.Lock()
				afterTurn := ex.x.Turn
				r.s.mu.Unlock()
				t.Logf("turn=%d->%d request=%q", beforeTurn, afterTurn, got)
				if got != "can you send these notes?" {
					t.Errorf("an older distinct call changed the later call's request: %q", got)
				}
			})
		}
	}
}

func TestSolR28OlderCallDuringHold(t *testing.T) {
	for _, callStart := range []float64{10} {
		t.Run(fmt.Sprint(callStart), func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			capture := time.Now().Add(-time.Minute)
			r27Hear(r, capture, "Whisper", 5, false, false, 3)
			r.waitSession()
			r27Hear(r, capture, "Whisper. No. Oh, yeah,", 5, true, true, 3)
			ex := r24HeldExchange(t, r)
			r.s.mu.Lock()
			beforeTurn := ex.x.Turn
			r.s.mu.Unlock()
			r27Hear(r, capture, "Wispr, what is the budget?", callStart, true, true, 9)
			r.s.mu.Lock()
			pending, held, afterTurn := r.s.turnOf(ex), ex.misheardHold, ex.x.Turn
			r.s.mu.Unlock()
			t.Logf("start=%g held=%v turn=%d->%d pending=%q", callStart, held, beforeTurn, afterTurn, pending)
			if callStart > 5 {
				if got := r.waitTurn(); !strings.Contains(got, "what is the budget?") {
					t.Fatalf("new call not answered: %q", got)
				}
				return
			}
			if !held || afterTurn != beforeTurn {
				t.Errorf("an older call released the newer hold")
			}
			select {
			case got := <-c.asked:
				t.Errorf("older call escaped hold to Answer: %q", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}
