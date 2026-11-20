package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// captureFinalizeWait is how long a capture pause gives Deepgram's Finalize,
// the call and its reply together, before it ends the session without it.
const captureFinalizeWait = 2 * time.Second

// captureStallAfter is how long the switch may be on with the gate still closed
// (the paused session is not ended yet) before the capture is reported as
// stalled, a fault, rather than resuming. A stall does not always clear by
// itself; see newSessionCaptureGate. It also bounds how long after a resume
// the capture reports resuming once the gate is open (see MicHold).
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
	capturePaused           = storage.ComponentStatusPaused   // the switch is off
	captureResuming         = storage.ComponentStatusResuming // switch on, gate still closed
	captureStalled          = storage.ComponentStatusStalled  // as resuming, past captureStallAfter
	captureLightUnavailable = "light_unavailable"             // switch on, gate held closed until the light is confirmed red
	captureMicUnavailable   = "mic_unavailable"               // gate open, microphone not capturing
	captureNoAudio          = "no_audio"                      // gate open, microphone open, no audio reaches the recording
	captureCapturing        = "capturing"                     // gate open, microphone capturing
)

// errGateClosed is what a captureGate writer returns while capture is paused.
// It ends Mic.Stream, so streamMicWithRetry can close the device.
var errGateClosed = errors.New("microphone capture gate closed")

// errLightWentOff is what a captureGate writer returns for audio the recording
// light may not have been red for: since the microphone opened under a red
// confirmation (confirmMicOpen), the light was written, or read back, as
// anything but red. The audio is dropped, and streamMicWithRetry closes the
// microphone, which drops what its stream still holds, and opens it again
// under a fresh confirmation.
var errLightWentOff = errors.New("the recording light was not red while the microphone was open")

// errClosedSinceMicOpen is what a captureGate writer returns for audio from a
// microphone that opened before a closing of the gate: since the microphone's
// last red confirmation began (confirmMicOpen), a pause or a hold for the
// light closed the gate, also if it has opened again since. Some of the audio,
// and whatever the microphone still holds, may have been captured during the
// pause. The audio is dropped, and streamMicWithRetry closes the microphone,
// which drops the rest, and opens it again under a fresh confirmation.
var errClosedSinceMicOpen = errors.New("the capture gate closed while the microphone was open")

