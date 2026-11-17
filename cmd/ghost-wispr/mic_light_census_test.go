package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// The census of every path that opens a capture device: each must open it
// only while the blink(1)'s last operation is a read-back of red, and keep it
// so until the open has finished. The PortAudio scan opens capture streams on
// the Jabra, so it counts as an open: startMicPipeline's start runs it first,
// and Mic.Reopen runs it before it opens the stream again.
//
//	path                                   where                          test
//	start-up, unpaused                     startMicrophone (atBoot)       TestEveryMicrophoneStartOpensOnlyWhileTheLightReadsRed
//	first resume after a paused start      startMicrophone (deferred)     the same, and TestAResumedMicrophoneOpensOnlyAfterTheLightReadsRed
//	start-up retry, unpaused or resumed    retryMicStartup                TestEveryMicrophoneStartOpensOnlyWhileTheLightReadsRed
//	resume of a running mic loop           streamMicWithRetry, Open       TestEveryMicLoopOpenOpensOnlyWhileTheLightReadsRed
//	reconnect after an error or overflow   reopenMic, Reopen              the same
//	reopen after the light was not red     streamMicWithRetry, Open       TestNoAudioPassesOnceTheLightWasNotRedWhileTheMicrophoneWasOpen

// lightRig is the recorder's recording-light stack (startHardware,
// wireLightConsent) on a blink(1) that records its operations, with the
// microphone the light reads as open or closed.
type lightRig struct {
	device   *opsLight
	capture  *captureGate
	hardware *hardwareControl
	mic      *openMicFake
	warnings *warningsRegistry
}

