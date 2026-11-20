package tts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// pacedOutput plays at real time, as the speaker output does: a Write blocks
// while more than queueLimit of audio waits to play. It notes when each
// frame begins to play, by the text the frame's first byte names.
type pacedOutput struct {
	mu      sync.Mutex
	playEnd time.Time          // when the audio written so far ends playing
	starts  map[byte]time.Time // a text's first frame: when it begins to play
	ends    map[byte]time.Time // a text's last frame so far: when it ends playing
	pbs     []*pacedPlayback   // every playback begun
	cut     chan struct{}      // closed by cancelAll, as the Supervisor cancels the input
	written map[byte]int       // frames written, by text
	after   map[byte]time.Time // the last frame write, by text
	cutOnce sync.Once
}

const (
	queueLimit = 200 * time.Millisecond
	bytesPerMs = 48 // 24 kHz mono PCM16
)

func newPacedOutput() *pacedOutput {
	return &pacedOutput{starts: map[byte]time.Time{}, ends: map[byte]time.Time{}, written: map[byte]int{}, after: map[byte]time.Time{}, cut: make(chan struct{})}
}

func (o *pacedOutput) Begin() voice.Playback {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := &pacedPlayback{out: o}
	o.pbs = append(o.pbs, p)
	return p
}

func (o *pacedOutput) cancelAll() { o.cutOnce.Do(func() { close(o.cut) }) }

func (o *pacedOutput) framesOf(text byte) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.written[text]
}

type pacedPlayback struct{ out *pacedOutput }

func (p *pacedPlayback) Write(ctx context.Context, frame []byte, _ audio.AudioFormat) error {
	o := p.out
	select {
	case <-o.cut:
		return audio.ErrCancelled
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d := time.Duration(len(frame)/bytesPerMs) * time.Millisecond
	o.mu.Lock()
	now := time.Now()
	start := o.playEnd
	if start.Before(now) {
		start = now
	}
	o.playEnd = start.Add(d)
	text := frame[0]
	if _, ok := o.starts[text]; !ok {
		o.starts[text] = start
	}
	o.ends[text] = o.playEnd
	o.written[text]++
	o.after[text] = now
	wait := time.Until(o.playEnd.Add(-queueLimit))
	o.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-o.cut:
			return audio.ErrCancelled
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *pacedPlayback) Drain(ctx context.Context) error {
	p.out.mu.Lock()
	end := p.out.playEnd
	p.out.mu.Unlock()
	select {
	case <-time.After(time.Until(end)):
		return nil
	case <-p.out.cut:
		return audio.ErrCancelled
	case <-ctx.Done():
		return ctx.Err()
	}
}

// slowStart is an engine that takes latency to its first audio, then emits
// a text's whole audio at once: its bytes all hold the text's first letter.
func slowStart(latency time.Duration, length time.Duration) fakeEngine {
	return fakeEngine{stream: func(ctx context.Context, text string, emit func([]byte) error) error {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return ctx.Err()
		}
		pcm := make([]byte, int(length/time.Millisecond)*bytesPerMs)
		for i := range pcm {
			pcm[i] = text[0]
		}
		return emit(pcm)
	}}
}

// TestAQueuedSentencePlaysWithNoSynthesisGap fails if a sentence said right
// after the one before returned starts to play only once its synthesis ends,
// leaving the room a gap of silence; voice A synthesises it while the one
// before plays (GWConcierge's live R6 measure: a first-fact p90 of 4.53 s).
// The caller says one sentence at a time, as the Concierge does.
func TestAQueuedSentencePlaysWithNoSynthesisGap(t *testing.T) {
	out := newPacedOutput()
	s := openSession(t, slowStart(500*time.Millisecond, time.Second), out)
	for _, text := range []string{"a first sentence", "b second sentence"} {
		if err := s.Say(context.Background(), text, voice.Fact); err != nil {
			t.Fatalf("Say(%q): %v", text, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for out.framesOf('b') == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out.mu.Lock()
	gap := out.starts['b'].Sub(out.ends['a'])
	out.mu.Unlock()
	t.Logf("the gap between two queued sentences: %v", gap.Round(time.Millisecond))
	if gap > 50*time.Millisecond {
		t.Fatalf("the second sentence began %v after the first ended, want no synthesis gap", gap.Round(time.Millisecond))
	}
}

// TestInterruptDropsTheNextSentencesAudio fails if an Interrupt, or Close, or
// a barge-in (Interrupt and the Output's cancel) while one sentence plays
// lets the next sentence, synthesised ahead, reach the Output.
func TestInterruptDropsTheNextSentencesAudio(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(s voice.Session, out *pacedOutput)
	}{
		{"Interrupt", func(s voice.Session, _ *pacedOutput) { s.Interrupt() }},
		{"Close", func(s voice.Session, _ *pacedOutput) { _ = s.Close() }},
		{"barge-in", func(s voice.Session, out *pacedOutput) { s.Interrupt(); out.cancelAll() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := newPacedOutput()
			s := openSession(t, slowStart(100*time.Millisecond, 2*time.Second), out)
			said := make(chan error, 2)
			go func() {
				for _, text := range []string{"a first sentence", "b second sentence"} {
					err := s.Say(context.Background(), text, voice.Fact)
					said <- err
					if err != nil {
						return
					}
				}
			}()
			// The second sentence is synthesised ahead while the first plays.
			if err := <-said; err != nil {
				t.Fatalf("the first Say: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			if out.framesOf('b') != 0 {
				t.Fatal("the second sentence played before the first ended")
			}
			tc.stop(s, out)
			time.Sleep(2500 * time.Millisecond) // past the first sentence's end
			if n := out.framesOf('b'); n != 0 {
				t.Fatalf("%d frames of the next sentence reached the Output after %s", n, tc.name)
			}
		})
	}
}