// captureGate is the pause switch for microphone capture. Pause closes the gate
// before it returns, so no audio the microphone captures after that passes the
// gate's writer to the recorder or Deepgram, and streamMicWithRetry closes the
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
//
// HoldForLight closes the gate as Pause does, without moving the switch, when
// the recording light cannot be confirmed red. The gate then opens only after
// ReleaseLightHold, through the same settle and checks as a Resume.
type captureGate struct {
	onPause             func()
	settle              func()
	beforeResume        func()
	beforeResumeRequest func() error
	beforeGateOpen      func() error
	onResumeRefused     func(error)
	beforeMicStart      func() error
	// checkLightPattern readies the light for a red confirmation: it checks
	// the stored pattern of a unit not yet checked (hardwareControl.CheckLight).
	// It can take seconds, so RequestResume and openAfter run it before they
	// take control, and a pause meanwhile takes control at once (review
	// 5958478401 of #79); confirmMicOpen runs it before its confirmation.
	checkLightPattern  func() error
	beforeMicOpen      func(ctx context.Context, closing <-chan struct{}) bool // readies the hardware before a resume's red confirmation; false once closing closes or ctx ends (prepareMicOpen)
	afterMicStart      func()
	afterGateOpen      func()
	onLightUnconfirmed func(error)   // a red confirmation for a microphone open failed; must not block
	lightOffs          func() uint64 // counts the times the recording light was written, or read back, as anything but red; nil without a light
	lightRedMark       func() uint64 // lightOffs as the light's last red confirmation left it, read under the light's lock
	checkLight         func()        // reads the light back, which counts it in lightOffs unless it is red; nil without a light
	afterMicClose      func()        // runs after the mic loop closed the microphone, so the light, which follows it, can go off; must not block
	dropHeldAudio      func()        // drops the audio the recorder keeps for the next session's start; runs at every microphone close, before afterMicClose
	onMicHeld          func(bool)    // true when a pause could not release the microphone, false once it is released; must not block
	capturePresent     func() bool   // reports whether a capture device exists, without opening one; nil when it cannot tell
	onClosed           func(int)     // runs once the gate has closed, with its closings count; must not block
	onRecording        func()        // runs at the first audio that reaches the recording after each opening into recording; must not block
	onRedCheckFailed   func()        // runs when an opening finds the light not red; must not block
	audioStall         time.Duration // see audioStallAfter
	stallAfter         time.Duration // see captureStallAfter
	writeWait          time.Duration // see captureWriteWait
	control            sync.Mutex    // serializes closing and opening the gate, with their steps
	mu                 sync.Mutex    // guards the fields below
	switchOff          bool          // Pause was called and Resume has not been since
	pauses             int           // counts pauses asked for, so a resume whose light check a pause came during does nothing
	pendingPauses      int           // Pause calls that wait for control: a resume then counts the unit as paused
	requestedPauses    int           // pauses asked for (pauseRequested) that Pause has not taken yet
	lightHeld          bool          // HoldForLight was called and ReleaseLightHold, or a resume that confirmed red, has not been since
	resumedAt          time.Time     // when Resume last moved the switch on
	starting           bool          // that Resume's start of the microphone has not finished (finishResume)
	preparing          bool          // a start waits in beforeMicOpen (prepareMicOpen)
	closed             bool          // no audio passes the gate
	recordingDue       bool          // the gate opened into recording, and no audio has reached the recording since (onRecording)
	closings           int           // counts closings, so a Resume left from an earlier one does nothing
	resumed            chan struct{} // closed while the gate is open
	closing            chan struct{} // closed while the gate is closed
	settled            chan struct{} // closed once the latest closing has settled
	writing            int           // writes that passed the gate and are still running
	writesDone         *sync.Cond    // signalled (with mu) when a write finishes
	// audioAt is when audio last reached the recording through the gate, or
	// when a start or gate opening gave it audioStall to begin (armAudio), in
	// Unix nanoseconds; 0 before the first. While it is older than audioStall,
	// the unit is not recording: the mic loop closes the microphone, and the
	// light, which follows it, goes off.
	audioAt atomic.Int64
	// stalled is set while the mic loop holds the microphone closed because no
	// audio reached the recording: the status says no_audio.
	stalled atomic.Bool
	// redMark is lightOffs as the last red confirmation for a microphone open
	// or start left it (confirmMicOpen). While lightOffs still reads it, the
	// light has been red since that microphone opened.
	redMark atomic.Uint64
	// beforeMicOpenRed confirms the light red for a microphone open, as
	// beforeMicStart does, but calls allowed under the light's lock just
	// before it writes the light, and writes nothing, returning allowed's
	// error, unless allowed returns nil (confirmMicOpenRed). Nil uses
	// beforeMicStart.
	beforeMicOpenRed func(allowed func() error) error
	// openMark is the gate's closing count as the microphone's last red
	// confirmation began (markMicOpen), or -1 if the gate was closed then;
	// openMarked says one has begun. Audio passes the gate only while no
	// closing has come since. Guarded by mu.
	openMark   int
	openMarked bool
}

// audioStallAfter is how long the microphone may deliver nothing to the
// recording, with the gate and the device open, before the unit counts as not
// recording, and the mic loop closes it. The device delivers a buffer every
// 250 ms.
const audioStallAfter = 2 * time.Second

// armAudio gives a capture that is starting audioStall to deliver.
func (g *captureGate) armAudio() { g.audioAt.Store(time.Now().UnixNano()) }

// audioFlowing reports whether audio reached the recording within
// audioStall. Before any start it has nothing to judge, and says yes.
func (g *captureGate) audioFlowing() bool {
	at := g.audioAt.Load()
	return at == 0 || time.Since(time.Unix(0, at)) <= g.audioStall
}

