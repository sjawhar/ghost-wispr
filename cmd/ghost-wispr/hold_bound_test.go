package main

import (
	"context"
	"net"
	"sync"
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
				&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
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
