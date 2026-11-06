package main

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// captureFinalizeWait is how long a capture pause gives Deepgram's Finalize,
// the call and its reply together, before it ends the session without it.
const captureFinalizeWait = 2 * time.Second

// captureStallAfter is how long the switch may be on with the gate still closed
// (the paused session is still being saved) before the capture is reported as
// stalled, a fault, rather than resuming.
const captureStallAfter = 30 * time.Second

// captureWriteWait is how long a pause waits for an audio write that passed
// the gate just before it closed (one mic buffer, normally milliseconds).
const captureWriteWait = 2 * time.Second

// captureEndRetry is how long a pause waits before it retries a session end
// that failed; each further retry waits twice as long, up to
// captureEndRetryMax.
const (
	captureEndRetry    = time.Second
	captureEndRetryMax = 10 * time.Second
)

// Capture states, as GET /api/status reports them.
const (
	capturePaused         = storage.ComponentStatusPaused   // the switch is off
	captureResuming       = storage.ComponentStatusResuming // switch on, gate still closed
	captureStalled        = storage.ComponentStatusStalled  // as resuming, past captureStallAfter
	captureMicUnavailable = "mic_unavailable"               // gate open, microphone not capturing
	captureCapturing      = "capturing"                     // gate open, microphone capturing
)

// errGateClosed is what a captureGate writer returns while capture is paused.
// It ends Mic.Stream, so streamMicWithRetry can stop the device.
var errGateClosed = errors.New("microphone capture gate closed")

// captureGate is the pause switch for microphone capture. Pause closes the gate
// before it returns, so no audio the microphone captures after that passes the
// gate's writer to the recorder or Deepgram, and streamMicWithRetry stops the
// capture device until the gate opens again.
//
// A transition runs up to three steps. onPause runs inside Pause, just before
// the gate closes, and must not wait on anything: audio passes until it
// returns. settle runs in the background once no write that passed the gate
// before the pause is still running. beforeResume runs after the pause has
// settled and before the gate opens.
//
// Pause and Resume never wait for settle (it ends the session, which can take
// as long as encoding its recording). Resume moves the switch at once; the gate
// opens once the pause has settled, unless a Pause comes first.
type captureGate struct {
	onPause      func()
	settle       func()
	beforeResume func()
	stallAfter   time.Duration // see captureStallAfter
	writeWait    time.Duration // see captureWriteWait

	control    sync.Mutex    // serializes closing and opening the gate, with their steps
	mu         sync.Mutex    // guards the fields below
	switchOff  bool          // Pause was called and Resume has not been since
	resumedAt  time.Time     // when Resume last moved the switch on
	closed     bool          // no audio passes the gate
	closings   int           // counts closings, so a Resume left from an earlier one does nothing
	resumed    chan struct{} // closed while the gate is open
	closing    chan struct{} // closed while the gate is closed
	settled    chan struct{} // closed once the latest closing has settled
	writing    int           // writes that passed the gate and are still running
	writesDone *sync.Cond    // signalled (with mu) when a write finishes
}

func newCaptureGate(onPause, settle, beforeResume func()) *captureGate {
	resumed := make(chan struct{})
	close(resumed)
	g := &captureGate{
		onPause:      onPause,
		settle:       settle,
		beforeResume: beforeResume,
		stallAfter:   captureStallAfter,
		writeWait:    captureWriteWait,
		resumed:      resumed,
		closing:      make(chan struct{}),
	}
	g.writesDone = sync.NewCond(&g.mu)
	return g
}

// Pause runs onPause, closes the gate, then settles the pause in the
// background. onPause runs first so nothing it blocks (new sessions) can start
// behind a closed gate; it waits for nothing (the app's only sets a flag), so
// the gate closes at once. Pausing while the gate is closed only cancels a
// pending Resume.
func (g *captureGate) Pause() {
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	g.switchOff = true
	alreadyClosed := g.closed
	g.mu.Unlock()
	if alreadyClosed {
		return
	}

	if g.onPause != nil {
		g.onPause()
	}

	g.mu.Lock()
	g.closed = true
	g.closings++
	g.resumed = make(chan struct{})
	close(g.closing)
	settled := make(chan struct{})
	g.settled = settled
	g.mu.Unlock()

	go func() {
		defer close(settled)
		if !g.waitForInflightWrites() {
			// Its chunk is from before the pause. Nothing orders it before
			// the resume: it can reach Deepgram late, and if it reaches the
			// recorder after the resume dropped the preroll, it opens the
			// next session's recording.
			log.Printf("warning: capture pause: an audio write was still running after %v; settling without it", g.writeWait)
		}
		if g.settle != nil {
			g.settle()
		}
	}()
}