func newCaptureGate(onPause, settle, beforeResume func()) *captureGate {
	resumed := make(chan struct{})
	close(resumed)
	g := &captureGate{
		onPause:      onPause,
		settle:       settle,
		beforeResume: beforeResume,
		stallAfter:   captureStallAfter,
		audioStall:   audioStallAfter,
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
// pending Resume. It counts itself (pauses) when called, before it waits for
// control, so a resume called after it is the later call; a pause already
// counted when it was asked for (pauseRequested) is not counted again.
func (g *captureGate) Pause() {
	g.mu.Lock()
	if g.requestedPauses > 0 {
		g.requestedPauses--
	} else {
		g.pauses++
	}
	g.pendingPauses++
	g.mu.Unlock()
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	g.switchOff = true
	g.pendingPauses--
	g.mu.Unlock()
	g.closeGate()
}

// pauseRequested counts a pause a person asked for, before the persistent
// controls' lock, which may be held by another resume's save: a resume called
// after it then carries it in its ticket and is not overtaken by it, and a
// resume whose check it came during is overtaken (review 5963682292 of #79).
// The Pause that the request leads to takes this count instead of counting
// again, so each pauseRequested must lead to one Pause.
func (g *captureGate) pauseRequested() {
	g.mu.Lock()
	g.pauses++
	g.requestedPauses++
	g.mu.Unlock()
}

// pauseCount is the number of pauses asked for so far.
func (g *captureGate) pauseCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pauses
}

// HoldForLight closes the gate because the recording light cannot be
// confirmed red, leaving the switch as it is: the gate stays closed until
// ReleaseLightHold, and no pause is saved.
func (g *captureGate) HoldForLight() {
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	g.lightHeld = true
	g.mu.Unlock()
	g.closeGate()
}

// ReleaseLightHold lets a gate HoldForLight closed open again, once its
// closing has settled, if the switch is on. The gate opens only after
// beforeGateOpen confirms the light red; if it cannot, the hold comes back.
func (g *captureGate) ReleaseLightHold() {
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.lightHeld {
		return
	}
	g.lightHeld = false
	if g.closed && !g.switchOff {
		g.resumedAt = time.Now()
		g.starting = true
		go g.openAfter(g.closings, g.settled, true)
	}
}

// isLightHeld reports whether HoldForLight holds the gate.
func (g *captureGate) isLightHeld() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lightHeld
}

// voiceStamp reports the gate's closings count and whether it is open, for a
// transcript as it arrives: the voice acts on no wake from before a closing.
func (g *captureGate) voiceStamp() (closings int, open bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closings, !g.closed
}

// closeGate runs onPause, closes the gate, then settles the closing in the
// background. The caller holds control. Closing a closed gate does nothing.
func (g *captureGate) closeGate() {
	g.mu.Lock()
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
	closings := g.closings
	g.resumed = make(chan struct{})
	close(g.closing)
	settled := make(chan struct{})
	g.settled = settled
	g.mu.Unlock()
	if g.onClosed != nil {
		g.onClosed(closings)
	}

	go func() {
		defer close(settled)
		if !g.waitForInflightWrites() {
			// Its chunk is from before the pause. Nothing orders it before
			// the resume: it can reach Deepgram late, and if it reaches the
			// recorder after the resume dropped the preroll, it opens the
			// next session's recording.
			slog.Warn("capture pause: an audio write was still running; settling without it", "waited", g.writeWait)
		}
		if g.settle != nil {
			g.settle()
		}
	}()
}

// Resume requests an asynchronous gate opening. Call RequestResume when the
// caller must learn why the request was refused.
func (g *captureGate) Resume() {
	_ = g.RequestResume()
}

// RequestResume checks a requested resume before changing the switch, then
// returns without waiting for the paused session to settle. The gate opens
// after beforeResume once settling finishes, unless a Pause comes first.
// While no capture device is present (captureMissing), it takes no light
// check, so the light stays off: nothing can open, and every microphone open
// confirms red for itself once a device is there. It checks the light's
// stored pattern before it takes control (prepareResume); a Pause called
// during that check is the later call and wins: the resume then does nothing
// and returns nil (resumeWith).
func (g *captureGate) RequestResume() error {
	t, err := g.prepareResume()
	if err != nil {
		return err
	}
	if err := g.resumeWith(t, nil); !errors.Is(err, errResumeOvertaken) {
		return err
	}
	return nil
}

// resumeTicket is what a resume's light check (prepareResume) leaves for the
// resume itself (resumeWith): the pause count before the check.
type resumeTicket struct{ pauses int }

// errResumeOvertaken reports a resume that a Pause called during its light
// check overtook: the later call, the pause, wins, and the resume did nothing.
var errResumeOvertaken = errors.New("resume overtaken by a later pause")

