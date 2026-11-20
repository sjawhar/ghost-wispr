package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// stalledMic is a capture device whose read blocks: Stream delivers no frame
// and returns only once the device is released (Close), as a PortAudio read on
// a device that stopped delivering frames returns only when the stream is
// aborted or closed. Every device call is reported on events.
type stalledMic struct {
	events  chan string
	release chan struct{}
	once    sync.Once
}

func newStalledMic() *stalledMic {
	return &stalledMic{events: make(chan string, 32), release: make(chan struct{})}
}

func (m *stalledMic) Stream(io.Writer) error {
	m.events <- "stream"
	<-m.release
	return errors.New("stream closed while reading")
}

func (m *stalledMic) Start() error  { m.events <- "start"; return nil }
func (m *stalledMic) Open() error   { m.events <- "open"; return nil }
func (m *stalledMic) Reopen() error { m.events <- "reopen"; return nil }

func (m *stalledMic) Close() error {
	m.events <- "close"
	m.unblock()
	return nil
}

func (m *stalledMic) unblock() { m.once.Do(func() { close(m.release) }) }

// TestAPauseReleasesACaptureDeviceWhoseReadBlocks fails if a pause leaves the
// capture device held while its read blocks. On the office Pi (d89eedba,
// 2026-11-17) the capture device delivered no frame: the session's raw
// capture stayed at 0 bytes, and 2 s after the resume the light went off
// (no_audio). The pause at 18:12:02Z answered 204 and the status said paused,
// but the Jabra's microphone stayed open (/proc/asound/card0/pcm0c/sub0/status
// "state: RUNNING", fuser held by ghost-wispr) with the light off, until a
// restart at 18:12:41Z, which logged "microphone still reading 5s after
// shutdown began; leaving the device and PortAudio to the process exit". The
// device closed at 18:12:46.350Z (usbmon SET_INTERFACE alt 0). The mic loop
// reads the pause only from a write, and a stalled read never writes.
func TestAPauseReleasesACaptureDeviceWhoseReadBlocks(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newStalledMic()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamMicWithRetry(ctx, capture, mic, io.Discard, (&recordedWaits{}).wait, func(string, ...any) {}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		mic.unblock()
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("mic loop did not stop")
		}
	})
	select {
	case got := <-mic.events:
		if got != "stream" {
			t.Fatalf("first device call = %q, want stream", got)
		}
	case <-time.After(testWait):
		t.Fatal("the mic loop did not start reading")
	}

	capture.Pause()
	if !capture.IsPaused() {
		t.Fatal("control: the gate is not paused")
	}
	var calls []string
	deadline := time.After(time.Second)
	for {
		select {
		case call := <-mic.events:
			calls = append(calls, call)
			if call == "close" {
				return
			}
			continue
		case <-deadline:
		}
		break
	}
	t.Fatalf("a pause left a capture device whose read blocks held for 1 s: device calls after the pause %v, want a close", calls)
}

// lightOp is one operation on the blink(1), as usbmon showed them on the Pi:
// a colour write ('n') or a read-back ('r') and the colour it returned.
type lightOp struct {
	read   bool
	colour [3]uint8
}

func (o lightOp) String() string {
	verb := "write"
	if o.read {
		verb = "read"
	}
	return fmt.Sprintf("%s %02x%02x%02x", verb, o.colour[0], o.colour[1], o.colour[2])
}

// opsLight is a blink(1) that records every colour write and read-back of
// LED 1 in order. SetRGB writes both LEDs; SetLED2 writes LED 2 alone, and
// led2Writes records each such write apart from ops.
type opsLight struct {
	mu         sync.Mutex
	colour     [3]uint8
	ops        []lightOp
	led2       [3]uint8
	led2Writes [][3]uint8
}

func (l *opsLight) SetRGB(r, g, b uint8) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.colour = [3]uint8{r, g, b}
	l.led2 = l.colour
	l.ops = append(l.ops, lightOp{colour: l.colour})
	return nil
}

func (l *opsLight) SetLED2(r, g, b uint8) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.led2 = [3]uint8{r, g, b}
	l.led2Writes = append(l.led2Writes, l.led2)
	return nil
}

func (l *opsLight) ReadRGB() (uint8, uint8, uint8, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, lightOp{read: true, colour: l.colour})
	return l.colour[0], l.colour[1], l.colour[2], nil
}

