package concierge

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// Sol's twenty-first review of #191 at 1948690
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6080583658),
// through the real Supervisor, Concierge and GPT-Live client on a loopback
// vendor. GPT-Live speaks here the way it does on the wire: audio deltas the
// real client plays through the Supervisor's speaker, so the client's own
// rule for a speech nothing explains is what decides (Main, after the review).

// vendorSpeaks has GPT-Live speak on the loopback wire, and returns once its
// speech has reached the Supervisor's speaker and ended there.
func vendorSpeaks(t *testing.T, w *vendorWire, r *cutRig) {
	t.Helper()
	pcm := make([]byte, 2*2400)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 4000)
	}
	w.send(map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)})
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.speaker.eventMu.Lock()
		active := r.speaker.active
		r.speaker.eventMu.Unlock()
		if active != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("GPT-Live's speech never reached the speaker")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.speaker.FlushSpeech() // the speech's start reaches the Supervisor
	r.speaker.eventMu.Lock()
	owner := r.speaker.active.owner
	r.speaker.active = nil
	r.speaker.queue = append(r.speaker.queue, audio.SpeechEvent{Input: audio.InputVoice, Owner: owner, At: time.Now()})
	r.speaker.eventMu.Unlock()
	r.speaker.FlushSpeech() // and its end
	// More than the client's speech gap (400 ms): GPT-Live sends no end of
	// speech, so its client ends one only once the audio has paused that
	// long, and the next delta begins a new speech.
	time.Sleep(600 * time.Millisecond)
}

// openVoiceBOn is openVoiceB with its own opening question, said in one
// final that ends its utterance.
func openVoiceBOn(t *testing.T, opening string) (*cutRig, *watchedVoice, *vendorWire) {
	t.Helper()
	url, conns := loopbackVendor(t, false)
	live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
	if err != nil {
		t.Fatal(err)
	}
	v := watch(live)
	r := openingRig(t, v)
	heldIssue(r.rig, "session", false)
	r.model.turns = answers(20)
	r.hearNow("Wispr, "+opening, true)
	w := within(t, conns, "vendor connection")
	w.next(t, "session.start")
	within(t, v.opened, "opened voice")
	return r, v, w
}

// TestATurnGPTLiveAnsweredByItselfLeavesTheRequest (finding 1, Sol's
// TestSol191R21AnsweredWithoutDelegation and
// TestSol191R21DirectRepeatReachesHolderAgain): GPT-Live answers a question
// itself, with no delegation, and then the room asks another that it does
// delegate. At 1948690 the answered question was kept pending, so the tool,
// the model and the holder got "what time is it? ask ABC-12 if it is
// merged?". A speech that begins with no delegation open and nothing the
// client sent to explain it answered what was said before it, and those
// finals leave the request; they stay in the history. Controls: the earlier
// question delegated and answered through the backend, and a repeat of an
// answered question that GPT-Live answers itself.
func TestATurnGPTLiveAnsweredByItselfLeavesTheRequest(t *testing.T) {
	const next = "ask ABC-12 if it is merged?"
	for _, tc := range []struct {
		name, opening       string
		delegated, repeated bool
	}{
		{"answered by itself", "what time is it?", false, false},
		{"control: delegated and answered", "what time is it?", true, false},
		{"a repeat answered by itself", "when will ABC-12 ship?", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, v, w := openVoiceBOn(t, tc.opening)
			if tc.delegated {
				sendDelegation(w, "initial", 1000)
				if q := toolWithin(t, v, 2*time.Second); q != tc.opening {
					t.Fatalf("control: the opening was asked as %q", q)
				}
			}
			vendorSpeaks(t, w, r) // the opening's answer
			if tc.repeated {
				r.hearNow(tc.opening, true)
				vendorSpeaks(t, w, r) // GPT-Live answers the repeat from its earlier result
			}
			r.hearNow(next, true)
			sendDelegation(w, "next", 3000)
			if q := toolWithin(t, v, 2*time.Second); q != next {
				t.Errorf("the tool was asked %q, want only %q", q, next)
			}
			for _, m := range holderMessages(r) {
				if strings.Contains(strings.ToLower(m), "merged") && strings.Contains(strings.ToLower(m), strings.ToLower(tc.opening)) {
					t.Errorf("the holder got the answered question with the new one: %q", m)
				}
			}
		})
	}
}

