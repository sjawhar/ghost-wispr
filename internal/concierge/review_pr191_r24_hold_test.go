// Sol's twenty-fourth review of #191 at 12c293e
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6084899163):
// Sol's probes, kept as regression tests, unchanged: the real question gate.

package concierge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// These are characterizations of the production question heuristic, not a
// claim that a grammatical question was addressed to Wispr. In particular,
// another person's asking side conversation is intentionally indistinguishable
// from a diarization change on the real request during the hesitation window.
func TestSolR24ActualAsksBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"control_plain_statement", "The budget can wait.", false},
		{"control_misheard_networks", "Whisper networks are a problem.", false},
		{"control_office_final", "Whisper. No. Oh, yeah,", false},
		{"imperatives_intentionally_excluded", "Make a dispatch issue and assign it to Raj.", false},
		{"imperative_side_talk", "Tell me about it.", false},
		{"question_side_talk", "Did you book our lunch?", true},
		{"question_without_mark", "Can you hand me the cable", true},
		{"addressed_to_somebody_else", "Alice, can you hand me the cable", true},
		{"office_question", "great. Can you send these notes to to whom?", true},
		{"clause_after_statement", "The budget can wait; who has the cable", true},
		{"question_word_inside_statement", "We know what the budget is.", false},
		{"bare_continuation", "to Raj on Dispatch.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Asks(tc.text); got != tc.want {
				t.Errorf("production Asks(%q)=%v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// Use a real initialized Concierge for its interface, with observation at the
// exact handover. Answer is held here rather than calling a model or sending to
// a holder. Thus these probes establish gate + Supervisor routing, not a real
// vendor/model/tool side effect. Nothing substitutes for production Asks.
type r24HoldCapture struct {
	*Concierge
	asked chan string
	mu    sync.Mutex
	heard []string
}

func (c *r24HoldCapture) Heard(_ context.Context, _ *voice.Exchange, words string) {
	c.mu.Lock()
	c.heard = append(c.heard, words)
	c.mu.Unlock()
}

func (c *r24HoldCapture) Answer(ctx context.Context, _ *voice.Exchange, turn string, _ voice.SayFunc) error {
	c.asked <- turn
	<-ctx.Done()
	return ctx.Err()
}

func (c *r24HoldCapture) heardCopy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.heard...)
}

type r24PublicHold struct {
	s       *voice.Supervisor
	voice   *r7Voice
	capture *r24HoldCapture
	start   float64
}

func r24OpenActualHold(t *testing.T, wait time.Duration) *r24PublicHold {
	t.Helper()
	base := newRig(t)
	t.Cleanup(base.concierge.Stop)
	r := &r24PublicHold{
		voice:   &r7Voice{},
		capture: &r24HoldCapture{Concierge: base.concierge, asked: make(chan string, 8)},
	}
	r.s = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:       func() (voice.Voice, error) { return r.voice, nil },
		Concierge:   r.capture,
		Speaker:     &r7Speaker{},
		WakePhrases: []string{"Wispr", "Ghost Wispr"},
		Session:     func() string { return "meeting-1" },
		Red:         func() bool { return true },
		Asks:        Asks,
		TurnWait:    wait,
		Logf:        t.Logf,
	})
	t.Cleanup(r.s.Close)
	r.hear("Whisper", false, false, 3)
	r7Until(t, "one live voice opened for the interim wake", func() bool {
		opened, closed := r.voice.counts()
		return opened == 1 && closed == 0
	})
	r.hear("Whisper. No. Oh, yeah,", true, true, 3)
	return r
}

func (r *r24PublicHold) hear(text string, final, endpoint bool, speaker int) {
	tr := voice.Transcript{Text: text, IsFinal: final, SpeechFinal: endpoint, GateOpen: true, At: time.Now()}
	for i, word := range strings.Fields(text) {
		start := r.start + 0.2*float64(i)
		tr.Words = append(tr.Words, voice.Word{Text: word, Speaker: speaker, Start: start, End: start + 0.15})
	}
	if final {
		r.start += 5
	}
	r.s.OnTranscript(&tr)
}

