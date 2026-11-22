package main

import (
	"context"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// lightOffBound is how soon after a pause these tests want the light off. A1
// ("Tap the Pi's power button: the light goes off") and R2 ("the light goes
// off") give no number; half a second is what a person tapping reads as at
// once, and leaves the light controller, which a pause wakes, room to run.
const lightOffBound = 500 * time.Millisecond

// lightOffWithin waits up to d for the light's last operation to show it off,
// waking the controller on each look. It returns the operations it saw last.
func (r *lightRig) lightOffWithin(d time.Duration) (bool, []lightOp) {
	deadline := time.Now().Add(d)
	for {
		ops := r.device.snapshot()
		if len(ops) > 0 && !isRedOp(ops[len(ops)-1]) {
			return true, ops
		}
		if time.Now().After(deadline) {
			return false, ops
		}
		r.hardware.Wake()
		time.Sleep(5 * time.Millisecond)
	}
}

// neverAnsweringServer listens on a loopback port, accepts every connection
// and never answers on it, as a Deepgram path that stalls does. It returns
// the websocket address.
func neverAnsweringServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return "ws://" + listener.Addr().String()
}

// TestAPauseDuringTheDeepgramDialLetsTheLightGoOff fails if a pause that
// comes while a microphone start dials Deepgram leaves the light red. The
// start holds red until it returns, and it returned only once
// newLiveTranscription's first dial had: against a Deepgram path that accepts
// and never answers, a tap left the light red for the 15 s handshake limit
// (ReviewPR77 on #77, finding 3).
func TestAPauseDuringTheDeepgramDialLetsTheLightGoOff(t *testing.T) {
	r := startLightRig(t, false)
	host := neverAnsweringServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opened := make(chan struct{})
	clients := make(chan *transcribe.ResilientClient, 1)
	started := goDone(func() {
		_ = r.capture.openUnderRed(func() error {
			// startMicPipeline: the device is open, then live transcription
			// starts before the start returns.
			r.mic.open.Store(true)
			close(opened)
			clients <- newLiveTranscription(ctx, "test-key",
				&interfaces.ClientOptions{Host: host},
				constOpts(&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}),
				transcribe.ResilientConfig{}, transcriptCallback{}, newWarningsRegistry(), nil)
			return nil
		})
	})
	t.Cleanup(func() {
		select {
		case rc := <-clients:
			_ = rc.Close()
		case <-time.After(testWait):
		}
		cancel()
		returnedWithin(started, testWait)
	})
	<-opened

	pauseCapture(r.capture, r.hardware)
	r.mic.open.Store(false) // the mic loop closes the device on the pause (streamUntilClosed)
	if off, ops := r.lightOffWithin(lightOffBound); !off {
		t.Fatalf("the light was still red %v after a pause during the Deepgram dial: operations %v", lightOffBound, ops)
	}
	if !returnedWithin(started, lightOffBound) {
		t.Fatalf("the microphone start still ran %v after the pause, holding the light red, while Deepgram did not answer", lightOffBound)
	}
}

