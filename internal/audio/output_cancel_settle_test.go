package audio

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// idleHeldStream holds the mixer in its idle close's query of what the
// device still holds, and then reports nothing held, so the stream closes.
type idleHeldStream struct {
	*fakeStream
	barrier *probeBarrier
}

func (s *idleHeldStream) queuedFrames() int {
	s.barrier.hold()
	return 0
}

// TestACancelledDrainReturnsWhenTheStreamClosesIdle fails if Drain of a
// playback a Cancel ended waits on after the stream closed for being idle:
// the close ends the speech, which settles the Cancel, but woke no Drain,
// which then waited for audio that never came (Sol's review of #153 at
// 4b9830e). The Cancel comes while the mixer is in its idle close, after
// the input ran out of audio and before its speech ended.
func TestACancelledDrainReturnsWhenTheStreamClosesIdle(t *testing.T) {
	barrier := newProbeBarrier()
	stream := &idleHeldStream{newFakeStream(48000), barrier}
	closed := make(chan struct{})
	out := NewOutput(OutputConfig{
		IdleClose: time.Millisecond,
		Open:      func(string) (PlaybackStream, int, error) { return stream, 48000, nil },
		Closing:   func(closeStream func() error) { _ = closeStream(); close(closed) },
	})
	t.Cleanup(func() {
		barrier.unblock()
		_ = out.Close()
	})
	ctx := testContext(t)
	pb := out.Begin(InputVoice)
	if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	probeWait(t, barrier.entered, "the idle close's query of the device")
	out.Cancel(InputVoice)
	out.mu.Lock()
	settling := pb.gen.settling
	out.mu.Unlock()
	if !settling {
		t.Fatal("fixture: the Cancel did not wait for the speech it ended")
	}
	done := make(chan error, 1)
	go func() { done <- pb.Drain(ctx) }()
	waitFor(t, "Drain to return or wait", func() bool {
		out.mu.Lock()
		defer out.mu.Unlock()
		return len(done) > 0 || len(out.waiting) > 0
	})
	barrier.unblock()
	probeWait(t, closed, "the stream to close")
	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Drain: %v, want ErrCancelled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain still waits after the idle close ended the cancelled speech")
	}
}

// TestADrainOfAnEarlierCancelDoesNotWaitForALaterOne fails if Drain of a
// playback whose Cancel the mixer has taken in waits for a later Cancel of
// the same input, of a playback begun before the mixer took the first one
// in: it waited on the input's mark, not its own Cancel's (Sol's review of
// #153 at 4b9830e: context deadline exceeded). The control drains the first
// playback once its Cancel is taken in.
func TestADrainOfAnEarlierCancelDoesNotWaitForALaterOne(t *testing.T) {
	stream := &probeStream{fakeStream: newFakeStream(48000)}
	out, start, events := probeOutput(t, stream)
	first, second := newProbeBarrier(), newProbeBarrier()
	t.Cleanup(first.unblock)
	t.Cleanup(second.unblock)
	stream.before = func(n int, _ []int16) {
		switch n {
		case 2:
			first.hold()
		case 3:
			second.hold()
		}
	}
	ctx := testContext(t)
	a := out.Begin(InputVoice)
	if err := a.WriteAll(ctx, constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	start()
	probeWait(t, first.entered, "the first playback's second write")
	probeVoiceEvent(t, events, true)
	out.Cancel(InputVoice)
	b := out.Begin(InputVoice) // before the mixer takes the first Cancel in
	if err := b.WriteAll(ctx, constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
		t.Fatal(err)
	}
	first.unblock()
	probeVoiceEvent(t, events, false) // the first Cancel taken in
	probeWait(t, second.entered, "the second playback's first write")
	if err := a.Drain(ctx); !errors.Is(err, ErrCancelled) {
		t.Fatalf("control: the first playback's Drain: %v, want ErrCancelled", err)
	}
	out.Cancel(InputVoice)
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	err := a.Drain(bounded)
	second.unblock()
	if bErr := b.Drain(ctx); !errors.Is(bErr, ErrCancelled) {
		t.Fatalf("the second playback's Drain: %v, want ErrCancelled", bErr)
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("the first playback's Drain waited for the later Cancel: %v", err)
	}
}

// TestALaterCancelsDrainDoesNotWaitForAnEarlierSpeech fails if Drain of a
// playback begun after a Cancel the mixer has not yet taken in, and then
// cancelled, waits for that earlier speech's take-back: its playback played
// none of it, and its Drain waited until the held write returned (Sol's
// review of #153 at 80418b0: context deadline exceeded). The new playback
// is empty, or holds 20 ms the held mixer cannot play. The control has no
// earlier speech.
func TestALaterCancelsDrainDoesNotWaitForAnEarlierSpeech(t *testing.T) {
	for _, tc := range []struct {
		earlier bool
		queued  time.Duration
	}{{false, 0}, {true, 0}, {true, 20 * time.Millisecond}} {
		t.Run(fmt.Sprintf("an earlier speech %v, %v queued", tc.earlier, tc.queued), func(t *testing.T) {
			stream := &probeStream{fakeStream: newFakeStream(48000)}
			out, start, _ := probeOutput(t, stream)
			held := newProbeBarrier()
			t.Cleanup(held.unblock)
			if tc.earlier {
				stream.before = func(n int, _ []int16) {
					if n == 2 {
						held.hold()
					}
				}
				a := out.Begin(InputVoice)
				if err := a.WriteAll(testContext(t), constant(48000, 40*time.Millisecond, 1000), mono48k); err != nil {
					t.Fatal(err)
				}
				start()
				probeWait(t, held.entered, "the earlier speech's second write")
				out.Cancel(InputVoice)
			}
			b := out.Begin(InputVoice)
			if tc.queued > 0 {
				if err := b.WriteAll(testContext(t), constant(48000, tc.queued, 1000), mono48k); err != nil {
					t.Fatal(err)
				}
			}
			out.Cancel(InputVoice)
			ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer stop()
			err := b.Drain(ctx)
			if got := b.Played(); got != 0 {
				t.Fatalf("the later playback played %v, want none", got)
			}
			held.unblock()
			if !errors.Is(err, ErrCancelled) {
				t.Fatalf("the later playback's Drain: %v, want ErrCancelled at once", err)
			}
		})
	}
}