func (*opsLight) ServerDown(time.Duration) error { return nil }
func (*opsLight) BlackPattern() error            { return nil }
func (*opsLight) Close() error                   { return nil }

func (l *opsLight) snapshot() []lightOp {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]lightOp(nil), l.ops...)
}

// secondLED returns LED 2's colour now and each SetLED2 write so far.
func (l *opsLight) secondLED() ([3]uint8, [][3]uint8) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.led2, append([][3]uint8(nil), l.led2Writes...)
}

// portAudioOpenTook is how long PortAudio took on the office Pi from the
// resume's last light write to the device open: 48.893 to 49.002, about
// 110 ms (usbmon, 2026-11-17 18:11Z).
const portAudioOpenTook = 110 * time.Millisecond

// TestAResumedMicrophoneOpensOnlyAfterTheLightReadsRed fails if, on the first
// resume of a unit that started paused, the input device opens while the
// blink(1)'s last operation is not a read-back of red. On the office Pi
// (d89eedba, 2026-11-17) usbmon showed this order at a resume through the API:
//
//	18:11:48.846Z blink(1) write ff0000   (the resume request's red check)
//	18:11:48.869Z blink(1) write ff0000   (the gate's red check)
//	18:11:48.893Z blink(1) write 000000   (the light controller, once both checks released their holds)
//	18:11:49.002Z Jabra SET_INTERFACE interface 2 alt 1: the microphone opens
//	18:11:49.003Z blink(1) write ff0000   (startOpenedMic's red check, after the open)
//
// startMicrophone calls start, which opens the input device (audio.NewMic)
// before startOpenedMic confirms red; and the gate's red hold ends when the
// gate opens, before that start, so the controller turns the light off in
// between. The microphone must never open unless the light last read red.
func TestAResumedMicrophoneOpensOnlyAfterTheLightReadsRed(t *testing.T) {
	device := &opsLight{}
	originalOpen, originalCheck, originalRetry := openBlink1, lightCheckEvery, lightRetryEvery
	openBlink1 = func(string, string) (indicator.Light, error) { return device, nil }
	lightCheckEvery = time.Hour
	lightRetryEvery = 5 * time.Millisecond
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause() // the unit started paused
	mic := &openMicFake{}
	warnings := newWarningsRegistry()
	lightCtx, stopLight := context.WithCancel(context.Background())
	hardware := startHardware(lightCtx, &config.Config{IndicatorLight: config.IndicatorLightBlink1}, capture, mic, warnings, server.NewHub())
	wireLightConsent(capture, hardware, warnings, func(err error) {
		t.Errorf("a light check refused the resume: %v", err)
	})

	opened := make(chan []lightOp, 1)
	var loops sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if !joinMicLoops(&loops, testWait) {
			t.Error("microphone start-up still running after shutdown")
		}
		hardware.Stop()
		stopLight()
		<-hardware.Done()
		openBlink1, lightCheckEvery, lightRetryEvery = originalOpen, originalCheck, originalRetry
	})
	// start stands in for startMicPipeline: PortAudio opens the device, which
	// takes portAudioOpenTook, then startOpenedMic confirms red and starts it.
	_, _ = startMicrophone(ctx, &loops, capture, alreadyBooted(), func(bool) error {
		time.Sleep(portAudioOpenTook)
		opened <- device.snapshot()
		mic.open.Store(true)
		return startOpenedMic(capture, &scriptedStartMic{}, func() { mic.open.Store(false) })
	}, func() {})

	if err := capture.RequestResume(); err != nil {
		t.Fatalf("resume refused: %v", err)
	}
	var ops []lightOp
	select {
	case ops = <-opened:
	case <-time.After(testWait):
		t.Fatal("the resume did not open the input device")
	}
	if len(ops) == 0 {
		t.Fatal("the input device opened before any light operation")
	}
	last := ops[len(ops)-1]
	if !last.read || last.colour != [3]uint8{255, 0, 0} {
		t.Fatalf("the input device opened after light operations %v: the last is %v, want a read-back of ff0000", ops, last)
	}
}

// scriptedStartMic is the opened microphone startOpenedMic starts.
type scriptedStartMic struct{}

func (*scriptedStartMic) Start() error { return nil }
func (*scriptedStartMic) Close() error { return nil }