// startLightRig starts the light stack, paused or not, and waits for the
// controller's first check, which leaves the light off: nothing records.
func startLightRig(t *testing.T, paused bool) *lightRig {
	t.Helper()
	device := &opsLight{}
	originalOpen, originalCheck, originalRetry := openBlink1, lightCheckEvery, lightRetryEvery
	openBlink1 = func(string, string) (indicator.Light, error) { return device, nil }
	lightCheckEvery = time.Hour
	lightRetryEvery = 5 * time.Millisecond
	r := &lightRig{device: device, capture: newCaptureGate(nil, nil, nil), mic: &openMicFake{}, warnings: newWarningsRegistry()}
	if paused {
		r.capture.Pause()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.hardware = startHardware(ctx, &config.Config{IndicatorLight: config.IndicatorLightBlink1}, r.capture, r.mic, r.warnings, server.NewHub())
	t.Cleanup(func() {
		r.hardware.Stop()
		cancel()
		<-r.hardware.Done()
		openBlink1, lightCheckEvery, lightRetryEvery = originalOpen, originalCheck, originalRetry
	})
	wireLightConsent(r.capture, r.hardware, r.warnings, func(err error) {
		t.Errorf("a light check refused the resume: %v", err)
	})
	r.waitFor(t, "the controller's first check to leave the light off", func(ops []lightOp) bool {
		return len(ops) > 0 && !isRedOp(ops[len(ops)-1])
	})
	return r
}

func isRedOp(op lightOp) bool { return op.colour == [3]uint8{255, 0, 0} }

func isRedReadBack(op lightOp) bool { return op.read && isRedOp(op) }

// waitFor waits until cond holds for the blink(1)'s operations.
func (r *lightRig) waitFor(t *testing.T, what string, cond func([]lightOp) bool) {
	t.Helper()
	waitForHardware(t, what, func() bool { return cond(r.device.snapshot()) })
}

// waitOff waits until the light's last operation shows it off: a write of off
// once it was red, or a read-back of off. It wakes the controller on each
// look, as the hub's status events do in the app: a fake device can change
// what the light should show just after one wake has been read.
func (r *lightRig) waitOff(t *testing.T) {
	t.Helper()
	r.waitFor(t, "the light off", func(ops []lightOp) bool {
		r.hardware.Wake()
		return len(ops) > 0 && !isRedOp(ops[len(ops)-1])
	})
}

// openSteps is what a capture open saw of the light: the blink(1)'s
// operations as it began, and once it had taken portAudioOpenTook.
type openSteps struct{ begin, end []lightOp }

// requireRed fails the test unless the light's last operation, as each step
// of the open saw it, is a read-back of red.
func requireRed(t *testing.T, what string, steps openSteps) {
	t.Helper()
	for _, step := range []struct {
		when string
		ops  []lightOp
	}{{"as it began", steps.begin}, {"once it had taken " + portAudioOpenTook.String(), steps.end}} {
		if len(step.ops) == 0 || !isRedReadBack(step.ops[len(step.ops)-1]) {
			t.Fatalf("%s %s with light operations %v, want the last a read-back of ff0000", what, step.when, step.ops)
		}
	}
}

// recordingStart stands in for a microphone start (startMicPipeline): its
// first step is the PortAudio scan, then it opens the input device, which
// takes portAudioOpenTook. It reports what each saw of the light.
func recordingStart(r *lightRig, steps chan<- openSteps) func() error {
	return func() error {
		begin := r.device.snapshot()
		time.Sleep(portAudioOpenTook)
		end := r.device.snapshot()
		r.mic.open.Store(true)
		steps <- openSteps{begin, end}
		return nil
	}
}

// TestEveryMicrophoneStartOpensOnlyWhileTheLightReadsRed fails if a start of
// the microphone, whose PortAudio scan and device open both open capture
// streams, begins while the blink(1)'s last operation is not a read-back of
// red, or if the light changes before the open has finished.
func TestEveryMicrophoneStartOpensOnlyWhileTheLightReadsRed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
		run    func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, r *lightRig, start func() error)
	}{
		{name: "start-up, unpaused", run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, r *lightRig, start func() error) {
			go func() {
				_, _ = startMicrophone(ctx, loops, r.capture, alreadyBooted(), func(bool) error { return start() }, func() { t.Error("the start-up retried, but no attempt failed") })
			}()
		}},
		{name: "first resume after a paused start", paused: true, run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, r *lightRig, start func() error) {
			_, _ = startMicrophone(ctx, loops, r.capture, alreadyBooted(), func(bool) error { return start() }, func() { t.Error("the start-up retried, but no attempt failed") })
			if err := r.capture.RequestResume(); err != nil {
				t.Fatalf("resume refused: %v", err)
			}
		}},
		{name: "start-up retry, unpaused", run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, r *lightRig, start func() error) {
			loops.Go(func() {
				retryMicStartup(ctx, time.Second, oneShotAfter(), r.capture, nil, start, func(err error) { t.Errorf("retry failed: %v", err) }, nil)
			})
		}},
		{name: "start-up retry, once resumed", paused: true, run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, r *lightRig, start func() error) {
			loops.Go(func() {
				retryMicStartup(ctx, time.Second, oneShotAfter(), r.capture, nil, start, func(err error) { t.Errorf("retry failed: %v", err) }, nil)
			})
			if err := r.capture.RequestResume(); err != nil {
				t.Fatalf("resume refused: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, tc.paused)
			var loops sync.WaitGroup
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancel()
				if !joinMicLoops(&loops, testWait) {
					t.Error("microphone start-up still running after shutdown")
				}
			})
			steps := make(chan openSteps, 1)
			tc.run(t, ctx, &loops, r, recordingStart(r, steps))
			select {
			case got := <-steps:
				requireRed(t, "the microphone start's scan and open ran", got)
			case <-time.After(testWait):
				t.Fatal("the microphone did not start")
			}
		})
	}
}

// censusMic is a capture device for the mic loop whose every open, Open for
// a resume or Reopen for a reconnect (which scans PortAudio again first),
// reports what it saw of the light, and opens the microphone the light reads.
// Stream returns the next error a test sends on fail, or blocks until Close.
type censusMic struct {
	r      *lightRig
	opened chan openSteps
	fail   chan error
	mu     sync.Mutex
	closed chan struct{}
}

func newCensusMic(r *lightRig) *censusMic {
	return &censusMic{r: r, opened: make(chan openSteps, 4), fail: make(chan error), closed: make(chan struct{})}
}

func (m *censusMic) open() error {
	begin := m.r.device.snapshot()
	time.Sleep(portAudioOpenTook)
	m.mu.Lock()
	m.closed = make(chan struct{})
	m.mu.Unlock()
	m.r.mic.open.Store(true)
	m.opened <- openSteps{begin, m.r.device.snapshot()}
	return nil
}

func (m *censusMic) Open() error   { return m.open() }
func (m *censusMic) Reopen() error { return m.open() }
func (m *censusMic) Start() error  { return nil }

