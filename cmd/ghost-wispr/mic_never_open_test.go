package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// The rule: the microphone is never open while the blink(1) is off. These
// tests cover the states ReviewPR77 found on #77 (and on main) where it was:
// the wait after a stream error, no_audio, the order of a pause's close and
// the light, and a light turned off from outside Ghost Wispr.

// lightMic is a capture device for the mic loop that the light reads as open
// (the rig's microphone) from each open until it is closed. Stream delivers
// no audio and returns once Close is called. Close waits closeDelay, records
// the light's operations as it closes, then marks the device closed.
type lightMic struct {
	r          *lightRig
	closeDelay time.Duration
	atClose    chan []lightOp
	mu         sync.Mutex
	closed     chan struct{}
}

func newLightMic(r *lightRig, closeDelay time.Duration) *lightMic {
	m := &lightMic{r: r, closeDelay: closeDelay, atClose: make(chan []lightOp, 8), closed: make(chan struct{})}
	r.mic.open.Store(true)
	return m
}

func (m *lightMic) open() error {
	m.mu.Lock()
	m.closed = make(chan struct{})
	m.mu.Unlock()
	m.r.mic.open.Store(true)
	return nil
}

func (m *lightMic) Open() error   { return m.open() }
func (m *lightMic) Reopen() error { return m.open() }
func (m *lightMic) Start() error  { return nil }

func (m *lightMic) Close() error {
	time.Sleep(m.closeDelay)
	select {
	case m.atClose <- m.r.device.snapshot():
	default:
	}
	m.r.mic.open.Store(false)
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}

func (m *lightMic) Stream(io.Writer) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	<-closed
	return errors.New("stream closed")
}

// openMicUnderRed opens the rig's microphone as the app does, under a red
// confirmation, and waits until the light reads red.
func openMicUnderRed(t *testing.T, r *lightRig) {
	t.Helper()
	if err := r.capture.openUnderRed(func() error { r.mic.open.Store(true); return nil }); err != nil {
		t.Fatalf("open the microphone: %v", err)
	}
	r.waitFor(t, "the light red while the microphone is open", func(ops []lightOp) bool {
		r.hardware.Wake()
		return len(ops) > 0 && isRedReadBack(ops[len(ops)-1])
	})
}

// runLightMicLoop runs the mic loop on mic until the test ends; its waits
// block until then.
func runLightMicLoop(t *testing.T, r *lightRig, mic micStreamer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := goDone(func() {
		streamMicWithRetry(ctx, r.capture, mic, io.Discard, func(time.Duration) { <-ctx.Done() }, nopLogf, nil)
	})
	t.Cleanup(func() {
		cancel()
		_ = mic.Close()
		if !returnedWithin(done, testWait) {
			t.Error("mic loop did not stop")
		}
	})
}

// requireLitAtClose fails unless the microphone closed within testWait,
// with the light red as it closed: its last operation, a write or a
// read-back, shows red.
func requireLitAtClose(t *testing.T, mic *lightMic, what string) {
	t.Helper()
	select {
	case ops := <-mic.atClose:
		if n := len(ops); n == 0 || !isRedOp(ops[n-1]) {
			t.Fatalf("the light went off before the microphone closed %s: operations as it closed %v", what, ops)
		}
	case <-time.After(testWait):
		t.Fatalf("the mic loop did not close the microphone %s", what)
	}
}

// TestAPauseClosesTheMicrophoneBeforeTheLightGoesOff fails if a pause turns
// the light off before the mic loop has closed the microphone (R2 asks for
// close, then light), or does not turn it off once it has.
func TestAPauseClosesTheMicrophoneBeforeTheLightGoesOff(t *testing.T) {
	r := startLightRig(t, false)
	openMicUnderRed(t, r)
	// The close takes 200 ms: long enough for the light controller, which the
	// pause wakes, to act before it if it may.
	mic := newLightMic(r, 200*time.Millisecond)
	runLightMicLoop(t, r, mic)

	pauseCapture(r.capture, r.hardware)
	requireLitAtClose(t, mic, "for the pause")
	if off, ops := r.lightOffWithin(lightOffBound); !off {
		t.Fatalf("the light was still red %v after the microphone closed for the pause: operations %v", lightOffBound, ops)
	}
}