func r24ActualTurn(t *testing.T, c *r24HoldCapture) string {
	t.Helper()
	select {
	case turn := <-c.asked:
		return turn
	case <-time.After(r7Deadline):
		t.Fatal("no turn reached the Concierge handover")
		return ""
	}
}

// The question-without-'?' row would fail if this accidentally used the voice
// fixture's default question-mark-only gate. The rejected-final rows observe
// actual closure, so zero observed requests cannot masquerade as a stuck hold.
func TestSolR24HoldActualAsksRouting(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		accepted   bool
	}{
		{"control_plain_statement", "The budget can wait.", false},
		{"imperatives_intentionally_excluded", "Make a dispatch issue and assign it to Raj.", false},
		{"tell_me_about_it", "Tell me about it.", false},
		{"another_speakers_question", "Did you book our lunch?", true},
		{"another_speakers_question_without_mark", "Can you hand me the cable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := r24OpenActualHold(t, 500*time.Millisecond)
			r.hear(tc.text, true, true, 9) // not the wake's speaker number 3
			if tc.accepted {
				if got := r24ActualTurn(t, r.capture); got != tc.text {
					t.Fatalf("production gate's handover=%q, want %q", got, tc.text)
				}
				if opened, closed := r.voice.counts(); opened != 1 || closed != 0 {
					t.Errorf("accepted question changed exchange ownership: opened=%d closed=%d", opened, closed)
				}
				return
			}
			r7Until(t, "the rejected-final hold expired and closed its one voice", func() bool {
				opened, closed := r.voice.counts()
				return opened == 1 && closed == 1
			})
			select {
			case got := <-r.capture.asked:
				t.Errorf("production gate accepted non-question %q as %q", tc.text, got)
			default:
			}
			for _, heard := range r.capture.heardCopy() {
				if strings.Contains(heard, tc.text) || strings.Contains(heard, "Oh, yeah") {
					t.Errorf("rejected text reached Concierge.Heard: %q", heard)
				}
			}
		})
	}
}

// Accepting the first grammatical question across diarization labels is the
// chosen ambiguity. After it, non-question finals are ordinary continuations;
// only the preceding plain statement remains outside the explicit request.
func TestSolR24HoldActualAsksCapturesFollowingFinals(t *testing.T) {
	r := r24OpenActualHold(t, time.Minute)
	r.hear("The budget can wait.", true, true, 8)
	r.hear("Could you send these notes", true, false, 9)
	r.hear("to Raj on Dispatch.", true, false, 2)
	r.hear("Make an issue and assign it to Raj.", true, true, 4)
	const want = "Could you send these notes to Raj on Dispatch. Make an issue and assign it to Raj."
	if got := r24ActualTurn(t, r.capture); got != want {
		t.Errorf("actual gate's multi-final request=%q, want %q", got, want)
	}
	for _, heard := range r.capture.heardCopy() {
		if strings.Contains(heard, "budget can wait") || strings.Contains(heard, "Oh, yeah") {
			t.Errorf("pre-request words reached Concierge.Heard: %q", heard)
		}
	}
}

// Unlike interrogative side-talk capture, losing a continuation after an
// explicit second wake is not a necessary ambiguity of concierge.Asks. This
// uses that real gate and the ordinary turn timer: a rejected continuation
// leaves only the second wake's fragment for that timer to hand over.
func TestSolR24HoldActualGateAfterSecondWake(t *testing.T) {
	r := r24OpenActualHold(t, 500*time.Millisecond)
	r.hear("Wispr, send these notes", true, false, 4)
	r.hear("to Raj on Dispatch.", true, true, 4)
	if got := r24ActualTurn(t, r.capture); got != "send these notes to Raj on Dispatch." {
		t.Errorf("explicit second wake's actual-gate request=%q, want its complete continuation", got)
	}
}