func (m *censusMic) Close() error {
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

func (m *censusMic) Stream(io.Writer) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	select {
	case err := <-m.fail:
		m.r.mic.open.Store(false)
		return err
	case <-closed:
		return errors.New("stream closed")
	}
}

// TestEveryMicLoopOpenOpensOnlyWhileTheLightReadsRed fails if the mic loop
// opens its microphone, for a resume or to reconnect it after a device error
// or an overflow, while the blink(1)'s last operation is not a read-back of
// red, or if the light changes before the open has finished. Each case first
// lets the light go off, as it does while nothing records.
func TestEveryMicLoopOpenOpensOnlyWhileTheLightReadsRed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
		err    error // the stream error that makes the loop reconnect
	}{
		{name: "resume", paused: true},
		{name: "reconnect after a device error", err: errors.New("device disconnected")},
		{name: "reconnect after an overflow", err: errors.New("Input overflowed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, tc.paused)
			mic := newCensusMic(r)
			if !tc.paused {
				// The microphone opened at start-up and records.
				r.mic.open.Store(true)
				r.hardware.Wake()
				r.waitFor(t, "the light red while recording", func(ops []lightOp) bool {
					return len(ops) > 0 && isRedReadBack(ops[len(ops)-1])
				})
			}
			proceed := make(chan struct{})
			release := sync.OnceFunc(func() { close(proceed) })
			ctx, cancel := context.WithCancel(context.Background())
			done := goDone(func() {
				streamMicWithRetry(ctx, r.capture, mic, io.Discard, func(time.Duration) { <-proceed }, nopLogf, nil)
			})
			t.Cleanup(func() {
				cancel()
				release()
				_ = mic.Close()
				if !returnedWithin(done, testWait) {
					t.Error("mic loop did not stop")
				}
			})

			if tc.paused {
				// Resume only once the loop has closed the microphone for the
				// pause: a resume before the loop's first pass would leave it
				// streaming a microphone nobody opened, which production never
				// does (startMicMonitoring starts the loop on an open one).
				mic.mu.Lock()
				closed := mic.closed
				mic.mu.Unlock()
				select {
				case <-closed:
				case <-time.After(testWait):
					t.Fatal("the mic loop did not close the microphone for the pause")
				}
				r.waitOff(t)
				if err := r.capture.RequestResume(); err != nil {
					t.Fatalf("resume refused: %v", err)
				}
			} else {
				mic.fail <- tc.err
				// The light goes off while the loop waits to reconnect.
				r.waitOff(t)
				release()
			}
			select {
			case got := <-mic.opened:
				requireRed(t, "the mic loop's open ran", got)
			case <-time.After(testWait):
				t.Fatal("the mic loop did not open the microphone")
			}
		})
	}
}

// TestAPauseDuringAMicrophoneStartKeepsTheLightRedUntilTheStartEnds fails if a
// pause turns the light off while a microphone start, which opens capture
// streams, is still under way, or if the light stays on once it has ended. A
// pause took every red hold away (hardware.CaptureStopped), so the start went
// on opening the device with the light off.
func TestAPauseDuringAMicrophoneStartKeepsTheLightRedUntilTheStartEnds(t *testing.T) {
	r := startLightRig(t, false)
	inStart, proceed := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(release)
	var loops sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if !joinMicLoops(&loops, testWait) {
			t.Error("microphone start-up still running after shutdown")
		}
	})
	started := goDone(func() {
		_, _ = startMicrophone(ctx, &loops, r.capture, alreadyBooted(), func(bool) error {
			close(inStart)
			<-proceed
			return nil
		}, func() {})
	})
	select {
	case <-inStart:
	case <-time.After(testWait):
		t.Fatal("the microphone start did not begin")
	}

	pauseCapture(r.capture, r.hardware)
	time.Sleep(50 * time.Millisecond) // the controller's check after the pause's wake
	if ops := r.device.snapshot(); !isRedReadBack(ops[len(ops)-1]) {
		t.Fatalf("the light changed during the start after a pause: operations %v, want the last a read-back of ff0000", ops)
	}
	release()
	if !returnedWithin(started, testWait) {
		t.Fatal("the microphone start did not end")
	}
	r.waitFor(t, "the light off once the start ended", func(ops []lightOp) bool {
		return !ops[len(ops)-1].read && !isRedOp(ops[len(ops)-1])
	})
}