// TestASpeechTheClientCausedRetiresNothing (the three orders Main ruled on
// after Sol's twenty-first review): a speech whose cause the client knows is
// no direct answer. Here the answer to a delegation: the room asks a follow-up
// while the backend works on the first question; the first answer is
// delivered, which closes that delegation, so none is open when GPT-Live
// speaks it; and the follow-up is still asked. Retiring at every speech with
// no delegation open would lose it there.
func TestASpeechTheClientCausedRetiresNothing(t *testing.T) {
	r, v, w := openVoiceBOn(t, "what about ABC-42?")
	sendDelegation(w, "initial", 1000)
	if q := toolWithin(t, v, 2*time.Second); q != "what about ABC-42?" {
		t.Fatalf("control: the opening was asked as %q", q)
	}
	r.hearNow("and who holds it?", true) // said while the first answer was on its way
	w.next(t, "session.commentary.append")
	// The result reached the vendor; its acknowledgement closes the
	// delegation, so no delegation is open when GPT-Live speaks it.
	time.Sleep(150 * time.Millisecond)
	vendorSpeaks(t, w, r) // the first answer, the delegation's result
	sendDelegation(w, "followup", 2000)
	if q := toolWithin(t, v, 2*time.Second); q != "and who holds it?" {
		t.Errorf("the follow-up said before the delegation's answer was spoken reached the tool as %q", q)
	}
}

// TestASpeechBeforeTheDelegationRetiresItsQuestion (the risky order Main asked
// to test): GPT-Live speaks before it delegates the same question. Its speech
// had nothing to explain it, so the client takes it for a direct answer and the
// question leaves the request; the delegation that follows finds nothing to
// ask and waits, and the room's next question is asked under it, once. On the
// real vendor every run of 2026-11-26 delegated first and spoke about 500 ms
// after the delegation, so this order was not seen; when it happens and the
// speech was no answer, the question is not asked.
func TestASpeechBeforeTheDelegationRetiresItsQuestion(t *testing.T) {
	r, v, w := openVoiceBOn(t, "what about ABC-42?")
	vendorSpeaks(t, w, r) // GPT-Live speaks first
	sendDelegation(w, "late", 1000)
	if q := toolWithin(t, v, 800*time.Millisecond); q != "" {
		t.Fatalf("the question GPT-Live already spoke on was asked: %q", q)
	}
	r.hearNow("is ABC-12 merged?", true)
	if q := toolWithin(t, v, 2*time.Second); q != "is ABC-12 merged?" {
		t.Errorf("the room's next question reached the tool as %q, want it asked once", q)
	}
	if extra := toolWithin(t, v, 300*time.Millisecond); extra != "" {
		t.Errorf("a second tool call: %q", extra)
	}
}

// TestAClarificationLeavesTheQuestionInTheHistory (the other case Main named):
// GPT-Live asks its own clarification ("Do you mean its status or its
// holder?") without delegating. That retires the question, so the room's
// reply is asked alone, and the question must still be in the recent history
// both the backend's tool call and the Concierge's model read. The reply,
// "what is the status of ABC-12?", reaches the model; the question's own
// words "what about ABC-12?" are in neither the reply nor the request, so only
// the history can put them in its prompt.
func TestAClarificationLeavesTheQuestionInTheHistory(t *testing.T) {
	const question, reply = "what about ABC-12?", "what is the status of ABC-12?"
	r, v, w := openVoiceBOn(t, question)
	vendorSpeaks(t, w, r) // GPT-Live's own clarification
	r.hearNow(reply, true)
	sendDelegation(w, "reply", 1000)
	call := within(t, v.calls, "the reply's tool call")
	var args voice.AnswerArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Question != reply {
		t.Fatalf("the reply reached the tool as %q, want it alone", args.Question)
	}
	if !strings.Contains(args.Transcript, question) {
		t.Errorf("the question the reply answers left the backend's history:\n%s", args.Transcript)
	}
	result := within(t, v.results, "the reply's answer")
	prompt := lastPrompt(r.rig)
	if prompt == "" {
		t.Fatalf("control: the Concierge never called its model, so its history was not read; it answered %s, holders %q", result, holderMessages(r))
	}
	if !strings.Contains(prompt, question) {
		t.Errorf("the question the reply answers left the model's history:\n%s", prompt)
	}
}