// prepareResume is the part of a resume that can take seconds, so it runs
// before control, and before the persistent controls' lock: it reads the
// pause count and, if the switch is off or a pause is asked for and has not
// taken control yet, checks the light's stored pattern (checkLightPattern). A
// resume of a unit whose switch is on checks nothing: a failed check must not
// be taken for a refused resume of a unit that records (review 5961636823 of
// #79).
func (g *captureGate) prepareResume() (resumeTicket, error) {
	g.mu.Lock()
	t := resumeTicket{pauses: g.pauses}
	paused := g.switchOff || g.pendingPauses > 0 || g.requestedPauses > 0
	g.mu.Unlock()
	if paused && g.beforeResumeRequest != nil && g.checkLightPattern != nil && !g.captureMissing() {
		if err := g.checkLightPattern(); err != nil {
			return t, err
		}
	}
	return t, nil
}

// resumeWith resumes once prepareResume has checked the light. Under control,
// it returns errResumeOvertaken, having done nothing, if a Pause was called
// since t; otherwise it runs persist, if set, and, if the switch is off,
// confirms the light red (beforeResumeRequest) and moves the switch on. The
// gate opens once the pause has settled.
func (g *captureGate) resumeWith(t resumeTicket, persist func() error) error {
	g.control.Lock()
	defer g.control.Unlock()

	g.mu.Lock()
	overtaken := g.pauses != t.pauses
	g.mu.Unlock()
	if overtaken {
		return errResumeOvertaken
	}
	if persist != nil {
		if err := persist(); err != nil {
			return err
		}
	}
	g.mu.Lock()
	if !g.switchOff {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	checked := g.beforeResumeRequest != nil && !g.captureMissing()
	if checked {
		if err := g.beforeResumeRequest(); err != nil {
			return err
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.switchOff {
		return nil
	}
	g.switchOff = false
	if checked {
		// It has just confirmed the light red.
		g.lightHeld = false
	}
	g.resumedAt = time.Now()
	g.starting = true
	go g.openAfter(g.closings, g.settled, false)
	return nil
}

// captureState reports what capture is doing: paused, light_unavailable,
// resuming or stalled while the gate is closed, resuming until the resume's
// start has finished (MicHold), then capturing, no_audio while no audio
// reaches the recording (the mic loop then holds the microphone closed for a
// while), or mic_unavailable.
func (g *captureGate) captureState(mic interface{ IsOpen() bool }) string {
	if hold := g.MicHold(); hold != "" {
		return hold
	}
	if g.IsPaused() {
		return capturePaused // the moment between Pause starting and the gate closing
	}
	if !mic.IsOpen() {
		if g.stalled.Load() {
			return captureNoAudio
		}
		return captureMicUnavailable
	}
	if !g.audioFlowing() {
		return captureNoAudio
	}
	return captureCapturing
}

// MicHold reports why capture is not up on purpose: paused, held for the
// light, or stalled or resuming while the gate is closed; and resuming after
// the gate opens, until the start that brings the microphone and Deepgram
// back has finished (finishResume) or the stall limit has passed, and for as
// long as any start of the microphone waits for the hardware
// (prepareMicOpen), as when the Jabra's ring must be darkened after speech. It
// is "" otherwise. Readiness checks read it.
func (g *captureGate) MicHold() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !g.closed:
		if !g.switchOff && (g.preparing || (g.starting && time.Since(g.resumedAt) <= g.stallAfter)) {
			return captureResuming
		}
		return ""
	case g.switchOff:
		return capturePaused
	case g.lightHeld:
		return captureLightUnavailable
	case time.Since(g.resumedAt) > g.stallAfter:
		return captureStalled
	default:
		return captureResuming
	}
}

// openAfter opens the gate once closing number closing has settled, if the
// switch is still on, the light does not hold it, and no later closing has
// happened. forLight says ReleaseLightHold asked: a refusal from
// beforeGateOpen then holds the gate for the light again, where a person's
// resume is refused and the switch goes off. While no capture device is
// present, it takes no light check, and so releases no hold (afterGateOpen).
// It checks the light's stored pattern (checkLightPattern) before it takes
// control, so a pause meanwhile takes control at once; the switch it then
// finds off stops the opening.
func (g *captureGate) openAfter(closing int, settled <-chan struct{}, forLight bool) {
	<-settled
	var lightErr error
	if g.beforeGateOpen != nil && g.checkLightPattern != nil && g.canOpen(closing) && !g.captureMissing() {
		lightErr = g.checkLightPattern()
	}
	g.control.Lock()

	if !g.canOpen(closing) {
		g.control.Unlock()
		return
	}
	checked := g.beforeGateOpen != nil && !g.captureMissing()
	if checked {
		err := lightErr
		if err == nil {
			err = g.beforeGateOpen()
		}
		if err != nil {
			g.mu.Lock()
			if g.closed && g.closings == closing {
				if forLight {
					g.lightHeld = true
				} else {
					g.switchOff = true
				}
			}
			g.mu.Unlock()
			onResumeRefused := g.onResumeRefused
			g.control.Unlock()
			if !forLight {
				// A person's resume found the light not red. A light hold's
				// retries do not sound again: its fault did once.
				g.redCheckFailed()
				if onResumeRefused != nil {
					onResumeRefused(err)
				}
			}
			return
		}
	}
	if g.beforeResume != nil {
		g.beforeResume()
	}

	g.mu.Lock()
	if !g.closed || g.switchOff || g.lightHeld || g.closings != closing {
		g.mu.Unlock()
		g.control.Unlock()
		return
	}
	g.closed = false
	g.recordingDue = true
	close(g.resumed)
	g.closing = make(chan struct{})
	g.mu.Unlock()
	g.armAudio()
	g.control.Unlock()
	if checked && g.afterGateOpen != nil {
		g.afterGateOpen()
	}
}

// redCheckFailed reports a failed red check: the error sound (R10).
func (g *captureGate) redCheckFailed() {
	if g.onRedCheckFailed != nil {
		g.onRedCheckFailed()
	}
}

// canOpen reports whether openAfter for closing number closing may still open
// the gate: it is closed, the switch is on, the light does not hold it, and no
// later closing has happened.
func (g *captureGate) canOpen(closing int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed && !g.switchOff && !g.lightHeld && g.closings == closing
}

func (g *captureGate) confirmMicStart() error {
	if g.beforeMicStart == nil {
		return nil
	}
	return g.beforeMicStart()
}

func (g *captureGate) finishMicStart() {
	g.armAudio()
	if g.afterMicStart != nil {
		g.afterMicStart()
	}
}

// captureMissing reports that capturePresent finds no capture device: the
// light then stays off, since nothing can open.
func (g *captureGate) captureMissing() bool {
	return g.capturePresent != nil && !g.capturePresent()
}

// confirmMicOpen confirms the light red for a microphone open or start, as
// confirmMicStart does, holding it red until finishMicStart, and marks the
// light as red from then on: audio passes the gate only while the light has
// stayed red since (lightWentOff). Before anything else it marks the gate
// (markMicOpen): audio passes only while the gate has not closed since, and
// the light is written red only while it has not (confirmMicOpenRed): a
// closing before the write returns errMicStartPaused, with the light left as
// it was. It first looks for a capture device, without opening one
// (capturePresent): with none it lights nothing and returns
// errNoCaptureDevice, so the light stays off while the microphone is missing.
// It checks the light's stored pattern next (checkLightPattern), which can
// take seconds and holds no lock of the gate; a Pause, or a closing for the
// light, during that check cancels the open: it lights nothing and returns
// errMicStartPaused (review 5961636823 of #79). A light that cannot be
// confirmed red returns an error that wraps errLightNotConfirmed.
func (g *captureGate) confirmMicOpen() error {
	g.markMicOpen()
	if g.captureMissing() {
		return errNoCaptureDevice
	}
	if g.checkLightPattern != nil {
		g.mu.Lock()
		closings, pauses := g.closings, g.pauses
		g.mu.Unlock()
		if err := g.checkLightPattern(); err != nil {
			return fmt.Errorf("%w: %w", errLightNotConfirmed, err)
		}
		g.mu.Lock()
		cancelled := g.closings != closings || g.pauses != pauses
		g.mu.Unlock()
		if cancelled {
			return errMicStartPaused
		}
	}
	if err := g.confirmMicOpenRed(); errors.Is(err, errMicStartPaused) {
		return err
	} else if err != nil {
		return fmt.Errorf("%w: %w", errLightNotConfirmed, err)
	}
	if g.lightRedMark != nil {
		// Read under the light's lock by the confirmation itself, so no
		// failure counted after it is taken into the mark.
		g.redMark.Store(g.lightRedMark())
	}
	return nil
}

// confirmMicOpenRed confirms the light red for a microphone open. Through
// beforeMicOpenRed it writes the light only while the gate has not closed
// since the open's mark (openAllowed), checked under the light's lock just
// before the write: red says the unit records, so a pause before that check
// leaves the light as it was for an open that then opens nothing. A pause
// after the check, while the confirmation takes its red hold and writes red,
// does not stop that write: Pause takes no light lock, so that it never waits
// on a light command in progress. A start is then refused under its red hold
// (startUnderRed), opens nothing, releases the hold, and the light goes off
// with the pause, within the half second a pause allows
// (TestAPauseDuringAStartsRedWriteLetsTheLightGoOff, on the three start
// paths). The mic loop instead opens the device under that red, then finds
// the gate closed since its mark and closes the device before it streams it
// (TestNoAudioCapturedDuringAPauseReachesTheRecording, a pause during the red
// confirmation): no audio passes, and the light goes off once that open has
// returned, with no bound tested. Without beforeMicOpenRed it is
// confirmMicStart.
func (g *captureGate) confirmMicOpenRed() error {
	if g.beforeMicOpenRed == nil {
		return g.confirmMicStart()
	}
	return g.beforeMicOpenRed(g.openAllowed)
}

// openAllowed returns errMicStartPaused once the gate has closed since the
// microphone open's mark, or was closed then (closedSinceMicOpen), and nil
// otherwise.
func (g *captureGate) openAllowed() error {
	if g.closedSinceMicOpen() {
		return errMicStartPaused
	}
	return nil
}

// markMicOpen records, as a microphone open's red confirmation begins, the
// gate's closing count, or none if the gate is closed: from then on audio
// passes the gate only while no closing has come since, so audio a microphone
// captured during a pause never reaches the recording, however briefly the
// pause lasted and wherever in the open it came.
func (g *captureGate) markMicOpen() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.openMarked = true
	g.openMark = g.closings
	if g.closed {
		g.openMark = -1
	}
}

