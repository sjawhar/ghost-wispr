// Sol's twenty-ninth review of #191 at 0a434b4
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6091648636):
// Sol's probe, kept as a regression test, through the real AsyncSender and
// ResilientClient capture clock. Changed only as the new CapturedAt needs (it
// reports whether it timed a point exactly, and the result's start carries
// that as FromExact), and with one row added, as Main asked: the clamp times
// only the older results' start, which then equals the newer result's exact
// start (the oldest audio the clock keeps); the control checks each row's
// exactness and, where the starts collide, that they do. Its rc.Close is
// deferred as the linter asks.

package voice

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/monoclock"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestSolR29ClampedResultIdentityDoesNotLoseNewerQuestion(t *testing.T) {
	for _, tc := range []struct {
		name                string
		chunks              int     // 250 ms capture buffers buffered
		nextStart           float64 // the newer result's start
		oldExact, nextExact bool    // whether the older and the newer result starts are timed exactly
		collide             bool    // whether the two starts are the same time
	}{
		{"retained_exact_clock", 48, 7, true, true, false},
		{"buffer_outlasts_clock", 2080, 7, false, false, true},
		{"clamp_on_one_side", 2056, 2, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const quarterBytes = 8000
			rc := transcribe.NewResilientClient(t.Context(), nil, transcribe.ResilientConfig{SampleRate: 16000, BufferSize: (tc.chunks + 1) * quarterBytes}, nil, nil)
			defer func() { _ = rc.Close() }()
			mic := transcribe.NewAsyncSender(t.Context(), rc, 64, nil)
			captured := time.Now().Add(-time.Hour)
			audio := make([]byte, quarterBytes)
			for i := range tc.chunks {
				mic.MarkRead(captured.Add(time.Duration(i) * 250 * time.Millisecond))
				if _, err := mic.Write(audio); err != nil {
					t.Fatal(err)
				}
				if (i+1)%16 == 0 {
					want := (i + 1) * quarterBytes
					eventually(t, "the actual resilient backlog", func() bool {
						rc.Mu.Lock()
						defer rc.Mu.Unlock()
						return rc.Buffer.Len() == want
					})
				}
			}
			c := newFakeConcierge()
			c.hold = true
			r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) { cfg.TurnWait, cfg.IdleWait = time.Hour, time.Hour })
			hear := func(text string, resultStart, wordStart float64, final bool) Transcript {
				tr := transcriptOf(text, wordStart, final, false, 0)
				var ok bool
				tr.From, tr.FromExact, ok = rc.CapturedAt(time.Duration(resultStart * float64(time.Second)))
				if !ok {
					t.Fatal("control: result start not captured")
				}
				for i := range tr.Words {
					tr.Words[i].Speaker = 4
					tr.Words[i].At, _, ok = rc.CapturedAt(time.Duration(tr.Words[i].Start * float64(time.Second)))
					if !ok {
						t.Fatal("control: word not captured")
					}
				}
				r.s.OnTranscript(&tr)
				return tr
			}
			call := hear("Wispr.", 0, 6, false)
			r.waitSession()
			next := hear("Can you send these notes?", tc.nextStart, 10, false)
			late := hear("Wispr, check the budget", 0, 6, true)
			if !late.endsAt().Before(next.Words[0].At) {
				t.Fatal("control: late final is not strictly before newer words")
			}
			if call.FromExact != tc.oldExact || late.FromExact != tc.oldExact || next.FromExact != tc.nextExact || tc.collide != call.From.Equal(next.From) {
				t.Fatalf("control: starts not as the row means: call %v (exact %v), late exact %v, next %v (exact %v)", call.From.Sub(captured), call.FromExact, late.FromExact, next.From.Sub(captured), next.FromExact)
			}
			if call.Words[0].At.Equal(next.Words[0].At) {
				t.Fatal("control: word timestamps also collided")
			}
			r.s.OnUtteranceEnd(0, true, next.endsAt(), monoclock.Now())
			got := r.waitTurn()
			t.Logf("buffer=%ds call.From=%v next.From=%v old-word=%v next-word=%v Answer=%q", tc.chunks/4, call.From.Sub(captured), next.From.Sub(captured), call.Words[0].At.Sub(captured), next.Words[0].At.Sub(captured), got)
			if want := "check the budget Can you send these notes?"; got != want {
				t.Fatalf("old final removed the newer question: Answer=%q, want %q", got, want)
			}
		})
	}
}