// blockedWrite is a recording whose writes block until release closes.
type blockedWrite struct {
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockedWrite(t *testing.T) *blockedWrite {
	w := &blockedWrite{writing: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(sync.OnceFunc(func() { close(w.release) }))
	return w
}

func (w *blockedWrite) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.release
	return len(p), nil
}

// TestAPauseDuringAWriteAfterAStallLetsTheLightGoOff fails if a pause leaves
// the light red while the first audio write after a stall waits on the
// recording file. The gate's writer confirmed the light red for that write and
// held it red until the file write returned, which can be never (ReviewPR77 on
// #77, finding 3).
func TestAPauseDuringAWriteAfterAStallLetsTheLightGoOff(t *testing.T) {
	r := startLightRig(t, false)
	// The microphone opens as the app opens it, under a red confirmation.
	if err := r.capture.openUnderRed(func() error { r.mic.open.Store(true); return nil }); err != nil {
		t.Fatalf("open the microphone: %v", err)
	}
	// The open's release woke the controller once: wait for that check (a
	// red read-back after the confirmation's), so that no check is pending
	// when the stall below begins. The controller then looks again only
	// when something wakes it.
	r.waitFor(t, "the controller's check after the open", func(ops []lightOp) bool {
		n := len(ops)
		return n >= 2 && isRedReadBack(ops[n-1]) && isRedReadBack(ops[n-2])
	})
	// No audio has reached the recording for longer than the stall limit.
	r.capture.audioAt.Store(time.Now().Add(-2 * audioStallAfter).UnixNano())
	recording := newBlockedWrite(t)
	writer := r.capture.Writer(recording)
	written := goDone(func() { _, _ = writer.Write(make([]byte, 8)) })
	t.Cleanup(func() { returnedWithin(written, testWait) })
	select {
	case <-recording.writing:
	case <-time.After(testWait):
		t.Fatal("the audio did not reach the recording")
	}

	pauseCapture(r.capture, r.hardware)
	r.mic.open.Store(false) // the mic loop closes the device on the pause (streamUntilClosed)
	if off, ops := r.lightOffWithin(lightOffBound); !off {
		t.Fatalf("the light was still red %v after a pause during a write after a stall: operations %v", lightOffBound, ops)
	}
}

// TestAStartAPauseCancelledRunsAgainAtTheNextResume fails if a microphone
// start that a pause cancelled before it opened a device (errMicStartPaused)
// is taken for a failure: the start-up retry would then wait its 15 s
// interval after the next resume, or the first resume after a paused start
// would hand over to that retry, instead of starting at the resume.
func TestAStartAPauseCancelledRunsAgainAtTheNextResume(t *testing.T) {
	t.Run("first resume after a paused start", func(t *testing.T) {
		capture := newCaptureGate(nil, nil, nil)
		capture.Pause()
		var loops sync.WaitGroup
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); joinMicLoops(&loops, testWait) })
		starts := make(chan int, 4)
		calls := 0
		_, _ = startMicrophone(ctx, &loops, capture, alreadyBooted(), func(bool) error {
			calls++
			if calls == 1 {
				// The pause lands before the test hears of this start, so its
				// next Resume always comes after it: reported first, a slow
				// runner let the Resume win and the pause stood (#89's CI).
				capture.Pause() // during the scan
				starts <- calls
				return errMicStartPaused
			}
			starts <- calls
			return nil
		}, func() { t.Error("a start a pause cancelled went to the start-up retry") })
		capture.Resume()
		if got := receiveWithin(t, starts); got != 1 {
			t.Fatalf("start %d, want 1", got)
		}
		capture.Resume()
		if got := receiveWithin(t, starts); got != 2 {
			t.Fatalf("start %d after the next resume, want 2", got)
		}
	})
	t.Run("start-up retry", func(t *testing.T) {
		capture := newCaptureGate(nil, nil, nil)
		starts := make(chan int, 4)
		calls := 0
		done := goDone(func() {
			// oneShotAfter fires once: a second interval wait never ends.
			retryMicStartup(context.Background(), time.Hour, oneShotAfter(), capture, nil, func() error {
				calls++
				if calls == 1 {
					capture.Pause() // reported after the pause, as above
					starts <- calls
					return errMicStartPaused
				}
				starts <- calls
				return nil
			}, func(err error) { t.Errorf("a start a pause cancelled was reported as failed: %v", err) }, nil)
		})
		if got := receiveWithin(t, starts); got != 1 {
			t.Fatalf("start %d, want 1", got)
		}
		capture.Resume()
		if got := receiveWithin(t, starts); got != 2 {
			t.Fatalf("start %d after the resume, want 2 without the retry interval", got)
		}
		if !returnedWithin(done, testWait) {
			t.Fatal("the retry still runs after a start that worked")
		}
	})
}

// pauseCancelledStartLoop is a loop that starts the microphone and, when a
// pause cancels the start (errMicStartPaused), starts it again at the next
// resume. run starts it on capture.
type pauseCancelledStartLoop struct {
	name string
	run  func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, capture *captureGate, start func() error)
}

// resumeStartLoops are the loops that wait for the gate to open, and for the
// hardware (prepareMicOpen), before every start: the first resume after a
// paused start-up, and the start-up retry.
var resumeStartLoops = []pauseCancelledStartLoop{
	{name: "first resume after a paused start", run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, capture *captureGate, start func() error) {
		capture.Pause()
		_, _ = startMicrophone(ctx, loops, capture, alreadyBooted(), func(bool) error { return start() }, func() { t.Error("a start a pause cancelled went to the start-up retry") })
		capture.Resume()
	}},
	{name: "start-up retry", run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, capture *captureGate, start func() error) {
		loops.Go(func() {
			// oneShotAfter fires once: a second interval wait never ends.
			retryMicStartup(ctx, time.Hour, oneShotAfter(), capture, nil, start, func(err error) { t.Errorf("a start a pause cancelled was reported as failed: %v", err) }, nil)
		})
	}},
}