// openUnderRed runs open, which opens or starts a capture device, only once a
// capture device is present and the light is confirmed red (confirmMicOpen),
// and holds the light red until open returns: the light controller cannot
// turn it off meanwhile, so no device opens, and no PortAudio scan runs, while
// the light last showed anything but red. Otherwise it runs nothing and
// returns confirmMicOpen's error.
func (g *captureGate) openUnderRed(open func() error) error {
	if err := g.confirmMicOpen(); err != nil {
		return err
	}
	defer g.finishMicStart()
	return open()
}

// startUnderRed runs a microphone start under a red hold (openUnderRed), then
// ends the latest Resume's "resuming" state (finishResume), whether the start
// worked or failed: readiness then reports what came back. A start finds the
// gate open when its loop looks; a pause, or a hold for the light, that closed
// it since then cancels the start before the light is confirmed red, or under
// the red hold before the start runs: it opens nothing, not even a PortAudio
// scan, and returns errMicStartPaused. A start a pause cancelled ends no
// resume's state: the gate can be open again for a later Resume by then, and
// that Resume's own start ends it.
func (g *captureGate) startUnderRed(start func() error) error {
	err := errMicStartPaused
	if !g.isClosed() {
		err = g.openUnderRed(func() error {
			if g.isClosed() {
				return errMicStartPaused
			}
			return start()
		})
	}
	if !errors.Is(err, errMicStartPaused) {
		g.finishResume()
	}
	return err
}

