package voice

import (
	"testing"
	"time"
)

// misheardWaited is the expiry of the hold ex holds now, as its timer would
// run it: Sol's round-24 probes call it to run an expiry at a chosen moment.
func (s *Supervisor) misheardWaited(ex *exchange) {
	s.mu.Lock()
	gen := ex.holdGen
	s.mu.Unlock()
	s.holdExpired(ex, gen)
}

// TestAnOldHoldsExpiryEndsNoLaterHold: a hold's expiry belongs to that hold.
// The name is misheard, then said again, which ends the first hold, and its
// final is misheard too, which begins a second hold. The first hold's expiry,
// already waiting for the lock when the call came (so its timer's Stop came
// too late), then runs during the second hold, and must end nothing; the
// second hold's own expiry still ends the exchange (Sol's twenty-fourth review
// of #191, the queued expiry).
func TestAnOldHoldsExpiryEndsNoLaterHold(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	sess := officeWake(t, r) // the first hold
	r.s.mu.Lock()
	ex, first := r.s.ex, r.s.ex.holdGen
	r.s.mu.Unlock()
	r.heardAs("Whisper", false, false, 3)          // the name again: a call, which ends the first hold
	r.heardAs("Whisper. No. Wait,", true, true, 3) // misheard again: the second hold
	r.s.mu.Lock()
	held, same, second := ex.misheardHold, r.s.ex == ex, ex.holdGen
	r.s.mu.Unlock()
	if !held || !same || second == first {
		t.Fatalf("control: the second misheard final began no second hold on the same exchange (held %v, same %v, generations %d and %d)", held, same, first, second)
	}
	r.s.holdExpired(ex, first)
	if closedNow(sess) || !r24SameExchange(r, ex) {
		t.Fatal("the first hold's expiry ended the exchange during the second hold")
	}
	r.s.holdExpired(ex, second)
	eventually(t, "the second hold's expiry ends the exchange", func() bool { return closedNow(sess) })
}