// TestATakeBackBetweenArrivalAndReadEndsTheDelegationWithAReplacement
// (finding 2, Sol's TestSol191R21ArrivalTakebackThenFinalBeforeRead): the
// worker of a waiting delegation is paused before its first read; "never
// mind" and then "is ABC-12 merged?" both arrive before it reads. At 1948690
// the old delegation claimed and asked the later final. A take-back since the
// delegation arrived ends it whatever the room asks now. Control: with no
// take-back it asks that final.
func TestATakeBackBetweenArrivalAndReadEndsTheDelegationWithAReplacement(t *testing.T) {
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
			r.hearNow("Wispr, what about ABC-12?", false)
			w := within(t, conns, "vendor")
			w.next(t, "session.start")
			within(t, v.opened, "Open")
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-12?" {
				t.Fatal(q)
			}
			var once sync.Once
			defer once.Do(func() { close(gate.release) })
			gate.armWorker()
			sendDelegation(w, "waiting", 2000)
			within(t, gate.blocked, "the worker before its first read")
			if cancel {
				r.hearNow("never mind", false)
			}
			r.hearNow("is ABC-12 merged?", false) // before the worker reads
			once.Do(func() { close(gate.release) })
			q := toolWithin(t, v, 800*time.Millisecond)
			if cancel && q != "" {
				t.Errorf("the take-back did not end the delegation; it asked %q", q)
			}
			if !cancel && q != "is ABC-12 merged?" {
				t.Errorf("control: the waiting delegation asked %q", q)
			}
		})
	}
}

// TestAQuotationAcrossTurnsKeepsItsWords (finding 3, Sol's
// TestSol191R21QuotesAcrossTurns): `does ABC-12 show "Please`, `cancel`,
// `then retry" in the tooltip?`, each ending its utterance. At 1948690 each
// turn was parsed alone, so "cancel" took back the first and the tool was
// asked only the last. One parse over every pending final keeps the quotation
// whole. Controls: the same with no endpoints, and "retry" for "cancel".
func TestAQuotationAcrossTurnsKeepsItsWords(t *testing.T) {
	const first = `does ABC-12 show "Please`
	for _, endpoints := range []bool{true, false} {
		for _, word := range []string{"cancel", "retry"} {
			t.Run(fmt.Sprintf("endpoints_%v/%s", endpoints, word), func(t *testing.T) {
				r, v, w := openVoiceBOn(t, "what about ABC-12?")
				sendDelegation(w, "initial", 1000)
				if q := toolWithin(t, v, 2*time.Second); q != "what about ABC-12?" {
					t.Fatalf("control: the opening was asked as %q", q)
				}
				r.hearNow(first, endpoints)
				r.hearNow(word, endpoints)
				r.hearNow(`then retry" in the tooltip?`, true)
				sendDelegation(w, "quoted", 2000)
				want := first + " " + word + ` then retry" in the tooltip?`
				if q := toolWithin(t, v, 2*time.Second); q != want {
					t.Errorf("the tool was asked %q, want %q", q, want)
				}
			})
		}
	}
}