// Resume moves the switch back on and returns without waiting for the pause
// to settle. The gate opens, after beforeResume, once the pause has settled,
// unless Pause is called first. It takes the transition lock like Pause, so
// the two apply in the order they are called.
func (g *captureGate) Resume() {
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.switchOff {
		return
	}
	g.switchOff = false
	g.resumedAt = time.Now()
	go g.openAfter(g.closings, g.settled)
}

// captureState reports what capture is doing: paused, resuming or stalled
// while the gate is closed, then capturing or mic_unavailable.
func (g *captureGate) captureState(mic interface{ IsOpen() bool }) string {
	if hold := g.MicHold(); hold != "" {
		return hold
	}
	if g.IsPaused() {
		return capturePaused // the moment between Pause starting and the gate closing
	}
	if !mic.IsOpen() {
		return captureMicUnavailable
	}
	return captureCapturing
}

// MicHold reports why the gate is holding the microphone off: paused, resuming
// or stalled, or "" while the gate is open. Readiness checks read it.
func (g *captureGate) MicHold() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !g.closed:
		return ""
	case g.switchOff:
		return capturePaused
	case time.Since(g.resumedAt) > g.stallAfter:
		return captureStalled
	default:
		return captureResuming
	}
}

// openAfter opens the gate once closing number closing has settled, if the
// switch is still on and no later closing has happened.
func (g *captureGate) openAfter(closing int, settled <-chan struct{}) {
	<-settled
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	open := g.closed && !g.switchOff && g.closings == closing
	g.mu.Unlock()
	if !open {
		return
	}
	if g.beforeResume != nil {
		g.beforeResume()
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = false
	close(g.resumed)
	g.closing = make(chan struct{})
}

// waitForInflightWrites waits, up to writeWait, until no write that passed the
// gate before it closed is still running. It reports false if one still is.
func (g *captureGate) waitForInflightWrites() bool {
	expired := false
	timer := time.AfterFunc(g.writeWait, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		expired = true
		g.writesDone.Broadcast()
	})
	defer timer.Stop()

	g.mu.Lock()
	defer g.mu.Unlock()
	for g.writing > 0 {
		if expired {
			return false
		}
		g.writesDone.Wait()
	}
	return true
}

func (g *captureGate) finishWrite() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writing--
	g.writesDone.Broadcast()
}

// IsPaused reports the switch: true from Pause until Resume.
func (g *captureGate) IsPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.switchOff
}

// isClosed reports the gate: true while no audio passes. It stays true after
// Resume until the pause has settled and the gate opens.
func (g *captureGate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// capturing reports whether audio is flowing: the gate is open and the
// microphone device is capturing.
func (g *captureGate) capturing(mic interface{ IsOpen() bool }) bool {
	return !g.isClosed() && mic.IsOpen()
}

// sleepUnlessPaused sleeps for d, or until the gate closes. The mic loop backs
// off with it, so a pause stops the device at once, not after the backoff.
func (g *captureGate) sleepUnlessPaused(d time.Duration) {
	g.mu.Lock()
	closing := g.closing
	g.mu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-closing:
	}
}

// waitResumed blocks until the gate is open. It reports false if ctx ends
// first.
func (g *captureGate) waitResumed(ctx context.Context) bool {
	g.mu.Lock()
	resumed := g.resumed
	g.mu.Unlock()

	select {
	case <-resumed:
		return true
	case <-ctx.Done():
		return false
	}
}

// Writer returns a writer that forwards audio to dst only while the gate is
// open, and otherwise drops it and returns errGateClosed.
func (g *captureGate) Writer(dst io.Writer) io.Writer {
	return &gatedWriter{gate: g, dst: dst}
}

type gatedWriter struct {
	gate *captureGate
	dst  io.Writer
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	g := w.gate
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return 0, errGateClosed
	}
	g.writing++
	g.mu.Unlock()
	defer g.finishWrite()
	return w.dst.Write(p)
}

