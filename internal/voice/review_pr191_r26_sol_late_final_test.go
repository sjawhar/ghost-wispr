// Sol's twenty-sixth review of #191 at b7ca361
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6088261337):
// Sol's probe, kept as a regression test: an older misheard final and a
// newer question, with only its imports grouped as the linter asks.

package voice

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

func TestSolR26OlderMisheardFinalDoesNotWithdrawNewerQuestion(t *testing.T) {
	c := newFakeConcierge()
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait = time.Hour })
	captured := time.Now().Add(-20 * time.Second)
	hear := func(text string, start float64, final, speechFinal bool, speaker int) Transcript {
		tr := transcriptOf(text, start, final, speechFinal, 0)
		for i := range tr.Words {
			tr.Words[i].Speaker = speaker
			tr.Words[i].At = captured.Add(time.Duration(start * float64(time.Second))).Add(time.Duration(i) * 200 * time.Millisecond)
		}
		r.s.OnTranscript(&tr)
		return tr
	}
	hear("Whisper", 0, false, false, 3)
	sess := r.waitSession()
	hear("Whisper. No. Oh, yeah,", 0, true, true, 3)
	r24HeldExchange(t, r)
	hear("Wispr.", 5, false, false, 4)
	question := hear("Can you send these notes?", 10, false, false, 4)
	r.s.mu.Lock()
	ex := r.s.ex
	before := r.s.turnOf(ex)
	r.s.mu.Unlock()
	if before != "Can you send these notes?" {
		t.Fatalf("control: real interim not pending: %q", before)
	}
	late := hear("Whisper networks are a problem.", 5, true, true, 9)
	if !late.endsAt().Before(question.Words[0].At) || !question.endsAt().Before(time.Now()) {
		t.Fatal("control: old final is not wholly before the newer question in past capture time")
	}
	r.s.mu.Lock()
	turn, asker := r.s.turnOf(ex), ex.x.AskerTurn
	r.s.mu.Unlock()
	if turn != "Can you send these notes?" || asker != turn {
		t.Errorf("older final withdrew a later real interim: turn=%q AskerTurn=%q room=%q", turn, asker, sess.x.Room().Asker)
	}
	r.s.OnUtteranceEnd(0, true, question.endsAt(), monoclock.Now())
	if got := r.waitTurn(); got != "Can you send these notes?" {
		t.Fatalf("later question=%q", got)
	}
}