// TestALaterTakeBackRemovesAnUnaskedTurnFromTheHistory (finding 4, Sol's
// TestSol191R21LaterTurnTakebackHistory): "when will ABC-12 ship?" as a turn
// no delegation asks, then "never mind. is ABC-12 merged?". At 1948690 the
// request held only the merge question, but the model's history still held
// the withdrawn one. The history is cut by the same parse. Controls: with no
// take-back the earlier question is both asked and in the history; and a
// question already asked stays in the history after a later take-back.
func TestALaterTakeBackRemovesAnUnaskedTurnFromTheHistory(t *testing.T) {
	const prior = "when will ABC-12 ship?"
	for _, tc := range []struct {
		name          string
		cancel, asked bool
	}{
		{"the take-back withdraws it", true, false},
		{"control: no take-back", false, false},
		{"control: already asked, then a take-back", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, v, w := openVoiceBOn(t, "what about ABC-12?")
			sendDelegation(w, "initial", 1000)
			if q := toolWithin(t, v, 2*time.Second); q != "what about ABC-12?" {
				t.Fatalf("control: the opening was asked as %q", q)
			}
			r.hearNow(prior, true)
			if tc.asked {
				sendDelegation(w, "prior", 2000)
				if q := toolWithin(t, v, 2*time.Second); q != prior {
					t.Fatalf("control: the earlier question was asked as %q", q)
				}
			}
			later := "is ABC-12 merged?"
			if tc.cancel {
				later = "never mind. " + later
			}
			r.hearNow(later, true)
			sendDelegation(w, "after", 3000)
			q := toolWithin(t, v, 2*time.Second)
			prompt := lastPrompt(r.rig)
			switch {
			case tc.cancel && !tc.asked:
				if strings.Contains(prompt, prior) {
					t.Errorf("the withdrawn question still reaches the model:\n%s", prompt)
				}
			case !tc.cancel:
				if !strings.Contains(q, prior) || !strings.Contains(prompt, prior) {
					t.Errorf("control: the earlier question was asked as %q, history:\n%s", q, prompt)
				}
			case tc.asked:
				if !strings.Contains(prompt, prior) {
					t.Errorf("control: an asked question left the history on a later take-back:\n%s", prompt)
				}
			}
		})
	}
}

// claimGate pauses the first claim it is armed for, after it succeeds and
// before its tool call, as scheduling can.
type claimGate struct {
	inner   voice.Voice
	mu      sync.Mutex
	armed   bool
	claimed chan struct{}
	release chan struct{}
}

func (*claimGate) DrivesTurn() bool { return true }

func (g *claimGate) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	y := *x
	y.Claim = func(turn, revision uint64) bool {
		won := x.Claim(turn, revision)
		g.mu.Lock()
		hold := won && g.armed
		g.armed = g.armed && !hold
		g.mu.Unlock()
		if hold {
			close(g.claimed)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		}
		return won
	}
	return g.inner.Open(ctx, &y)
}

// TestAClaimedRequestIsAnsweredAsCommitted (Sol's
// TestSol191R21CommittedConsumerIsolation, which Sol's twenty-first review
// found failing at 1948690 and its parent alike): a delegation claims "when
// will ABC-12 ship?", and "never mind. is ABC-12 merged?" lands before its
// tool call runs. The handoff gave the Concierge the room's current turn, so
// its model was asked the merge question for the claimed call and then again
// for the next. A claimed call is answered as committed. Control: with no
// take-back the later turn is no replacement, and both are asked as said.
func TestAClaimedRequestIsAnsweredAsCommitted(t *testing.T) {
	const first, second = "when will ABC-12 ship?", "is ABC-12 merged?"
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("later_takeback_%v", cancel), func(t *testing.T) {
			url, conns := loopbackVendor(t, false)
			live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			g := &claimGate{inner: live, claimed: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			defer once.Do(func() { close(g.release) })
			v := watch(g)
			r := openingRig(t, v)
			heldIssue(r.rig, "session", false)
			r.model.turns = answers(20)
			r.hearNow("Wispr, what about ABC-12?", true)
			w := within(t, conns, "vendor connection")
			w.next(t, "session.start")
			within(t, v.opened, "opened voice")
			if q, _ := delegate(t, w, v, r, "initial", 1000); q != "what about ABC-12?" {
				t.Fatalf("control: the opening was asked as %q", q)
			}
			r.hearNow(first, true)
			g.mu.Lock()
			g.armed = true
			g.mu.Unlock()
			sendDelegation(w, "committed", 2000)
			within(t, g.claimed, "the claim of the first question")
			later := second
			if cancel {
				later = "never mind. " + second
			}
			r.hearNow(later, true)
			once.Do(func() { close(g.release) })
			within(t, v.calls, "the claimed call")
			within(t, v.results, "the claimed call's answer")
			if got := modelQuestion(r); got != first {
				t.Errorf("the claimed call's model was asked %q, want the committed %q", got, first)
			}
			sendDelegation(w, "next", 3000)
			if q := toolWithin(t, v, 2*time.Second); q != second {
				t.Errorf("the next delegation asked %q, want %q", q, second)
			}
			if got := modelQuestion(r); got != second {
				t.Errorf("the next call's model was asked %q, want %q", got, second)
			}
		})
	}
}