// pauseCancelledStartLoops are those, and the boot start, which runs at once
// on the open gate and hands a start a pause cancelled to the first.
var pauseCancelledStartLoops = append(slices.Clip(resumeStartLoops), pauseCancelledStartLoop{
	name: "boot start", run: func(t *testing.T, ctx context.Context, loops *sync.WaitGroup, capture *captureGate, start func() error) {
		loops.Go(func() {
			_, _ = startMicrophone(ctx, loops, capture, alreadyBooted(), func(bool) error { return start() }, func() { t.Error("a start a pause cancelled went to the start-up retry") })
		})
	},
})

// runOn runs the loop on capture until the test ends, with start as the
// microphone start, and returns the channel on which each start sends the
// number of its call, 1 first, before start runs. A start that runs while
// the gate is closed fails the test: the microphone opens only while the
// unit is not paused. Unless the test readies the hardware itself
// (beforeMicOpen), so does a loop that readies it while the gate is closed:
// it did not wait for the gate, and spins while the unit is paused.
func (l pauseCancelledStartLoop) runOn(t *testing.T, capture *captureGate, start func(call int) error) <-chan int {
	t.Helper()
	var loops sync.WaitGroup
	t.Cleanup(func() {
		if !joinMicLoops(&loops, testWait) {
			t.Error("microphone start-up still running after shutdown")
		}
	})
	if capture.beforeMicOpen == nil {
		var reported atomic.Bool
		capture.beforeMicOpen = func(context.Context, <-chan struct{}) bool {
			if capture.isClosed() && !reported.Swap(true) {
				t.Error("the loop readied the hardware while the gate was closed: it did not wait for the gate to open")
			}
			return true
		}
	}
	starts := make(chan int, 4)
	calls := 0
	l.run(t, t.Context(), &loops, capture, func() error {
		calls++
		if capture.isClosed() {
			t.Errorf("start %d ran while the gate was closed", calls)
		}
		starts <- calls
		return start(calls)
	})
	return starts
}

// nextStart fails the test unless the next start on starts is call number
// want, within testWait. what says when that start must come.
func nextStart(t *testing.T, starts <-chan int, want int, what string) {
	t.Helper()
	select {
	case got := <-starts:
		if got != want {
			t.Fatalf("start %d %s, want start %d", got, what, want)
		}
	case <-time.After(testWait):
		t.Fatalf("no start within %v %s", testWait, what)
	}
}

// TestAResumeBeforeTheLoopWaitsAgainStartsTheMicrophone fails if a resume
// that lands after a pause cancelled a microphone start, and before the
// start's loop waits for the gate again, is lost: the person resumed, and the
// microphone never starts. It also fails if the cancelled start ends that
// resume's "resuming" state, which lasts until the start that brings the
// microphone back has finished. The cancelled start pauses, resumes and sees
// the gate open before it returns, so the resume has landed before its loop
// looks again.
func TestAResumeBeforeTheLoopWaitsAgainStartsTheMicrophone(t *testing.T) {
	for _, tc := range pauseCancelledStartLoops {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			holds := make(chan string, 4) // MicHold as each later start runs
			starts := tc.runOn(t, capture, func(call int) error {
				if call > 1 {
					holds <- capture.MicHold()
					return nil
				}
				capture.Pause() // during the scan
				capture.Resume()
				opened, stop := context.WithTimeout(t.Context(), testWait)
				defer stop()
				if !capture.waitResumed(opened) {
					t.Error("the gate did not open after the resume")
				}
				return errMicStartPaused
			})
			nextStart(t, starts, 1, "once the gate was open")
			nextStart(t, starts, 2, "after a resume that landed before the loop waited again: the resume was lost")
			if hold := <-holds; hold != captureResuming {
				t.Fatalf("mic hold while the start after the resume ran = %q, want %q: the cancelled start ended the resume's state", hold, captureResuming)
			}
			waitMicHold(t, capture, "")
		})
	}
}

