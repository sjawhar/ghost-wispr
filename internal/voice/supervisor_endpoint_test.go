package voice

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
)

// Sol's fourth review of #191 at 5121fa9c
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6053113051):
// an endpoint ended whichever turn was current when it came. Main's rule: an
// endpoint ends only a turn none of whose words began after the utterance it
// ends; words that began after it stay in the open turn.

// spoken is a result of text, final or not, ending its utterance or not, its
// words captured 0.2 s apart from t0, each 0.15 s long.
func spoken(text string, final, speechFinal bool, t0 time.Time) *Transcript {
	tr := transcriptOf(text, 0, final, speechFinal, 0)
	for i := range tr.Words {
		tr.Words[i].At = t0.Add(time.Duration(i) * 200 * time.Millisecond)
	}
	return &tr
}

// endOf is when tr's last word ended, by capture.
func endOf(tr *Transcript) time.Time {
	last := tr.Words[len(tr.Words)-1]
	return last.At.Add(time.Duration((last.End - last.Start) * float64(time.Second)))
}

// deliver hands tr to s once its last word was said, as Deepgram sends it.
func deliver(s *Supervisor, tr *Transcript) {
	time.Sleep(time.Until(endOf(tr)) + 10*time.Millisecond)
	now := monoclock.Now()
	tr.At, tr.Mono = now.Wall, now.Mono
	s.OnTranscript(tr)
}

// TestALateUtteranceEndLeavesTheQuestionWhole fails if an UtteranceEnd that
// arrives after words of the turn that began after the utterance it ends
// hands the turn over: the question is cut where the asker paused, and the
// rest is lost. An UtteranceEnd for the turn's last words still ends it. The
// words were said seconds before their results come, within the rig's turn
// wait of each other.
func TestALateUtteranceEndLeavesTheQuestionWhole(t *testing.T) {
	t.Run("late, mid-sentence", func(t *testing.T) {
		c := newFakeConcierge("It ships tomorrow.")
		r := newRig(t, c)
		t0 := time.Now().Add(-4 * time.Second)
		first := spoken("Wispr, what is happening with", true, false, t0)
		deliver(r.s, first)
		rest := t0.Add(2 * time.Second) // after a pause Deepgram calls an utterance's end
		deliver(r.s, spoken("the realtime voice", false, false, rest))
		r.s.OnUtteranceEnd(0, true, endOf(first), monoclock.Now()) // late: for the words before the pause
		deliver(r.s, spoken("the realtime voice budget?", true, true, rest))
		if got := r.waitTurn(); got != "what is happening with the realtime voice budget?" {
			t.Errorf("the Concierge got %q, want the whole question", got)
		}
	})
	t.Run("control: for the turn's last words", func(t *testing.T) {
		c := newFakeConcierge("It ships tomorrow.")
		r := newRig(t, c)
		question := spoken("Wispr, what is happening with the budget", true, false, time.Now().Add(-2*time.Second))
		deliver(r.s, question)
		r.s.OnUtteranceEnd(0, true, endOf(question), monoclock.Now())
		if got := r.waitTurn(); got != "what is happening with the budget" {
			t.Errorf("the Concierge got %q, want the question the UtteranceEnd ended", got)
		}
		if signals := r.signals(); !slices.Equal(signals, []string{"utterance_end"}) {
			t.Errorf("the turn ended on %v, want the UtteranceEnd", signals)
		}
	})
}

// signals are the kinds of the turn_signal timing events so far.
func (r *rig) signals() []string {
	var kinds []string
	for _, l := range r.timingLines() {
		if l.event != "turn_signal" || len(l.kv) != 2 {
			continue
		}
		if kind, ok := l.kv[1].(string); ok {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// TestAnUtteranceEndThatCannotBePlacedEndsNoTurn fails if an UtteranceEnd
// whose last word's end could not be timed ends the asker's turn: it might
// be for an earlier utterance. The turn ends by its own speech_final or the
// turn wait instead.
func TestAnUtteranceEndThatCannotBePlacedEndsNoTurn(t *testing.T) {
	c := newFakeConcierge("It ships tomorrow.")
	r := newRig(t, c)
	deliver(r.s, spoken("Wispr, what is happening with the budget", true, false, time.Now().Add(-2*time.Second)))
	r.s.OnUtteranceEnd(0, true, time.Time{}, monoclock.Now())
	if got := r.waitTurn(); got != "what is happening with the budget" {
		t.Errorf("the Concierge got %q", got)
	}
	if signals := r.signals(); !slices.Equal(signals, []string{"turn_wait"}) {
		t.Errorf("the turn ended on %v, want the turn wait", signals)
	}
}

// askedBack opens an exchange with a voice that drives its own turn, whose
// answer asks the room a question ("Did you mean …?"), and returns once its
// speech ended and the follow-up window opened.
func askedBack(t *testing.T, c *fakeConcierge) (*rig, *fakeSession) {
	t.Helper()
	c.acks = []string{didYouMean}
	r := newDrivingRigWith(t, time.Minute, c)
	r.s.cfg.FollowUpWait = time.Minute
	r.heard("Wispr, what is happening with the voice work?", true, true)
	sess := r.waitSession()
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("the answer tool: %v", err)
	}
	<-c.asked
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the follow-up window opened after Wispr asked", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && r.s.ex.followUpTimer != nil && r.s.ex.askedBack
	})
	return r, sess
}

// heardTurns are the turn numbers of c's Heard calls whose words are part of
// words: a split statement's tail ("can wait") is too.
func heardTurns(c *fakeConcierge, words string) []uint64 {
	var turns []uint64
	calls := c.heardCalls()
	for i := range calls {
		if calls[i].words != "" && strings.Contains(words, calls[i].words) {
			turns = append(turns, calls[i].x.Turn)
		}
	}
	return turns
}