// TestNoAudioPassesOnceTheLightWasNotRedWhileTheMicrophoneWasOpen fails if
// audio reaches the recording after the light was not red at some point while
// the microphone was open: what the microphone holds then may have been
// captured with the light off. The loop must drop it, close the microphone,
// which drops the rest, and open it again under a red confirmation. The light
// can read back off by itself, as when its watchdog fires. (No audio no
// longer turns the light off with the microphone open: the loop closes the
// microphone first, TestNoAudioClosesTheMicrophoneBeforeTheLightGoesOff.)
func TestNoAudioPassesOnceTheLightWasNotRedWhileTheMicrophoneWasOpen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lightOff func(t *testing.T, r *lightRig)
	}{
		{name: "the light read back off by itself", lightOff: func(t *testing.T, r *lightRig) {
			r.device.mu.Lock()
			r.device.colour = [3]uint8{}
			r.device.mu.Unlock()
			r.hardware.Wake()
			r.waitFor(t, "the controller to find the light off and light it again", func(ops []lightOp) bool {
				n := len(ops)
				return n >= 3 && ops[n-3] == lightOp{read: true} && isRedReadBack(ops[n-1])
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, false)
			mic := newScriptedMic()
			r.mic.open.Store(true)
			if err := startMicIfUnpaused(r.capture, mic.Start); err != nil {
				t.Fatalf("start the microphone: %v", err)
			}
			mic.expectEvents(t, "start")
			recorded := &lockedBuffer{}
			runMicLoop(t, r.capture, mic, recorded, func(time.Duration) {}, nil)
			mic.expectEvents(t, "stream")
			mic.capture(t, []byte{1, 2, 3, 4})
			if got := recorded.Len(); got != 4 {
				t.Fatalf("control: %d bytes recorded with the light red, want 4", got)
			}

			tc.lightOff(t, r)
			select {
			case mic.chunks <- []byte{5, 6, 7, 8}:
			case <-time.After(testWait):
				t.Fatal("timed out handing a chunk to the mic loop")
			}
			if err := <-mic.written; !errors.Is(err, errLightWentOff) {
				t.Fatalf("write after the light was not red = %v, want errLightWentOff", err)
			}
			if got := recorded.Len(); got != 4 {
				t.Fatalf("%d bytes recorded, want 4: audio passed after the light was not red", got)
			}
			mic.expectEvents(t, "close", "open", "stream")
			mic.capture(t, []byte{9, 10, 11, 12})
			if got := recorded.Len(); got != 8 {
				t.Fatalf("%d bytes recorded once the microphone opened again, want 8", got)
			}
			if ops := r.device.snapshot(); !isRedOp(ops[len(ops)-1]) {
				t.Fatalf("audio recorded with light operations %v, want the light red", ops)
			}
		})
	}
}

// stuckMic is a capture device whose read blocks until release, and whose
// Close cannot release it while that read runs (audio.ErrMicStillReading), as
// a Mic whose Stream does not return in time. Every call is reported on
// events.
type stuckMic struct {
	events  chan string
	release chan struct{}
	reading atomic.Bool
}

func newStuckMic() *stuckMic {
	return &stuckMic{events: make(chan string, 32), release: make(chan struct{})}
}

func (m *stuckMic) Stream(io.Writer) error {
	m.reading.Store(true)
	m.events <- "stream"
	<-m.release
	m.reading.Store(false)
	return errors.New("read returned")
}

func (m *stuckMic) Close() error {
	if m.reading.Load() {
		m.events <- "close: still reading"
		return audio.ErrMicStillReading
	}
	m.events <- "close"
	return nil
}

func (m *stuckMic) Start() error  { m.events <- "start"; return nil }
func (m *stuckMic) Open() error   { m.events <- "open"; return nil }
func (m *stuckMic) Reopen() error { m.events <- "reopen"; return nil }

func (m *stuckMic) expectEvents(t *testing.T, want ...string) {
	t.Helper()
	for i, w := range want {
		select {
		case got := <-m.events:
			if got != w {
				t.Fatalf("device call %d = %q, want %q (want sequence %v)", i+1, got, w, want)
			}
		case <-time.After(testWait):
			t.Fatalf("timed out waiting for device call %d %q (want sequence %v)", i+1, w, want)
		}
	}
}