// TestAPauseAfterTheLastResumeKeepsACancelledStartClosed fails if a resume
// that came before the pause that cancelled a microphone start is kept for
// after that pause: the pause came last, so the microphone stays closed until
// the next resume. This order made TestAStartAPauseCancelledRunsAgainAtTheNextResume
// time out under load while its start reported itself before its pause: the
// test's resume ran first, found the switch on and did nothing.
func TestAPauseAfterTheLastResumeKeepsACancelledStartClosed(t *testing.T) {
	for _, tc := range pauseCancelledStartLoops {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			resumed := make(chan struct{})
			paused := make(chan struct{})
			starts := tc.runOn(t, capture, func(call int) error {
				if call > 1 {
					return nil
				}
				select {
				case <-resumed:
				case <-t.Context().Done():
				}
				capture.Pause() // during the scan, after the test's resume
				close(paused)
				return errMicStartPaused
			})
			nextStart(t, starts, 1, "once the gate was open")
			capture.Resume() // the switch is on: nothing to resume
			close(resumed)
			if !returnedWithin(paused, testWait) {
				t.Fatal("the start did not pause")
			}
			expectStaysClosed(t, capture, "after a pause that came after the last resume")
			select {
			case got := <-starts:
				t.Fatalf("start %d ran after a pause that came after the last resume", got)
			default:
			}
			if !capture.IsPaused() {
				t.Fatal("the switch is on after a pause that came after the last resume")
			}
			capture.Resume()
			nextStart(t, starts, 2, "after the next resume")
		})
	}
}

// TestAPauseWhileAStartWaitsForTheHardwareStartsAtTheNextResume fails if a
// pause that comes while a start waits for the hardware (prepareMicOpen, as
// while speech plays on the Jabra) makes the next resume wait for anything
// but the gate: the start-up retry waited its 15 s interval first.
func TestAPauseWhileAStartWaitsForTheHardwareStartsAtTheNextResume(t *testing.T) {
	for _, tc := range resumeStartLoops {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			paused := make(chan struct{})
			prepares := 0
			capture.beforeMicOpen = func(_ context.Context, closing <-chan struct{}) bool {
				prepares++
				if prepares > 1 {
					return true
				}
				capture.Pause() // while the hardware gets ready
				close(paused)
				<-closing
				return false
			}
			starts := tc.runOn(t, capture, func(int) error { return nil })
			if !returnedWithin(paused, testWait) {
				t.Fatal("the start never waited for the hardware")
			}
			capture.Resume()
			nextStart(t, starts, 1, "after the resume that followed a pause during the wait for the hardware")
		})
	}
}

// TestAPauseJustBeforeAStartOpensNothing fails if a pause that lands after a
// loop found the gate open, but before the start runs, lets the start run:
// the start's PortAudio scan would open capture streams after the pause. A
// pause once the hardware is ready must also stop the light from being
// confirmed red for the start, which would light it red again after the
// pause; one during that confirmation stops the start under the red hold.
// The next resume starts the microphone.
func TestAPauseJustBeforeAStartOpensNothing(t *testing.T) {
	for _, during := range []string{"after the hardware is ready", "during the red confirmation"} {
		for _, tc := range resumeStartLoops {
			t.Run(during+"/"+tc.name, func(t *testing.T) {
				capture := newCaptureGate(nil, nil, nil)
				paused := make(chan struct{})
				pause := sync.OnceFunc(func() {
					capture.Pause()
					close(paused)
				})
				capture.beforeMicOpen = func(context.Context, <-chan struct{}) bool {
					if during == "after the hardware is ready" {
						pause()
					}
					return true
				}
				capture.beforeMicStart = func() error {
					if during == "during the red confirmation" {
						pause()
					} else if capture.isClosed() {
						t.Error("the light was confirmed red for a start while the gate was closed")
					}
					return nil
				}
				starts := tc.runOn(t, capture, func(int) error { return nil })
				if !returnedWithin(paused, testWait) {
					t.Fatal("the pause never came")
				}
				expectStaysClosed(t, capture, "after a pause just before a start")
				select {
				case got := <-starts:
					t.Fatalf("start %d ran after a pause %s", got, during)
				default:
				}
				capture.Resume()
				nextStart(t, starts, 1, "after the resume that followed a pause "+during)
			})
		}
	}
}

