// Sol's twenty-fifth review of #191 at 053303b
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6086871370):
// Sol's probes, kept as regression tests: hold generations, interim withdrawal and concurrent key refreshes.
// One expectation changed, to the rule this round fixes: TestSolR25InterimWithdrawal
// wanted the withdrawn interim "send these notes" still pending as the turn
// after the final that withdrew it; the turn is now empty, since a final that
// withdraws an interim clears it (Main, after Sol's twenty-fifth review).

package voice

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSolR25HoldGenerations(t *testing.T) {
	for _, wrapping := range []bool{false, true} {
		t.Run(fmt.Sprintf("counter_wrap_%v", wrapping), func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			sess := officeWake(t, r)
			ex := r24HeldExchange(t, r)
			r.s.mu.Lock()
			old := []int{ex.holdGen}
			if wrapping {
				ex.holdGen = int(^uint(0)>>1) - 2
				old = []int{ex.holdGen}
			}
			r.s.mu.Unlock()
			sawNegative := false
			for i := range 128 {
				r.heardAs("Whisper", false, false, 3)
				r.s.mu.Lock()
				released := !ex.misheardHold && r.s.ex == ex
				r.s.mu.Unlock()
				if !released {
					t.Fatalf("hold %d: explicit interim did not release the hold", i)
				}
				r.heardAs("Whisper. No. Wait,", true, true, 3)
				r.s.mu.Lock()
				gen, held := ex.holdGen, ex.misheardHold
				r.s.mu.Unlock()
				if !held {
					t.Fatalf("hold %d: withdrawn interim did not establish another hold", i)
				}
				sawNegative = sawNegative || gen < 0
				for _, prior := range old {
					if prior == gen {
						t.Fatalf("hold %d reused generation %d", i, gen)
					}
					r.s.holdExpired(ex, prior)
					if !r24SameExchange(r, ex) || closedNow(sess) {
						t.Fatalf("hold %d ended by old expiry %d (current %d)", i, prior, gen)
					}
				}
				old = append(old, gen)
			}
			if wrapping && !sawNegative {
				t.Fatal("counter-wrap control never crossed MaxInt")
			}
			r.s.holdExpired(ex, old[len(old)-1])
			eventually(t, "the current generation expiry closes its voice", func() bool { return closedNow(sess) })
			select {
			case words := <-c.asked:
				t.Fatalf("withdrawn calls handed over %q", words)
			default:
			}
		})
	}
}

func TestSolR25HoldGenerationAcrossExchanges(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	var old []*exchange
	for i := range 32 {
		sess := officeWake(t, r)
		ex := r24HeldExchange(t, r)
		r.s.mu.Lock()
		gen := ex.holdGen
		r.s.mu.Unlock()
		if gen != 1 {
			t.Fatalf("new exchange's first hold = %d", gen)
		}
		for _, prior := range old {
			r.s.holdExpired(prior, gen)
			if !r24SameExchange(r, ex) || closedNow(sess) {
				t.Fatalf("exchange %d ended by a prior exchange's same-number expiry", i)
			}
		}
		old = append(old, ex)
		r.s.holdExpired(ex, gen)
		eventually(t, "the fresh exchange's own expiry closes it", func() bool { return closedNow(sess) })
	}
}

func TestSolR25InterimWithdrawal(t *testing.T) {
	for _, final := range []string{"Whisper. No. Wait,", "Whisper networks are a problem."} {
		t.Run(final, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
			sess := officeWake(t, r)
			ex := r24HeldExchange(t, r)
			r.s.mu.Lock()
			old := ex.holdGen
			r.s.mu.Unlock()
			r.heardAs("Wispr, send these notes", false, false, 4)
			r.s.mu.Lock()
			released := !ex.misheardHold
			r.s.mu.Unlock()
			if !released {
				t.Fatal("explicit interim did not end the old hold")
			}
			r.s.holdExpired(ex, old)
			if !r24SameExchange(r, ex) {
				t.Fatal("queued expiry ended the interim call")
			}
			r.heardAs(final, true, true, 4)
			r.s.mu.Lock()
			held, gen, words := ex.misheardHold, ex.holdGen, r.s.turnOf(ex)
			r.s.mu.Unlock()
			if !held || gen == old || words != "" {
				t.Fatalf("withdrawal state held=%v gen=%d old=%d turn=%q", held, gen, old, words)
			}
			// A rejected statement neither releases this hold nor reaches Answer.
			r.heardAs("The budget can wait.", true, true, 9)
			r.s.holdExpired(ex, old)
			if !r24SameExchange(r, ex) || closedNow(sess) {
				t.Fatal("withdrawn interim's newer hold was killed by the original expiry")
			}
			r.s.holdExpired(ex, gen)
			eventually(t, "withdrawn call times out", func() bool { return closedNow(sess) })
			select {
			case got := <-c.asked:
				t.Fatalf("withdrawn interim reached Answer: %q", got)
			default:
			}
		})
	}
}

func TestSolR25KeysConcurrentRefresh(t *testing.T) {
	type snapshot struct{ keys []string }
	var current atomic.Pointer[snapshot]
	current.Store(&snapshot{[]string{"OPS"}})
	refresh, done := make(chan struct{}), make(chan struct{})
	applied := make(chan struct{})
	go func() {
		defer close(done)
		for range refresh {
			next := []string{"INFRA"}
			if current.Load().keys[0] == "INFRA" {
				next = []string{"OPS"}
			}
			current.Store(&snapshot{next})
			applied <- struct{}{}
		}
	}()
	defer func() { close(refresh); <-done }()
	// The writer finishes between taking the immutable snapshot and returning it.
	// This is the copy-on-write behavior of production projectKeys.set/Get.
	var gotSnapshot string
	var calls int
	s := &Supervisor{cfg: SupervisorConfig{ProjectKeys: func() []string {
		snap := current.Load()
		gotSnapshot = strings.Join(snap.keys, "\n")
		calls++
		refresh <- struct{}{}
		<-applied
		return snap.keys
	}}}
	ex := &exchange{}
	memo := new(keysMemo)
	const raw = "status of infra forty five ninety two?"
	for i := range 1024 {
		before := calls
		got := s.keysOfLocked(ex, memo, raw)
		want := raw
		if gotSnapshot == "INFRA" {
			want = "status of INFRA-4592?"
		}
		if calls-before != 1 || memo.keys != gotSnapshot || got != want {
			t.Fatalf("refresh %d: calls %d, snapshot %q, memo %q, words %q want %q", i, calls-before, gotSnapshot, memo.keys, got, want)
		}
	}
}
