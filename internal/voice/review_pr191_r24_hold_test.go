// Sol's twenty-fourth review of #191 at 12c293e
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6084899163):
// Sol's probes, kept as regression tests, except TestSolR24HoldBargeInWhileHeld:
// its held-and-speaking row sets the voice speaking in a state where no answer
// plays, which Sol's review says does not establish a reachable regression, and
// it fails the same at 12c293e.

package voice

import (
	"strings"
	"testing"
	"time"
)

// These overlay probes are not a replacement for concierge.Asks. The voice
// package cannot import concierge without an import cycle; the companion
// hold_concierge_r24_test.go exercises the real gate through a public Supervisor.
// A minute-long timer below lets tests invoke the exact expiry callback without
// racing a real deadline. The natural-expiry test is separate.
func r24HeldExchange(t *testing.T, r *rig) *exchange {
	t.Helper()
	r.s.mu.Lock()
	ex := r.s.ex
	held := ex != nil && ex.misheardHold && !ex.answering && !ex.drives && ex.timer != nil
	r.s.mu.Unlock()
	if !held {
		t.Fatal("control: officeWake did not enter a non-driving hesitation hold with a timer")
	}
	return ex
}

func r24SameExchange(r *rig, ex *exchange) bool {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.ex == ex && !ex.ended && ex.ctx.Err() == nil
}

// An explicit second wake addresses Wispr without needing the hold's question
// heuristic. Its ordinary continuation must therefore reach the same turn even
// when it is not a question on its own. The always-false gate isolates whether
// a gate is incorrectly consulted after an explicit wake; it does not model
// the production gate's language recognition.
func TestSolR24HoldSecondWakeContinuation(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "control_without_hold"
		if held {
			name = "second_wake_during_hold"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
				cfg.TurnWait = time.Minute
				cfg.Asks = func(string) bool { return false }
			})
			if held {
				officeWake(t, r)
				r24HeldExchange(t, r)
			} else {
				r.heardAs("Wispr.", true, false, 3)
				r.waitSession()
			}
			r.s.mu.Lock()
			ex := r.s.ex
			r.s.mu.Unlock()
			r.heardAs("Wispr, send these notes", true, false, 4)
			if !r24SameExchange(r, ex) {
				t.Fatal("the explicit second wake replaced or ended the exchange")
			}
			r.heardAs("to Raj on Dispatch.", true, true, 4)
			select {
			case got := <-c.asked:
				if got != "send these notes to Raj on Dispatch." {
					t.Errorf("second-wake request = %q, want its complete continuation", got)
				}
			case <-time.After(time.Second):
				r.s.mu.Lock()
				pending, hold := r.s.turnOf(ex), ex.misheardHold
				r.s.mu.Unlock()
				t.Fatalf("the second wake's endpoint handed no turn over: pending=%q hold=%v", pending, hold)
			}
		})
	}
}

// Stop normally cancels the old hold timer. Crossing its original deadline is
// not by itself proof that a queued callback is harmless; that case is tested
// separately below. Keep Answer pending so completion cannot end the exchange.
func TestSolR24HoldSecondWakeCrossesOldExpiry(t *testing.T) {
	const wait = 350 * time.Millisecond
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = wait })
	sess := officeWake(t, r)
	ex := r24HeldExchange(t, r)
	r.heardAs("Wispr, tell me about ABC-12.", true, true, 4)
	if got := r.waitTurn(); got != "tell me about ABC-12." {
		t.Fatalf("control: second explicit wake handed %q to the Concierge", got)
	}
	time.Sleep(2 * wait)
	if !r24SameExchange(r, ex) || closedNow(sess) {
		t.Fatal("the answer to the second explicit wake ended across the old hold's deadline")
	}
}

// Model the AfterFunc race precisely: the old callback was runnable before a
// second wake got the mutex, and acquires it after that wake hands over its
// turn. Calling the production callback is deterministic; it does not establish
// the frequency of this scheduler interleaving. The still-held control proves
// the injected callback really can end this exact exchange.
func TestSolR24HoldQueuedExpiryAfterExplicitWake(t *testing.T) {
	for _, tc := range []struct {
		name, request string
		wantLive      bool
	}{
		{"control_still_held", "", false},
		{"control_question_releases_hold", "Can you send these notes?", true},
		{"explicit_second_wake", "Wispr, tell me about ABC-12.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Minute })
			sess := officeWake(t, r)
			ex := r24HeldExchange(t, r)
			if tc.request != "" {
				r.heardAs(tc.request, true, true, 4)
				r.waitTurn() // the response has begun, on the same exchange
				if !r24SameExchange(r, ex) {
					t.Fatal("control: the new request was not accepted on the held exchange")
				}
			}
			r.s.misheardWaited(ex)
			if live := r24SameExchange(r, ex); live != tc.wantLive {
				t.Errorf("after the old hold callback, live=%v, want %v", live, tc.wantLive)
			}
			if !tc.wantLive {
				eventually(t, "the injected expiry closed its voice", func() bool { return closedNow(sess) })
			}
		})
	}
}

