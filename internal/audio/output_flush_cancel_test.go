package audio

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldUnderrunStream holds its first write until release closes, having
// closed entered, and reports an underrun from then on, so the Output's
// Underrun callback runs after that write returned and before the mixer
// records it.
type heldUnderrunStream struct {
	writes           int // only the mixer touches it
	entered, release chan struct{}
}

func (s *heldUnderrunStream) Write([]int16) error {
	if s.writes++; s.writes == 1 {
		close(s.entered)
		<-s.release
	}
	return nil
}

func (s *heldUnderrunStream) underruns() int   { return min(s.writes, 1) }
func (*heldUnderrunStream) Drop() (int, error) { return 0, nil }
func (*heldUnderrunStream) Close() error       { return nil }

// TestASecondCancelKeepsTheFirstCancelsTime fails if a second Cancel, after
// the stream took a speech's first frame, replaces the time of a first
// Cancel that came while that write was held: the speech then reports a
// start and an end, though the first Cancel cut it before the stream took
// its audio (Sol's review of #133 at 51143e4: every other test passed with
// cancelAt set on every Cancel). Controls: one Cancel while the write is
// held reports nothing; one after the write returned reports a start and a
// cancelled end.
func TestASecondCancelKeepsTheFirstCancelsTime(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after bool
		started       bool
	}{
		{"control: one Cancel while the first write is held", true, false, false},
		{"control: one Cancel after the first write returned", false, true, true},
		{"a Cancel while held and a second after the write returned", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &heldUnderrunStream{entered: make(chan struct{}), release: make(chan struct{})}
			barrier := newProbeBarrier()
			out := NewOutput(OutputConfig{
				Open:     func(string) (PlaybackStream, int, error) { return stream, 48000, nil },
				Underrun: barrier.hold,
			})
			t.Cleanup(func() { _ = out.Close() })
			t.Cleanup(barrier.unblock)
			release := sync.OnceFunc(func() { close(stream.release) })
			t.Cleanup(release)
			events := make(chan SpeechEvent, 16)
			defer out.OnSpeech(func(e SpeechEvent) { events <- e })()
			pb := out.Begin(InputVoice, nil)
			if err := pb.WriteAll(testContext(t), constant(48000, 100*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			probeWait(t, stream.entered, "the first write")
			if tc.before {
				out.Cancel(InputVoice)
			}
			release()
			probeWait(t, barrier.entered, "the Underrun callback after the first write returned")
			if tc.after {
				out.Cancel(InputVoice)
			}
			barrier.unblock()
			waitFor(t, "the mixer to take the Cancel in", func() bool {
				out.mu.Lock()
				defer out.mu.Unlock()
				return !pb.in.speaking
			})
			// Every event is emitted by now: read the ones delivered and the
			// ones still queued while no delivery is under way.
			out.deliverMu.Lock()
			var got []SpeechEvent
			for len(events) > 0 {
				got = append(got, <-events)
			}
			out.mu.Lock()
			got = append(got, out.events...)
			out.mu.Unlock()
			out.deliverMu.Unlock()
			if tc.started {
				if len(got) != 2 || !got[0].Started || got[1].Started || !got[1].Cancelled {
					t.Fatalf("control: events %+v, want a start and a cancelled end", got)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("a speech the first Cancel cut while its first write was held reported %+v", got)
			}
		})
	}
}

// unopenedOutput is an Output whose stream never opens: the tests that use
// it queue its events by hand.
func unopenedOutput(t *testing.T) *Output {
	t.Helper()
	out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) {
		return nil, 0, errors.New("no speaker in this test")
	}})
	t.Cleanup(func() { _ = out.Close() })
	return out
}

// TestFlushSpeechDeliversWhatIsQueued fails if Output.FlushSpeech returns
// before the listeners have had an event queued before the call, while the
// Output's own delivery has not run (Sol's review of #133 at 51143e4: the
// Supervisor's tests passed with FlushSpeech doing nothing).
func TestFlushSpeechDeliversWhatIsQueued(t *testing.T) {
	out := unopenedOutput(t)
	var got atomic.Int32
	defer out.OnSpeech(func(SpeechEvent) { got.Add(1) })()
	// Queued as emit queues it, but without waking the Output's own
	// delivery, so only FlushSpeech can hand it out.
	out.mu.Lock()
	out.events = append(out.events, SpeechEvent{Input: InputVoice, Started: true})
	out.mu.Unlock()
	if n := got.Load(); n != 0 {
		t.Fatalf("fixture: the Output's own delivery handed out %d events", n)
	}
	out.FlushSpeech()
	if n := got.Load(); n != 1 {
		t.Fatalf("FlushSpeech returned with %d of 1 queued events delivered", n)
	}
}

// TestFlushSpeechWaitsForADeliveryUnderWay fails if Output.FlushSpeech
// returns while the Output's own delivery of an earlier event is still in a
// listener, or hands a later event out first: deliveries take turns
// (deliverMu).
func TestFlushSpeechWaitsForADeliveryUnderWay(t *testing.T) {
	out := unopenedOutput(t)
	entered, release := make(chan struct{}), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letGo)
	var mu sync.Mutex
	var order []string
	var firstDone atomic.Bool
	defer out.OnSpeech(func(e SpeechEvent) {
		mu.Lock()
		order = append(order, e.Input)
		mu.Unlock()
		if e.Input == "first" {
			close(entered)
			<-release
			firstDone.Store(true)
		}
	})()
	out.mu.Lock()
	out.emit(&SpeechEvent{Input: "first", Started: true}) // the Output's own delivery takes it
	out.mu.Unlock()
	probeWait(t, entered, "the Output's own delivery of the first event")
	out.mu.Lock()
	out.events = append(out.events, SpeechEvent{Input: "second", Started: true}) // its own delivery is not woken
	out.mu.Unlock()
	flushed := make(chan bool, 1)
	go func() {
		out.FlushSpeech()
		flushed <- firstDone.Load()
	}()
	select {
	case done := <-flushed:
		t.Fatalf("FlushSpeech returned while the delivery of an earlier event was in its listener (finished %v)", done)
	case <-time.After(100 * time.Millisecond):
	}
	letGo()
	select {
	case done := <-flushed:
		if !done {
			t.Fatal("FlushSpeech returned before the earlier event's delivery finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FlushSpeech did not return once the earlier delivery finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"first", "second"}) {
		t.Fatalf("listeners had %v, want first then second", order)
	}
}