// TestAPauseBeforeAStartsLightCheckLeavesTheLightOff fails if a pause that
// lands after a start found the gate open, but before its red confirmation
// writes the light, lets that confirmation turn the light red. Red says the
// unit records, and nothing records then: the start opens nothing. The pause
// lands during the capture-device check (capturePresent), which runs between
// the two. The next resume starts the microphone under a red light.
func TestAPauseBeforeAStartsLightCheckLeavesTheLightOff(t *testing.T) {
	for _, tc := range resumeStartLoops {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, false)
			var armed atomic.Bool
			var arm sync.Once // only the first start meets the pause
			r.capture.beforeMicOpen = func(context.Context, <-chan struct{}) bool {
				arm.Do(func() { armed.Store(true) }) // the start is ready; its light check comes next
				return true
			}
			pausedAt := make(chan int, 1) // the blink(1)'s operations at the pause
			var paused atomic.Bool
			r.capture.capturePresent = func() bool {
				if armed.CompareAndSwap(true, false) {
					pauseCapture(r.capture, r.hardware)
					paused.Store(true)
					pausedAt <- len(r.device.snapshot())
				}
				return true
			}
			// confirmed closes once the start's red confirmation, after the
			// pause, has returned, through whichever hook it takes.
			confirmed := make(chan struct{})
			var confirmedOnce sync.Once
			afterPause := func() {
				if paused.Load() {
					confirmedOnce.Do(func() { close(confirmed) })
				}
			}
			openRed, startRed := r.capture.beforeMicOpenRed, r.capture.beforeMicStart
			r.capture.beforeMicOpenRed = func(allowed func() error) error {
				defer afterPause()
				holds := r.hardware.redHolds.Load()
				return openRed(func() error {
					// A hold makes the light controller light the light,
					// so a refused confirmation must not have taken one.
					err := allowed()
					if err != nil && r.hardware.redHolds.Load() > holds {
						t.Error("a red confirmation the gate refused held the light red")
					}
					return err
				})
			}
			r.capture.beforeMicStart = func() error { defer afterPause(); return startRed() }
			starts := tc.runOn(t, r.capture, func(int) error { return nil })
			var from int
			select {
			case from = <-pausedAt:
			case <-time.After(testWait):
				t.Fatal("no start checked for a capture device")
			}
			if !returnedWithin(confirmed, testWait) {
				t.Fatal("the start's red confirmation never returned")
			}
			r.waitOff(t)
			if ops := r.device.snapshot(); slices.ContainsFunc(ops[from:], func(op lightOp) bool { return !op.read && isRedOp(op) }) {
				t.Fatalf("the light was written red after a pause that came before the start's light check: operations since the pause %v", ops[from:])
			}
			select {
			case got := <-starts:
				t.Fatalf("start %d ran after a pause that came before its light check", got)
			default:
			}
			r.capture.Resume()
			nextStart(t, starts, 1, "after the resume that followed the pause")
		})
	}
}

// TestAPauseDuringAStartsRedWriteLetsTheLightGoOff fails if a pause that lands
// after a start's light check passed, while the confirmation takes its red
// hold and writes red, leaves the light red past lightOffBound, or lets the
// start run. Pause takes no light lock, so that it never waits on a light
// command, and the red write goes ahead (confirmMicOpenRed); the start is
// refused under its red hold, which is then released, and the light must go
// off within the bound of any pause, without the test waking the light
// controller. The control checks that the pause landed in that window: the
// light was written red after it. (Review 5975477041 of #93, its probe.)
func TestAPauseDuringAStartsRedWriteLetsTheLightGoOff(t *testing.T) {
	for _, tc := range pauseCancelledStartLoops {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, false)
			type pausedAt struct {
				when time.Time
				ops  int
			}
			paused := make(chan pausedAt, 1)
			var pause sync.Once
			openRed := r.capture.beforeMicOpenRed
			r.capture.beforeMicOpenRed = func(allowed func() error) error {
				return openRed(func() error {
					err := allowed()
					if err == nil {
						pause.Do(func() { // the check passed; the hold and the red write come next
							pauseCapture(r.capture, r.hardware)
							paused <- pausedAt{time.Now(), len(r.device.snapshot())}
						})
					}
					return err
				})
			}
			starts := tc.runOn(t, r.capture, func(int) error { return nil })
			var at pausedAt
			select {
			case at = <-paused:
			case <-time.After(testWait):
				t.Fatal("no start passed its light check")
			}
			var ops []lightOp
			redAfter, off := false, false
			for deadline := at.when.Add(lightOffBound); ; time.Sleep(time.Millisecond) {
				ops = r.device.snapshot()[at.ops:]
				redAfter = slices.ContainsFunc(ops, func(op lightOp) bool { return !op.read && isRedOp(op) })
				off = len(ops) > 0 && !isRedOp(ops[len(ops)-1])
				if (redAfter && off) || time.Now().After(deadline) {
					break
				}
			}
			if !redAfter {
				t.Fatalf("control: the light was not written red after the pause, so the pause did not land between the check and the red write: operations since the pause %v", ops)
			}
			if !off {
				t.Fatalf("the light was still red %v after a pause during a start's red write: operations since the pause %v", lightOffBound, ops)
			}
			select {
			case got := <-starts:
				t.Fatalf("start %d ran after a pause during its red write", got)
			default:
			}
			r.capture.Resume()
			nextStart(t, starts, 1, "after the resume that followed the pause")
		})
	}
}

func receiveWithin(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatal("timed out")
		return 0
	}
}