// captureSessions is the part of *session.Manager a capture pause uses.
type captureSessions interface {
	PauseCapture()
	WaitForSessionStarts()
	ResumeCapture()
	ActiveSession() (string, time.Time)
	AwaitFinalize() <-chan struct{}
	ForceEndSession(ctx context.Context) error
}

// deepgramFinalizer asks Deepgram for final results on the audio it already has.
type deepgramFinalizer interface{ Finalize() error }

// newSessionCaptureGate returns the capture gate the app uses. deepgram returns
// the live Deepgram client, or nil when there is none.
//   - Pause blocks new sessions, which only sets a flag, then closes the gate.
//   - The pause then settles: it waits out a session start already under way,
//     Deepgram is asked to finalize, the call and its reply get up to
//     finalizeWait to reach the session that was recording, and that session
//     ends (after any other end of it already under way), so its last words
//     are kept and its recording is closed.
//   - After that, a Resume drops preroll audio from before the pause and lets
//     sessions start again, all before audio flows.
//
// Every wait before the gate opens is bounded except on the database and on
// the recording file: captureWriteWait for a write in flight at the pause,
// finalizeWait for Deepgram, and the recorder's encode time limit for a session
// end (audio.encodeTimeout). The session start the settle waits out and the
// session end write to the database, which has one connection; the session end
// and the preroll drop take the recorder's lock, which a write to the
// recording file holds. Those waits last as long as the database or the file
// write does. A session end that fails is retried, first after endRetry, until
// it succeeds. The gate stays closed through all of it: opening it while the
// session from before the pause is still current would file speech after the
// resume into it. So a stuck or failing database, or a recording that cannot
// be written or saved, keeps the microphone off, and the capture reports
// resuming, then stalled, until the session has ended.
func newSessionCaptureGate(sessions captureSessions, preroll interface{ DiscardPreroll() }, deepgram func() deepgramFinalizer, finalizeWait, endRetry time.Duration, logf func(string, ...any)) *captureGate {
	return newCaptureGate(
		sessions.PauseCapture,
		func() { settleCapturePause(sessions, deepgram(), finalizeWait, endRetry, logf) },
		func() {
			preroll.DiscardPreroll()
			sessions.ResumeCapture()
		},
	)
}

func settleCapturePause(sessions captureSessions, dg deepgramFinalizer, finalizeWait, endRetry time.Duration, logf func(string, ...any)) {
	// A session start under way finishes before anything else, so its session
	// is the one this pause ends. Its database writes are not bounded.
	sessions.WaitForSessionStarts()

	if dg != nil {
		// With no session recording, Deepgram's reply is dropped, but Finalize
		// still clears the audio it holds so none of it reaches a later session.
		var finalized <-chan struct{}
		if id, _ := sessions.ActiveSession(); id != "" {
			finalized = sessions.AwaitFinalize()
		}
		// The call can block on a half-open socket. Past finalizeWait the pause
		// goes on without it; a late call only finalizes early whatever Deepgram
		// then holds, since it dropped the buffered audio before it blocked.
		called := make(chan error, 1)
		go func() { called <- dg.Finalize() }()
		deadline := time.NewTimer(finalizeWait)
		select {
		case err := <-called:
			if err != nil {
				logf("warning: finalize deepgram on capture pause: %v", err)
			}
			if finalized != nil {
				select {
				case <-finalized:
				case <-deadline.C:
					logf("capture pause: no final transcript from deepgram within %v", finalizeWait)
				}
			}
		case <-deadline.C:
			logf("warning: capture pause: deepgram finalize did not return within %v", finalizeWait)
		}
		deadline.Stop()
	}

	// Not bounded by a deadline: the end waits for any other end of the session
	// already under way, then for the recorder's lock, then encodes (bounded by
	// the recorder's encode time limit) and writes the database, which is not.
	// An end that fails leaves the session current, so it is retried until it
	// succeeds; giving up would open the gate onto that session.
	wait := endRetry
	for {
		err := sessions.ForceEndSession(context.Background())
		if err == nil || errors.Is(err, session.ErrNoActiveSession) {
			return
		}
		logf("warning: end session on capture pause failed, retrying in %v: %v", wait, err)
		time.Sleep(wait)
		wait = min(2*wait, captureEndRetryMax)
	}
}
