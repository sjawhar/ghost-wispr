package tts

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// genOutput is the speaker output as audio.Output behaves: Cancel ends the
// playbacks begun before it, and a playback begun after Cancel plays. It
// counts, by text, the frames written to a playback begun after the cancel.
type genOutput struct {
	mu        sync.Mutex
	gen       int
	cancelled map[int]chan struct{}
	after     map[byte]int // frames written to a playback begun after a cancel
	written   map[byte]int
}

func newGenOutput() *genOutput {
	return &genOutput{cancelled: map[int]chan struct{}{0: make(chan struct{})}, after: map[byte]int{}, written: map[byte]int{}}
}

func (o *genOutput) Begin() voice.Playback {
	o.mu.Lock()
	defer o.mu.Unlock()
	return &genPlayback{out: o, gen: o.gen}
}

// cancel is the Supervisor's Speaker.Cancel(InputVoice).
func (o *genOutput) cancel() {
	o.mu.Lock()
	defer o.mu.Unlock()
	close(o.cancelled[o.gen])
	o.gen++
	o.cancelled[o.gen] = make(chan struct{})
}

type genPlayback struct {
	out *genOutput
	gen int
}

func (p *genPlayback) Write(ctx context.Context, frame []byte, _ audio.AudioFormat) error {
	o := p.out
	o.mu.Lock()
	done := o.cancelled[p.gen]
	o.mu.Unlock()
	select {
	case <-done:
		return audio.ErrCancelled
	default:
	}
	if err := ctx.Err(); err != nil { // as audio.Playback.Write since a5be0a6
		return err
	}
	o.mu.Lock()
	o.written[frame[0]]++
	if p.gen > 0 {
		o.after[frame[0]]++
	}
	o.mu.Unlock()
	// Real time: 20 ms frames, a 200 ms queue that is full once the first
	// sentence has played a while.
	select {
	case <-time.After(20 * time.Millisecond):
	case <-done:
		return audio.ErrCancelled
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (p *genPlayback) Drain(ctx context.Context) error { return nil }

// TestAStopCutsTheSentenceSynthesisedAhead: two sentences said as the
// Concierge says them, on the exchange's context; the second is synthesised
// while the first plays. Then the Supervisor's endLocked order for a stop:
// the exchange's context ends, the Output's voice input is cancelled, and the
// session's Interrupt runs on a goroutine of its own. No frame of the second
// sentence may reach the Output after the stop.
func TestAStopCutsTheSentenceSynthesisedAhead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stop      func(s voice.Session, out *genOutput, cancelExchange context.CancelFunc)
		wantAfter bool // the control that shows the count sees the second sentence
	}{
		{"control: no stop, the second sentence plays", nil, true},
		{"control: Interrupt before the Output's cancel", func(s voice.Session, out *genOutput, cancelExchange context.CancelFunc) {
			cancelExchange()
			s.Interrupt()
			out.cancel()
		}, false},
		{"treatment: endLocked's order, Interrupt on its own goroutine", func(s voice.Session, out *genOutput, cancelExchange context.CancelFunc) {
			cancelExchange()
			out.cancel()
			go s.Interrupt()
		}, false},
		{"treatment: endLocked's order, Interrupt 20 ms later", func(s voice.Session, out *genOutput, cancelExchange context.CancelFunc) {
			cancelExchange()
			out.cancel()
			go func() { time.Sleep(20 * time.Millisecond); s.Interrupt() }()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := newGenOutput()
			s := openSession(t, slowStart(50*time.Millisecond, 2*time.Second), out)
			exchange, cancelExchange := context.WithCancel(context.Background())
			defer cancelExchange()
			said := make(chan error, 2)
			go func() {
				for _, text := range []string{"a first sentence", "b second sentence"} {
					err := s.Say(exchange, text, voice.Fact)
					said <- err
					if err != nil {
						return
					}
				}
			}()
			// The first Say returns once its first frame is queued; the second
			// sentence is synthesised while the first plays, and its Say waits
			// for its turn to play.
			if err := <-said; err != nil {
				t.Fatalf("the first Say: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			if tc.stop != nil {
				tc.stop(s, out, cancelExchange)
			}
			time.Sleep(2500 * time.Millisecond)
			out.mu.Lock()
			after, b := out.after['b'], out.written['b']
			out.mu.Unlock()
			if tc.wantAfter {
				if b == 0 {
					t.Fatal("the count saw no frame of the second sentence")
				}
				return
			}
			if after > 0 {
				t.Fatalf("%d frames (%d ms) of the second sentence reached a playback begun after the stop", after, after*20)
			}
		})
	}
}

// TestASynthesisAfterAStopSendsNoText fails if a Say whose caller's ctx a
// stop already ended still sends its text to the vendor because its
// synthesis context, which follows the caller's through an AfterFunc on its
// own goroutine, has not ended yet.
func TestASynthesisAfterAStopSendsNoText(t *testing.T) {
	var streamed atomic.Int32
	v, err := New(fakeEngine{stream: func(context.Context, string, func([]byte) error) error {
		streamed.Add(1)
		return nil
	}}).Open(context.Background(), &voice.Exchange{Out: newFakeOutput()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	s := v.(*session)

	stopped := errors.New("the exchange stopped")
	caller, stop := context.WithCancelCause(context.Background())
	stop(stopped)
	wctx, cancelW := context.WithCancelCause(caller)
	defer cancelW(nil)
	// The synthesis context, whose AfterFunc has not run yet.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r := &sayRequest{ctx: ctx, wctx: wctx, caller: caller, cancel: func(error) {}, text: "after the stop", done: make(chan error, 1), more: make(chan struct{}, 1)}
	s.synthesise(r)
	if n := streamed.Load(); n != 0 {
		t.Fatalf("the vendor got %d texts after the stop, want none", n)
	}
	if err := <-r.done; !errors.Is(err, stopped) {
		t.Fatalf("the Say's result = %v, want the stop's cause", err)
	}
}