// TestADrivingTurnEndsOnlyAtItsOwnEndpoint fails if, for a voice that drives
// its own turn, an endpoint for words before a word of the turn ends the
// turn, so the rest of the same words begins a new turn under a new number
// (Exchange.Turn): an UtteranceEnd that comes late, mid-sentence, or a
// speech_final on a late final whose words ended before an interim already
// heard. On 5121fa9c a statement's tail, "can wait", became a turn of its
// own after Wispr's question, read as a question and sent to the holder
// (Sol's fourth review of #191).
func TestADrivingTurnEndsOnlyAtItsOwnEndpoint(t *testing.T) {
	t.Run("a late UtteranceEnd, mid-sentence", func(t *testing.T) {
		c := newFakeConcierge()
		r, _ := askedBack(t, c)
		t0 := time.Now()
		first := spoken("okay, the budget", true, false, t0)
		deliver(r.s, first)
		rest := t0.Add(2 * time.Second)
		deliver(r.s, spoken("can wait", false, false, rest))
		r.s.OnUtteranceEnd(0, true, endOf(first), monoclock.Now())
		deliver(r.s, spoken("can wait", true, true, rest))
		if turns := heardTurns(c, "okay, the budget can wait"); len(turns) < 2 || len(slices.Compact(slices.Clone(turns))) != 1 {
			t.Errorf("the statement's Heard calls carry turns %v, want one number", turns)
		}
	})
	t.Run("a late speech_final, after a later interim", func(t *testing.T) {
		c := newFakeConcierge()
		r, _ := askedBack(t, c)
		t0 := time.Now()
		prefix := spoken("okay, the budget", true, true, t0)
		tail := t0.Add(2 * time.Second)
		deliver(r.s, spoken("can wait", false, false, tail)) // a later socket's interim
		deliver(r.s, prefix)                                 // an earlier socket's final, ending its utterance
		deliver(r.s, spoken("can wait", true, true, tail))
		r.s.mu.Lock()
		heard := r.s.turnOf(r.s.ex)
		r.s.mu.Unlock()
		turns := heardTurns(c, "okay, the budget can wait")
		if heard != "okay, the budget can wait" || len(slices.Compact(slices.Clone(turns))) != 1 {
			t.Errorf("the turn heard is %q, its Heard calls carry turns %v; want the whole statement under one number", heard, turns)
		}
	})
	t.Run("control: an endpoint for the turn's last words", func(t *testing.T) {
		c := newFakeConcierge()
		r, _ := askedBack(t, c)
		t0 := time.Now()
		first := spoken("okay, the budget", true, false, t0)
		deliver(r.s, first)
		r.s.OnUtteranceEnd(0, true, endOf(first), monoclock.Now())
		deliver(r.s, spoken("what about the wake word", true, true, t0.Add(2*time.Second)))
		before, after := heardTurns(c, "okay, the budget"), heardTurns(c, "what about the wake word")
		if len(before) != 1 || len(after) != 1 || after[0] != before[0]+1 {
			t.Errorf("the two utterances carry turns %v and %v, want the second the next", before, after)
		}
	})
}

// TestTheAnswerToolKeepsTheVendorsReadingOfAReply fails if, after Wispr asked
// the room a question, the answer tool does not hand the Concierge the
// vendor's own text of the reply (Exchange.VendorReading) beside the room's
// words, or hands it with no question asked: the Concierge takes the reply
// for side talk when either reading is, the vendor's when it holds the
// room's words, so a reply the Supervisor's turn heard only the end of ("can
// wait", of "okay, the budget can wait") is not read as a question (Main's
// rule after Sol's fourth review of #191).
func TestTheAnswerToolKeepsTheVendorsReadingOfAReply(t *testing.T) {
	for _, tc := range []struct {
		heard, vendor string
	}{
		{"can wait", "okay, the budget can wait"},
		{"okay, the budget can wait", "Can wait?"},
	} {
		t.Run(tc.heard+" / "+tc.vendor, func(t *testing.T) {
			c := newFakeConcierge() // its answer's last line is Wispr's question
			r, sess := askedBack(t, c)
			r.heard(tc.heard, true, true)
			args, _ := json.Marshal(AnswerArgs{Question: tc.vendor})
			if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d2", Name: AnswerTool, Args: args}); err != nil {
				t.Fatalf("the answer tool: %v", err)
			}
			if got := <-c.asked; got != tc.heard {
				t.Fatalf("the Concierge got %q, want the room's words %q", got, tc.heard)
			}
			c.mu.Lock()
			x := c.exchanges[len(c.exchanges)-1]
			c.mu.Unlock()
			if x.VendorReading != tc.vendor {
				t.Errorf("the vendor's reading is %q, want its text %q", x.VendorReading, tc.vendor)
			}
		})
	}
	t.Run("control: no question asked", func(t *testing.T) {
		c := newFakeConcierge("It ships tomorrow.")
		r := newDrivingRigWith(t, time.Minute, c)
		r.heard("Wispr, what is happening with the voice work?", true, true)
		sess := r.waitSession()
		args, _ := json.Marshal(AnswerArgs{Question: "What is happening with the voice work, and the budget?"})
		if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: args}); err != nil {
			t.Fatalf("the answer tool: %v", err)
		}
		<-c.asked
		c.mu.Lock()
		x := c.exchanges[len(c.exchanges)-1]
		c.mu.Unlock()
		if x.VendorReading != "" {
			t.Errorf("the vendor's reading is %q with no question of Wispr's, want none", x.VendorReading)
		}
	})
}
