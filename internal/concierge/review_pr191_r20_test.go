package concierge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's twentieth review of #191 at d1a563d
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6078702495),
// through the real Supervisor, Concierge and GPT-Live client on a loopback
// vendor. A request is owned by final and by span: a claim marks the exact
// spans it sent, and a turn's words no request asked stay pending when the
// next turn begins (Main, after the review).

// TestAClosingQuoteRestoresWordsNoRequestSent (finding 1, Sol's
// TestSol191R20QuoteRestoresClaimedSpan): inside one turn, `does ABC-42 show
// "Please`, then `cancel`, which provisionally takes back what came before
// it, then `then retry`. A delegation claims the surviving `then retry`. The
// closing quote of `later" in the tooltip?` turns `cancel` back into quoted
// text, which restores words no request sent. At d1a563d the claim's
// revision boundary hid them, and the next delegation asked only the last
// final. They are asked now, and the claimed `then retry` is not asked again.
// Control: no early claim, and the whole question is asked once.
func TestAClosingQuoteRestoresWordsNoRequestSent(t *testing.T) {
	const first = `does ABC-42 show "Please`
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("early_claim_%v", early), func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatalf("initial question %q", q)
			}
			r.hearNow(first, false)
			r.hearNow("cancel", false)
			r.hearNow("then retry", false)
			if early {
				sendDelegation(w, "provisional", 2000)
				if q := toolWithin(t, v, time.Second); q != "then retry" {
					t.Fatalf("the provisional request was %q, want %q", q, "then retry")
				}
			}
			r.hearNow(`later" in the tooltip?`, false)
			sendDelegation(w, "restored", 3000)
			q := toolWithin(t, v, time.Second)
			want := `does ABC-42 show "Please cancel then retry later" in the tooltip?`
			if early {
				want = `does ABC-42 show "Please cancel later" in the tooltip?`
			}
			if q != want {
				t.Errorf("after the closing quote the tool was asked %q, want %q", q, want)
			}
		})
	}
}

// arrivalGate pauses one read of the room, the one at blockAt, as goroutine
// scheduling can; 0 pauses none.
type arrivalGate struct {
	inner   voice.Voice
	mu      sync.Mutex
	reads   int
	blockAt int
	blocked chan struct{}
	release chan struct{}
	opened  chan struct{}
}

func (*arrivalGate) DrivesTurn() bool { return true }

// armWorker makes the gate pause the next delegation's worker before its
// first read of the room: a delegation reads the room the same number of
// times as the one before it, the session's own opening read aside, and its
// worker's first read is the last of them before it waits. Counting that way
// pauses the same point whether or not the session also reads the room as a
// delegation arrives, so the test runs unchanged at d1a563d, which does not.
func (g *arrivalGate) armWorker() {
	g.mu.Lock()
	defer g.mu.Unlock()
	perDelegation := g.reads - 1
	g.blockAt = g.reads + perDelegation
}

func (g *arrivalGate) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	y := *x
	base := x.Room
	y.Room = func() voice.RoomView {
		g.mu.Lock()
		g.reads++
		block := g.reads == g.blockAt
		g.mu.Unlock()
		if block {
			close(g.blocked)
			<-g.release
		}
		return base()
	}
	s, err := g.inner.Open(ctx, &y)
	if err == nil {
		close(g.opened)
	}
	return s, err
}

// TestATakeBackBeforeTheWorkersFirstReadEndsTheDelegation (finding 2, Sol's
// TestSol191R20TakebackBetweenArrivalAndFirstRoomRead): a delegation arrives
// with no new final; before its worker first reads the room, the room says
// "never mind". At d1a563d the worker took the room after the take-back as
// the state it waited from, kept waiting, and asked a later, unrelated final
// under that old delegation. The room is now read as the delegation arrives,
// and a take-back after that ends it. Control: with no take-back the
// delegation still waits for the later final and asks it.
func TestATakeBackBeforeTheWorkersFirstReadEndsTheDelegation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("takeback_%v", cancel), func(t *testing.T) {
			url, conns := loopbackVendor(t, false)
			live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			gate := &arrivalGate{inner: live, blocked: make(chan struct{}), release: make(chan struct{}), opened: make(chan struct{})}
			v := watch(gate)
			r := openingRig(t, v)
			heldIssue(r.rig, "session", false)
			r.model.turns = answers(8)
			r.hearNow("Wispr, what about ABC-42?", false)
			w := within(t, conns, "vendor")
			w.next(t, "session.start")
			within(t, v.opened, "Open")
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatal(q)
			}
			var once sync.Once
			defer once.Do(func() { close(gate.release) })
			gate.armWorker()
			sendDelegation(w, "waiting", 2000)
			within(t, gate.blocked, "the waiting delegation's worker before its first read")
			if cancel {
				r.hearNow("never mind", false)
			}
			once.Do(func() { close(gate.release) })
			time.Sleep(50 * time.Millisecond)
			r.hearNow("is ABC-12 merged?", false)
			q := toolWithin(t, v, 800*time.Millisecond)
			if cancel && q != "" {
				t.Errorf("a delegation that arrived before the take-back outlived it and asked %q", q)
			}
			if !cancel && q != "is ABC-12 merged?" {
				t.Errorf("control: the waiting delegation asked %q, want the later final", q)
			}
		})
	}
}