func TestSolR24HoldStopAndGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*rig)
	}{
		{"spoken_stop_interim", func(r *rig) { r.heardAs("Wispr, stop", false, false, 4) }},
		{"spoken_stop_final", func(r *rig) { r.heardAs("Wispr, stop", true, true, 4) }},
		{"button_stop", func(r *rig) { r.s.Stop("r24 button control") }},
		{"capture_gate_close", func(r *rig) { r.closeGate() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Minute })
			sess := officeWake(t, r)
			old := r24HeldExchange(t, r)
			tc.stop(r)
			if r24SameExchange(r, old) || old.ctx.Err() == nil {
				t.Fatal("stop left the held exchange or its context live")
			}
			eventually(t, "stop closed the held voice", func() bool { return closedNow(sess) })
			r.heardAs("Can you send these notes?", true, true, 4)
			select {
			case got := <-c.asked:
				t.Fatalf("a request with no new wake revived the stopped hold: %q", got)
			default:
			}
			if tc.name == "capture_gate_close" {
				for _, bad := range []struct {
					name         string
					closings     int
					open, finish bool
				}{
					{"pre_close_result", 0, true, false},
					{"closed_gate_result", 1, false, false},
					{"finalize_reply", 1, true, true},
				} {
					tr := transcriptOf("Wispr, what is the status?", 50, true, true, bad.closings)
					tr.GateOpen, tr.FromFinalize = bad.open, bad.finish
					r.s.OnTranscript(&tr)
					r.s.mu.Lock()
					opened := r.s.ex != nil
					r.s.mu.Unlock()
					if opened {
						t.Fatalf("%s reopened the exchange after capture closed", bad.name)
					}
				}
			}
			// A valid new wake is the positive control for the stopped/stale
			// results. A callback belonging to old must not kill this new one.
			r.heardAs("Wispr, what is the status?", true, true, 4)
			fresh := r.waitSession()
			if got := r.waitTurn(); got != "what is the status?" {
				t.Fatalf("control: the valid new wake handed %q over", got)
			}
			r.s.misheardWaited(old)
			if closedNow(fresh) {
				t.Fatal("an old held exchange's callback closed the fresh voice")
			}
		})
	}
}

// Rejected finals/interims must not refresh the bound. Use the actual callback
// after checking timer identity, so scheduler delays cannot make this test pass
// merely because a short real timer happened to win the race first.
func TestSolR24HoldBoundAndWhisperNetworks(t *testing.T) {
	for _, final := range []string{"Whisper. No. Oh, yeah,", "Whisper networks are a problem."} {
		t.Run(final, func(t *testing.T) {
			c := newFakeConcierge()
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Minute })
			r.heardAs("Whisper", false, false, 3)
			sess := r.waitSession()
			r.heardAs(final, true, true, 3)
			ex := r24HeldExchange(t, r)
			r.s.mu.Lock()
			timer := ex.timer
			r.s.mu.Unlock()
			r.heardAs("They really are.", false, false, 1)
			r.heardAs("They really are.", true, true, 1)
			r.s.mu.Lock()
			unchanged := ex.timer == timer && ex.misheardHold && r.s.turnOf(ex) == ""
			r.s.mu.Unlock()
			if !unchanged {
				t.Fatal("non-question talk changed the hold's timer or entered its request")
			}
			r.s.misheardWaited(ex)
			eventually(t, "the bounded misheard exchange closed", func() bool { return closedNow(sess) })
			r.heardAs("Can you send these notes?", true, true, 4)
			select {
			case got := <-c.asked:
				t.Fatalf("expired misheard wake accepted %q", got)
			default:
			}
			for _, h := range c.heardCalls() {
				if strings.Contains(h.words, "networks") || strings.Contains(h.words, "They really") {
					t.Errorf("rejected text reached Concierge.Heard: %q", h.words)
				}
			}
		})
	}
}