// TestNoAudioClosesTheMicrophoneBeforeTheLightGoesOff fails if the light goes
// off, once no audio reaches the recording (no_audio), while the microphone
// stays open, as it did until audio came back; or if the mic loop does not
// close the microphone then.
func TestNoAudioClosesTheMicrophoneBeforeTheLightGoesOff(t *testing.T) {
	r := startLightRig(t, false)
	openMicUnderRed(t, r)
	// No audio has reached the recording for longer than the stall limit.
	r.capture.audioAt.Store(time.Now().Add(-2 * audioStallAfter).UnixNano())
	mic := newLightMic(r, 200*time.Millisecond)
	runLightMicLoop(t, r, mic)
	r.hardware.Wake()

	requireLitAtClose(t, mic, "once no audio reached the recording")
	if off, ops := r.lightOffWithin(lightOffBound); !off {
		t.Fatalf("the light was still red %v after the microphone closed for no audio: operations %v", lightOffBound, ops)
	}
	if got := r.capture.captureState(r.mic); got != captureNoAudio {
		t.Fatalf("capture state with the microphone closed for no audio = %q, want %q", got, captureNoAudio)
	}
}

// TestAStreamErrorClosesTheMicrophoneBeforeTheWait fails if the mic loop
// leaves the microphone open while it waits after a stream error: the light
// goes off once audio stops reaching the recording, and the wait lasts 250 ms
// after an overflow and up to 30 s after a device error.
func TestAStreamErrorClosesTheMicrophoneBeforeTheWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "device error", err: errors.New("device disconnected")},
		{name: "overflow", err: errors.New("Input overflowed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mic := newScriptedMic()
			mic.streamErrs = []error{tc.err}
			waited := make(chan []string, 1)
			var seen []string
			waits := &recordedWaits{then: func() {
				for {
					select {
					case event := <-mic.events:
						seen = append(seen, event)
						continue
					default:
					}
					break
				}
				select {
				case waited <- slices.Clone(seen):
				default:
				}
			}}
			runMicLoop(t, newCaptureGate(nil, nil, nil), mic, io.Discard, waits.wait, nil)
			select {
			case calls := <-waited:
				if !slices.Contains(calls, "close") {
					t.Fatalf("device calls before the wait after a %s = %v, want a close: the microphone stayed open through the wait", tc.name, calls)
				}
			case <-time.After(testWait):
				t.Fatal("the mic loop did not wait after the stream error")
			}
		})
	}
}

// TestAudioStopsAtOnceWhenTheLightGoesOffFromOutside fails if audio reaches
// the recording after the light went off outside Ghost Wispr (another
// `ghost-wispr light-off`, an unplug, the blink(1)'s watchdog) until the light
// controller's next look, up to a second later.
func TestAudioStopsAtOnceWhenTheLightGoesOffFromOutside(t *testing.T) {
	r := startLightRig(t, false)
	openMicUnderRed(t, r)
	recorded := &lockedBuffer{}
	writer := r.capture.Writer(recorded)
	if _, err := writer.Write([]byte{1, 2, 3, 4}); err != nil || recorded.Len() != 4 {
		t.Fatalf("control: a write with the light red = %v, %d bytes recorded; want it recorded", err, recorded.Len())
	}

	r.device.mu.Lock()
	r.device.colour = [3]uint8{} // the light goes off outside Ghost Wispr
	r.device.mu.Unlock()
	if _, err := writer.Write([]byte{5, 6, 7, 8}); !errors.Is(err, errLightWentOff) {
		t.Fatalf("write once the light went off from outside = %v, want errLightWentOff", err)
	}
	if got := recorded.Len(); got != 4 {
		t.Fatalf("%d bytes recorded, want 4: audio passed after the light went off from outside", got)
	}
}