// TestAQuestionInAnEarlierTurnIsStillAsked (the limit named at d1a563d, Sol's
// TestSol191R20UnclaimedEarlierTurn): after the opening is answered, the room
// asks "when will ABC-12 ship?", ends that utterance, then asks "and is
// ABC-42 merged?" before any delegation comes. At d1a563d the new turn
// discarded the first question. Both are asked now, in order, each whole.
// The "okay" that ended the opening turn is a backchannel, which never stops
// the voice either, and is left out. Control: a delegation before the second
// utterance asks the first question alone, and the next asks only the second.
func TestAQuestionInAnEarlierTurnIsStillAsked(t *testing.T) {
	const first, second = "when will ABC-12 ship?", "and is ABC-42 merged?"
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("delegate_before_next_utterance_%v", early), func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatal(q)
			}
			r.hearNow("okay", true)
			r.hearNow(first, true)
			if early {
				sendDelegation(w, "first", 2000)
				if q := toolWithin(t, v, 2*time.Second); q != first {
					t.Fatalf("control: the first question was asked as %q", q)
				}
			}
			r.hearNow(second, true)
			// The tool's own argument: the Concierge may answer a question
			// without calling its model, so the model's last question can
			// be an earlier one.
			sendDelegation(w, "after", 3000)
			q := toolWithin(t, v, 2*time.Second)
			want := first + " " + second
			if early {
				want = second
			}
			if q != want {
				t.Errorf("the tool was asked %q, want %q", q, want)
			}
		})
	}
}

// TestATakeBackInALaterTurnWithdrawsAnUnaskedQuestion (Main, after Sol's
// twentieth review): earlier turns now stay pending, so a take-back in a
// later turn must withdraw them too, or the room's "never mind" would be
// answered. The question, an endpoint, "never mind" alone: the delegation
// asks nothing. Words a request already sent are not touched. Controls: with
// no take-back the earlier question is asked; a take-back followed by a new
// question in the same later turn asks only the new one.
func TestATakeBackInALaterTurnWithdrawsAnUnaskedQuestion(t *testing.T) {
	for _, tc := range []struct{ name, later, want string }{
		{"never mind alone", "never mind", ""},
		{"control: no take-back", "and who holds it?", "what about ABC-12? and who holds it?"},
		{"control: a take-back then a new question", "never mind. is ABC-42 merged?", "is ABC-42 merged?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatal(q)
			}
			r.hearNow("okay", true)
			r.hearNow("what about ABC-12?", true) // its own turn, never asked
			r.hearNow(tc.later, true)
			sendDelegation(w, "after", 2000)
			if q := toolWithin(t, v, 800*time.Millisecond); q != tc.want {
				t.Errorf("the delegation asked %q, want %q", q, tc.want)
			}
		})
	}
}

// TestAnEarlierTurnOfBackchannelsIsNoRequest (Main, after Sol's twentieth
// review): an earlier turn left unasked is dropped only when it is made of the
// Supervisor's backchannel words, the same list that keeps a backchannel from
// stopping the voice. Anything else in it is kept. The current turn is never
// filtered: "yeah" said now may answer Wispr's own question ("Did you mean
// ABC-12?"), and is the room's request.
func TestAnEarlierTurnOfBackchannelsIsNoRequest(t *testing.T) {
	for _, tc := range []struct{ name, earlier, current, want string }{
		{"okay", "okay", "is ABC-12 merged?", "is ABC-12 merged?"},
		{"yeah, right", "yeah, right", "is ABC-12 merged?", "is ABC-12 merged?"},
		{"control: okay then words", "okay, the budget", "is ABC-12 merged?", "okay, the budget is ABC-12 merged?"},
		{"control: a backchannel said now", "", "yeah", "yeah"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, v, w := openVoiceB(t)
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-42?" {
				t.Fatal(q)
			}
			r.hearNow("mm-hm", true) // ends the opening turn
			if tc.earlier != "" {
				r.hearNow(tc.earlier, true)
			}
			r.hearNow(tc.current, true)
			sendDelegation(w, "after", 2000)
			if q := toolWithin(t, v, 2*time.Second); q != tc.want {
				t.Errorf("the tool was asked %q, want %q", q, tc.want)
			}
		})
	}
}