func micHeldWarned(t *testing.T, warnings *warningsRegistry) bool {
	t.Helper()
	return slices.ContainsFunc(statusWarnings(t, warnings), func(w string) bool {
		return strings.Contains(w, "Microphone still open after a pause")
	})
}

// TestShutdownReleasesACaptureDeviceWhoseReadBlocks fails if shutdown leaves
// a capture device whose read blocks open, so that the mic loop does not end.
// On the office Pi (2026-11-17) the restart after such a read logged
// "microphone still reading 5s after shutdown began; leaving the device and
// PortAudio to the process exit".
func TestShutdownReleasesACaptureDeviceWhoseReadBlocks(t *testing.T) {
	mic := newStalledMic()
	t.Cleanup(mic.unblock)
	var loops sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loops.Go(func() {
		streamMicWithRetry(ctx, newCaptureGate(nil, nil, nil), mic, io.Discard, func(time.Duration) {}, nopLogf, nil)
	})
	select {
	case got := <-mic.events:
		if got != "stream" {
			t.Fatalf("first device call = %q, want stream", got)
		}
	case <-time.After(testWait):
		t.Fatal("the mic loop did not start reading")
	}

	cancel()
	if !joinMicLoops(&loops, time.Second) {
		t.Fatal("the mic loop still runs 1 s after shutdown began, its device open")
	}
	select {
	case got := <-mic.events:
		if got != "close" {
			t.Fatalf("device call at shutdown = %q, want close", got)
		}
	default:
		t.Fatal("shutdown ended the mic loop without closing the device")
	}
}

// TestAPauseThatCannotReleaseTheMicrophoneWarnsOnTheStatus fails if GET
// /api/status does not warn while a pause could not release the microphone,
// or still warns once the microphone is released.
func TestAPauseThatCannotReleaseTheMicrophoneWarnsOnTheStatus(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	warnings := newWarningsRegistry()
	capture.onMicHeld = micHeldWarning(warnings)
	mic := newStuckMic()
	release := sync.OnceFunc(func() { close(mic.release) })
	ctx, cancel := context.WithCancel(context.Background())
	done := goDone(func() {
		streamMicWithRetry(ctx, capture, mic, io.Discard, func(time.Duration) {}, nopLogf, nil)
	})
	t.Cleanup(func() {
		cancel()
		release()
		if !returnedWithin(done, testWait) {
			t.Error("mic loop did not stop")
		}
	})
	mic.expectEvents(t, "stream")
	if micHeldWarned(t, warnings) {
		t.Fatal("control: the status warns before any pause")
	}

	capture.Pause()
	mic.expectEvents(t, "close: still reading")
	waitForHardware(t, "the status to warn that the microphone is still open", func() bool { return micHeldWarned(t, warnings) })

	release()
	mic.expectEvents(t, "close")
	waitForHardware(t, "the warning to clear once the microphone closed", func() bool { return !micHeldWarned(t, warnings) })
}

// TestAMicrophoneAPauseCannotReleaseKeepsTheJabraLightUnchanged fails if the
// Jabra's light is written while a capture stream a pause could not close is
// still open, or is not written once the stream closes. A Jabra whose light
// changes while one of its streams is open sends Phone Mute by itself
// (captureDevice).
func TestAMicrophoneAPauseCannotReleaseKeepsTheJabraLightUnchanged(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newStuckMic()
	device := newCaptureDevice(capture.IsPaused)
	detach := device.attach(func(on bool) error {
		if on {
			mic.events <- "light on"
		} else {
			mic.events <- "light off"
		}
		return nil
	})
	t.Cleanup(detach)
	mic.expectEvents(t, "light off")
	if err := device.openStream(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	mic.expectEvents(t, "light off")
	release := sync.OnceFunc(func() { close(mic.release) })
	ctx, cancel := context.WithCancel(context.Background())
	done := goDone(func() {
		streamMicWithRetry(ctx, capture, orderedMic{micStreamer: mic, device: device}, io.Discard, func(time.Duration) {}, nopLogf, nil)
	})
	t.Cleanup(func() {
		cancel()
		release()
		if !returnedWithin(done, testWait) {
			t.Error("mic loop did not stop")
		}
	})
	mic.expectEvents(t, "stream")

	capture.Pause()
	mic.expectEvents(t, "close: still reading")
	select {
	case got := <-mic.events:
		t.Fatalf("device call %q while the stream a pause could not close is open, want none", got)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	mic.expectEvents(t, "close", "light on")
}