// lightWentOff reports whether the light was written, or read back, as
// anything but red since the microphone last opened under confirmMicOpen.
func (g *captureGate) lightWentOff() bool {
	return g.lightOffs != nil && g.lightOffs() != g.redMark.Load()
}

// closedSinceMicOpen reports whether the gate has closed since the
// microphone's last red confirmation began (markMicOpen), or was closed then:
// also when a resume has opened it again since, and isClosed reads open.
func (g *captureGate) closedSinceMicOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.openMarked && g.closings != g.openMark
}

// closingSignal returns a channel that is closed once the gate is closed: at
// once if it is closed now, else at its next closing.
func (g *captureGate) closingSignal() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closing
}

// micHeld reports to onMicHeld that a pause could not release the microphone
// (true), or that it is released after all (false).
func (g *captureGate) micHeld(held bool) {
	if g.onMicHeld != nil {
		g.onMicHeld(held)
	}
}

// micClosed drops the audio the recorder keeps for the next session's start,
// then tells the light that the mic loop closed the microphone: the light
// follows the open microphone, so it may now go off. Audio from before the
// light went off, for a pause, a hold for the light, a stall or a light that
// read back off, never leads a recording that starts after it reads red
// again.
func (g *captureGate) micClosed() {
	if g.dropHeldAudio != nil {
		g.dropHeldAudio()
	}
	if g.afterMicClose != nil {
		g.afterMicClose()
	}
}

