package voice

import (
	"strings"
	"testing"
	"time"
)

// The office, 2026-11-27 03:09Z (dispatch://INFRA-4592, session
// 20261127001917): an interim heard the wake and the chime played; the final
// was "Whisper. No. Oh, yeah,", the name where the wake was heard but not as
// a call, and the exchange ended 2.26 s after the wake as a misheard wake. The
// request came in the next finals, which Deepgram labelled with three other
// speaker numbers: "great. Can you send these notes to to whom? To Raj. On
// dispatch. Make a dispatch issue and assign it to Raj." After a chime, a
// final that holds the name but no call now keeps the exchange open for the
// next finals for TurnWait (Main, on INFRA-4592); only when no final in that
// time addresses Wispr does it end as misheard.

// heardAs is rig.heard with every word of the result said by speaker.
func (r *rig) heardAs(text string, final, speechFinal bool, speaker int) {
	r.t.Helper()
	r.mu.Lock()
	closings, start := r.closings, r.start
	if final {
		r.start += 5
	}
	r.mu.Unlock()
	tr := transcriptOf(text, start, final, speechFinal, closings)
	for i := range tr.Words {
		tr.Words[i].Speaker = speaker
	}
	r.s.OnTranscript(&tr)
}

// officeWake opens the office exchange: the interim that heard the wake, and
// the final that holds the name but no call.
func officeWake(t *testing.T, r *rig) *fakeSession {
	t.Helper()
	r.heardAs("Whisper", false, false, 3)
	sess := r.waitSession()
	r.heardAs("Whisper. No. Oh, yeah,", true, true, 3)
	return sess
}

func closedNow(sess *fakeSession) bool { _, _, closed := sess.state(); return closed }

// TestAPauseAfterTheNameKeepsTheCall is the office case: the request in the
// next finals, from other speaker numbers, within TurnWait, is the asker's
// turn, and the Concierge gets all of it.
func TestAPauseAfterTheNameKeepsTheCall(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Second })
	sess := officeWake(t, r)
	time.Sleep(200 * time.Millisecond) // the asker hesitates
	if closedNow(sess) {
		t.Fatal("the exchange ended on the final that holds the name but no call")
	}
	r.heardAs("great. Can you send these notes to to whom?", true, false, 1)
	r.heardAs("To Raj. On dispatch.", true, false, 2)
	r.heardAs("Make a dispatch issue and assign it to Raj.", true, true, 4)
	select {
	case turn := <-c.asked:
		for _, want := range []string{"Can you send these notes", "To Raj", "assign it to Raj"} {
			if !strings.Contains(turn, want) {
				t.Errorf("the Concierge was asked %q, want the whole request, with %q", turn, want)
			}
		}
		if strings.Contains(turn, "Oh, yeah") {
			t.Errorf("the Concierge was asked %q, with the misheard final's words", turn)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request after the pause never reached the Concierge")
	}
}

// TestAMisheardWakeStillEndsWhenNobodyAddressesWispr: the name with no call,
// then talk that asks nothing ("they really are."), ends the exchange as a
// misheard wake once TurnWait passes, and nothing reaches the Concierge. A
// request said after that time is not answered: the exchange it would have
// been is over. "Whisper networks are a problem." ends as it did
// (TestAWakeTheFinalWithdrawsEndsTheExchange, TestAMisheardWakeReadsNothing).
func TestAMisheardWakeStillEndsWhenNobodyAddressesWispr(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(r *rig)
	}{
		{"talk that asks nothing", func(r *rig) { r.heardAs("they really are.", true, true, 1) }},
		{"a request after the bound", func(r *rig) {
			time.Sleep(500 * time.Millisecond)
			r.heardAs("great. Can you send these notes to Raj?", true, true, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge("fact")
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = 300 * time.Millisecond })
			sess := officeWake(t, r)
			tc.after(r)
			eventually(t, "the misheard wake's exchange ended", func() bool { return closedNow(sess) })
			select {
			case turn := <-c.asked:
				t.Fatalf("the Concierge was asked %q, which nobody addressed to Wispr in time", turn)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// TestTheHoldAsksTheConciergesGate: the Supervisor decides whether a final
// after a misheard wake addresses Wispr by Config.Asks, the Concierge's own
// gate in the unit, not by a question mark alone. A gate that accepts an
// imperative with none makes it the asker's turn.
func TestTheHoldAsksTheConciergesGate(t *testing.T) {
	c := newFakeConcierge("fact")
	const request = "Make a dispatch issue and assign it to Raj."
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.TurnWait = time.Second
		cfg.Asks = func(turn string) bool { return turn == request }
	})
	officeWake(t, r)
	r.heardAs(request, true, true, 4)
	select {
	case turn := <-c.asked:
		if !strings.Contains(turn, "assign it to Raj") {
			t.Errorf("the Concierge was asked %q, want %q", turn, request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a final the gate accepts never reached the Concierge")
	}
}