// lightUnconfirmed hands a red confirmation that failed for a microphone open
// to the light guard, which holds the gate for the light: a light that reads
// back off while the controller wants it off passes the controller's own
// check, so only a confirmation finds it broken.
func (g *captureGate) lightUnconfirmed(err error) {
	if g.onLightUnconfirmed != nil && errors.Is(err, errLightNotConfirmed) {
		g.onLightUnconfirmed(err)
	}
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

// finishResume ends the resuming state of the latest Resume once its start of
// the microphone has finished, whether it worked or failed (Deepgram then
// connects in the background):
// readiness then reports what is up and what is not. A start that finishes
// while the gate is closed again does not end the next Resume's state; that
// Resume's own start does. A start a pause cancelled (errMicStartPaused) must
// not call it, as the gate can be open again for a later Resume by then;
// startUnderRed keeps that rule for the microphone start-up.
func (g *captureGate) finishResume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.starting = false
	}
}

// isClosed reports the gate: true while no audio passes. It stays true after
// Resume until the pause has settled and the gate opens.
func (g *captureGate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// capturing reports whether the unit records: the gate is open, the
// microphone device is capturing, and its audio reaches the recording.
func (g *captureGate) capturing(mic interface{ IsOpen() bool }) bool {
	return !g.isClosed() && mic.IsOpen() && g.audioFlowing()
}

// sleepUnlessPaused sleeps for d, or until the gate closes or ctx ends. The mic
// loop backs off with it, so a pause closes the device at once, and shutdown
// ends the loop at once, not after the backoff.
func (g *captureGate) sleepUnlessPaused(ctx context.Context, d time.Duration) {
	g.mu.Lock()
	closing := g.closing
	g.mu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-closing:
	case <-ctx.Done():
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

// prepareMicOpen runs beforeMicOpen before a resume opens the microphone, and
// before its red confirmation, so the recording light does not turn red while
// it waits. Capture reports resuming meanwhile. It reports false if the gate
// closes (a pause, or a hold for the light) or ctx ends first; the start then
// opens nothing.
func (g *captureGate) prepareMicOpen(ctx context.Context) bool {
	if g.beforeMicOpen == nil {
		return true
	}
	g.mu.Lock()
	closing := g.closing
	g.preparing = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.preparing = false
		g.mu.Unlock()
	}()
	return g.beforeMicOpen(ctx, closing)
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

// gateStampedWriter receives the gate closing count captured when its Write
// passed the capture gate. It uses the same count to refuse side effects once
// a later gate closing invalidates an in-flight write.
type gateStampedWriter interface {
	WriteWithGateStamp(p []byte, gate *captureGate, closings int) (int, error)
}

// Write records p, only while the light has stayed red, and the gate open,
// since the microphone opened under a red confirmation. It reads the light
// back first (checkLight), so a light that went off, also from outside Ghost
// Wispr (an unplug, the blink(1)'s watchdog, another `ghost-wispr light-off`),
// stops the audio at once, not at the light controller's next look. Once the
// light was not red at some point since the microphone opened (lightWentOff),
// the audio is dropped with errLightWentOff; once the gate closed since then
// (a pause, or a hold for the light, also one a resume has ended), with
// errClosedSinceMicOpen. Some of that audio, and whatever the microphone still
// holds, may have been captured with the light off or during the pause, so
// the mic loop closes the microphone and opens it again under a new red
// confirmation.
func (w *gatedWriter) Write(p []byte) (int, error) {
	g := w.gate
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return 0, errGateClosed
	}
	if g.openMarked && g.closings != g.openMark {
		g.mu.Unlock()
		return 0, errClosedSinceMicOpen
	}
	closings := g.closings
	g.writing++
	g.mu.Unlock()
	defer g.finishWrite()
	if g.checkLight != nil {
		g.checkLight()
	}
	if g.lightWentOff() {
		return 0, errLightWentOff
	}
	if writer, ok := w.dst.(gateStampedWriter); ok {
		n, err := writer.WriteWithGateStamp(p, g, closings)
		if err == nil {
			g.audioAt.Store(time.Now().UnixNano())
			g.noteRecording(closings)
		}
		return n, err
	}
	n, err := w.dst.Write(p)
	if err == nil {
		g.audioAt.Store(time.Now().UnixNano())
		g.noteRecording(closings)
	}
	return n, err
}

// noteRecording runs onRecording at the first audio that reached the
// recording since the gate opened into recording, unless the gate has closed
// since that audio passed it.
func (g *captureGate) noteRecording(closings int) {
	g.mu.Lock()
	due := g.recordingDue && !g.closed && g.closings == closings
	if due {
		g.recordingDue = false
	}
	g.mu.Unlock()
	if due && g.onRecording != nil {
		g.onRecording()
	}
}

// writeTapIfCurrent gives p to tap only while the gate is still in the
// closing generation in which the write began. Holding mu across the tap
// write orders it before a closing's Tap.Reset, or drops it after one.
func (g *captureGate) writeTapIfCurrent(tap io.Writer, p []byte, closings int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.closings != closings {
		return
	}
	_, _ = tap.Write(p)
}

// captureReadiness is what /healthz/ready reads of capture: the gate's
// MicHold, and no_audio while the gate and the microphone are open but no
// audio reaches the recording. The unit records nothing then, so readiness
// fails, and reports no_audio for the microphone.
type captureReadiness struct {
	capture *captureGate
	mic     interface{ IsOpen() bool }
}

func (r captureReadiness) MicHold() string {
	if hold := r.capture.MicHold(); hold != "" {
		return hold
	}
	if r.capture.captureState(r.mic) == captureNoAudio {
		return captureNoAudio
	}
	return ""
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
//   - Every gate closing, a pause or a light hold, blocks new sessions and
//     closes the gate.
//   - The closing then settles: it waits out a session start already under
//     way, Deepgram is asked to finalize, the call and its reply get up to
//     finalizeWait to reach the session that was recording, and that session
//     ends (after any other end of it already under way), so its last words
//     are kept and its recording is closed.
//   - After the next opening drops preroll audio from before the closing,
//     sessions start again, all before audio flows.
//   - Every close of the microphone, which turns the light off, drops the
//     audio the recorder keeps for the next session's start too.
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
// resume into it. A recording that cannot be saved is given up after five
// attempts, and the session ends without it (its raw audio stays on disk), but
// a database that stays stuck or keeps failing, or a write to the recording
// file that never returns, keeps the microphone off until it recovers or the
// process restarts; the capture reports resuming, then stalled, meanwhile.
//
// At shutdown (ctx ends) the pause stops retrying the session end. The gate
// may then open, but it lets nothing through: the mic loops drop every buffer
// written after shutdown began, and end.
func newSessionCaptureGate(ctx context.Context, sessions captureSessions, preroll interface{ DiscardPreroll() }, deepgram func() deepgramFinalizer, finalizeWait, endRetry time.Duration, logf func(string, ...any)) *captureGate {
	g := newCaptureGate(
		sessions.PauseCapture,
		func() { settleCapturePause(ctx, sessions, deepgram(), finalizeWait, endRetry, logf) },
		func() {
			preroll.DiscardPreroll()
			sessions.ResumeCapture()
		},
	)
	g.dropHeldAudio = preroll.DiscardPreroll
	return g
}

func settleCapturePause(ctx context.Context, sessions captureSessions, dg deepgramFinalizer, finalizeWait, endRetry time.Duration, logf func(string, ...any)) {
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
				slog.Warn("finalize deepgram on capture pause", "error", err)
			}
			if finalized != nil {
				select {
				case <-finalized:
				case <-deadline.C:
					logf("capture pause: no final transcript from deepgram within %v", finalizeWait)
				}
			}
		case <-deadline.C:
			slog.Warn("capture pause: deepgram finalize did not return in time", "waited", finalizeWait)
		}
		deadline.Stop()
	}

	// Not bounded by a deadline: the end waits for any other end of the session
	// already under way, then for the recorder's lock, then encodes (bounded by
	// the recorder's encode time limit) and writes the database, which is not.
	// An end that fails leaves the session current, so it is retried until it
	// succeeds, or until shutdown; giving up would open the gate onto that
	// session.
	wait := endRetry
	for {
		err := sessions.ForceEndSession(ctx)
		if err == nil || errors.Is(err, session.ErrNoActiveSession) || ctx.Err() != nil {
			return
		}
		slog.Warn("end session on capture pause failed; retrying", "retry_in", wait, "error", err)
		retry := time.NewTimer(wait)
		select {
		case <-retry.C:
		case <-ctx.Done():
			retry.Stop()
			return
		}
		wait = min(2*wait, captureEndRetryMax)
	}
}
