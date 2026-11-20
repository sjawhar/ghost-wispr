package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/jabra"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

// jabraOwnMuteWindow is how long after an operation on the capture device
// ends that a Jabra mute press is taken for the Jabra's own report while
// capture is paused. On the office Pi the Jabra Speak2 75 sent Phone Mute by
// itself 12 ms and 15 ms after its microphone stream closed (kernel time
// stamps, INFRA-1027 acceptance trace section 9.6). The press's kernel time
// stamp, on the monotonic clock, is compared, so the window does not have to
// cover Ghost Wispr's own scheduling, and no wall-clock step moves it: 100 ms
// is more than six times the longest delay measured, and shorter than a person
// takes to see the unit pause and tap again.
const jabraOwnMuteWindow = 100 * time.Millisecond

// jabraLightSettle is how long after a light write no stream opens on the
// Jabra. The write returns before the light reaches the device: the kernel
// sends it from a work queue (drivers/hid/hid-input.c, hidinput_input_event
// schedules hid->led_work), and the office Pi's Jabra is a full-speed USB
// device whose HID interface has no OUT endpoint, so usbhid sends the report as
// a control transfer, in a 1 ms frame. A stream opened before the report
// arrives is open while the light changes. 20 ms is ten times a work-queue run
// and a control frame; it was not measured on the unit.
const jabraLightSettle = 20 * time.Millisecond

// captureDevice orders the Jabra's mute light against every audio stream on
// the Jabra: the microphone's capture stream, the paused listener's capture
// stream, and the speaker's playback streams. A Jabra whose mute light
// changes while one of its streams is open sends Phone Mute by itself just
// after its microphone stream next closes, which reads as a person's tap
// (INFRA-1027 trace, sections 9.6 and 9.7). So each operation that opens,
// closes or probes the capture device, and each light write, holds one lock,
// and the light is written only while no stream is open: after a close, and
// before an open. A playback stream is not opened through that lock, because
// a PortAudio teardown, which a probe runs, waits for playback to stop; a
// playback open waits only for a light write under way (lightMu).
//
// It also hands the microphone between the paused listener and the
// recording microphone (INFRA-4592 R2): the listener's stream closes before
// the recording microphone prepares to open, opens or PortAudio scans
// (closeListener), and opens only while the recording microphone's stream is
// closed (openListener).
//
// It keeps the span of its operations on CLOCK_MONOTONIC, the clock the
// Jabra's events are stamped with (jabra.StampWithMonotonicClock), so that the
// Jabra mute button can tell a report the Jabra sent because of one from a
// person's tap (ownOperationAt). The close of a playback stream, and the
// listener's opens and closes, are among those operations.
type captureDevice struct {
	muted   func() bool
	op      sync.Mutex    // held through each capture operation and each light write
	lightMu sync.Mutex    // held through each light write, and while a playback stream counts itself open
	mu      sync.Mutex    // guards the fields below
	open    bool          // a capture stream of the recording microphone may be open
	playing int           // playback streams counted open (playbackOpening) and not yet closed
	busy    int           // operations under way
	spans   []opSpan      // recent stretches of operations, oldest first; the last one is open while busy > 0
	lightAt time.Duration // CLOCK_MONOTONIC of the last light write; 0 before any
	// ring, when set (showPaused), is what the Jabra's ring shows in place of
	// muted: the Jabra button's count.
	ring func() bool
	// light writes the connected Jabra's light, on while the ring shows
	// Muted (ringMuted); nil without a Jabra.
	light   func(on bool) error
	written bool // light has written shown since it was attached or refreshed
	shown   bool
	// silent is closed, and replaced, each time a playback stream closes, so
	// a microphone open waiting for the speech to end (prepareLightForOpen)
	// looks again.
	silent chan struct{}
	// listening is set while the paused listener's stream is open, from its
	// open (openListener) until its close has returned (closeListenerStream);
	// listenerGone is signalled at each close. stopListener asks the
	// listener to close it, and returns at once (setListener).
	listening    bool
	listenerGone *sync.Cond
	stopListener func()
	// attachChanged, when set, runs after a Jabra's light is attached or
	// detached: the paused listener can run only with the button attached.
	attachChanged func()
}

// newCaptureDevice makes the capture device; muted reports whether the unit
// is in the closed state, the only state whose ring shows Muted.
func newCaptureDevice(muted func() bool) *captureDevice {
	d := &captureDevice{muted: muted, silent: make(chan struct{})}
	d.listenerGone = sync.NewCond(&d.mu)
	return d
}

// showPaused makes the Jabra's ring show ring in place of the closed state.
// The Jabra button counts the unit as resumed while its own resume checks
// the light, before the switch moves, so the person's next press, on an
// unmuted ring, closes the microphone (review 5968112897 of #79).
func (d *captureDevice) showPaused(ring func() bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ring = ring
}

// ringMuted reports whether the Jabra's ring should show Muted: ring if it is
// set, else the closed state. Neither shows Muted while the unit records or
// listens: the listener holds an open stream on the Jabra, whose ring must
// not change while it is open, and the microphone is open then.
func (d *captureDevice) ringMuted() bool {
	d.mu.Lock()
	ring := d.ring
	d.mu.Unlock()
	if ring != nil {
		return ring()
	}
	return d.muted()
}

// openStream runs open, which opens a capture stream, as one operation, and
// writes the light first, then waits for it to settle (jabraLightSettle). open
// reports whether a stream exists afterwards. The paused listener's stream
// closes first.
func (d *captureDevice) openStream(open func() (holds bool, err error)) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.closeListener()
	d.begin()
	defer d.end()
	d.writeLight()
	d.settleLight()
	holds, err := open()
	d.setOpen(holds)
	return err
}

// closeStream runs closeFn, which closes the capture stream, as one operation,
// and writes the light afterwards. A close that leaves the stream open, as one
// whose read is still running (audio.ErrMicStillReading), keeps it counted
// open, so the light is not written while the stream is open, and the paused
// listener does not open.
func (d *captureDevice) closeStream(closeFn func() error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.begin()
	defer d.end()
	err := closeFn()
	d.setOpen(errors.Is(err, audio.ErrMicStillReading))
	d.writeLight()
	return err
}

// probe runs fn, which opens and closes capture streams of its own while it
// lists the devices (PortAudio's initialization), as one operation, once the
// paused listener's stream has closed and the last light write has settled
// (jabraLightSettle), and writes the light afterwards unless a capture stream
// is open.
func (d *captureDevice) probe(fn func() error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.closeListener()
	d.begin()
	defer d.end()
	d.settleLight()
	err := fn()
	d.writeLight()
	return err
}

// setListener names how to ask the paused listener to close its stream: stop
// must return at once (local.Listener.Interrupt).
func (d *captureDevice) setListener(stop func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopListener = stop
}

// closeListener asks the paused listener to close its stream, if it is open,
// and waits until it has. Every recording microphone open, the wait before
// one (prepareLightForOpen) and every PortAudio scan call it first: an ALSA
// stream still open would make the scan miss the Jabra, and the recording
// microphone may open only once the listener's has closed.
func (d *captureDevice) closeListener() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.listening {
		if d.stopListener != nil {
			d.stopListener()
		}
		d.listenerGone.Wait()
	}
}

// openListener runs open, which opens the paused listener's capture stream,
// as one operation, once want holds and the recording microphone's stream is
// closed, also one a close could not release (audio.ErrMicStillReading);
// otherwise it opens nothing and returns local.ErrNotNow. It writes the light
// first, then waits for it to settle, as openStream does: the ring shows
// unmuted while the listener listens, and cannot change while its stream is
// open.
func (d *captureDevice) openListener(want func() bool, open func() error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.mu.Lock()
	recording := d.open
	d.mu.Unlock()
	if recording || !want() {
		return local.ErrNotNow
	}
	d.begin()
	defer d.end()
	d.writeLight()
	d.settleLight()
	if err := open(); err != nil {
		return err
	}
	d.mu.Lock()
	d.listening = true
	d.mu.Unlock()
	return nil
}

// closeListenerStream runs closeFn, which closes the paused listener's
// stream, as an operation ownOperationAt covers, then counts the stream
// closed, wakes closeListener, and writes the light if it may. It takes no
// operation lock: an openStream that holds it waits in closeListener for this
// close, and writes the light itself.
func (d *captureDevice) closeListenerStream(closeFn func() error) error {
	d.begin()
	err := closeFn()
	d.mu.Lock()
	d.listening = false
	d.listenerGone.Broadcast()
	d.mu.Unlock()
	d.end()
	d.syncLight()
	return err
}

// playbackOpening counts a speaker playback stream as open, once any light
// write under way has finished and settled (jabraLightSettle), so that no
// light is written until the stream closes (playbackClosing). The speaker calls
// it just before it opens the stream.
func (d *captureDevice) playbackOpening() {
	d.lightMu.Lock()
	defer d.lightMu.Unlock()
	d.settleLight()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.playing++
}

// playbackClosing closes a speaker playback stream with closeStream, as an
// operation ownOperationAt covers, then writes the light if it may not show
// the pause and no stream is open. The speaker calls it once it is done with
// the stream. A closeStream that panics still ends the operation and stops
// counting the stream, so nothing waits for that playback for good.
func (d *captureDevice) playbackClosing(closeStream func()) {
	func() {
		d.begin()
		defer func() {
			d.mu.Lock()
			d.playing--
			close(d.silent)
			d.silent = make(chan struct{})
			d.mu.Unlock()
			d.end()
		}()
		closeStream()
	}()
	d.syncLight()
}

// prepareLightForOpen makes the ring show what it must before a microphone
// opens, so the Jabra's ring never reads Muted while the unit records: the
// light cannot change once the microphone is open. It closes the paused
// listener's stream first. While the light may not show the state and a
// playback stream is open, it waits for the playback to close, then writes
// the light; a playback that opens as it writes holds the write back, and it
// waits for that one too. It returns false, having written nothing, if cancel
// closes (a pause) or ctx ends first. A Jabra light that fails to write does
// not hold the microphone back: the button reconnects the Jabra.
func (d *captureDevice) prepareLightForOpen(ctx context.Context, cancel <-chan struct{}) bool {
	d.closeListener()
	for {
		select {
		case <-cancel:
			return false
		case <-ctx.Done():
			return false
		default:
		}
		paused := d.ringMuted()
		d.op.Lock()
		d.mu.Lock()
		stale := d.light != nil && (!d.written || d.shown != paused)
		playing, silent := d.playing > 0, d.silent
		d.mu.Unlock()
		if !stale || !playing {
			if stale && d.writeLight() {
				d.op.Unlock()
				continue // the next pass waits for that playback
			}
			d.op.Unlock()
			return true
		}
		d.op.Unlock()
		select {
		case <-silent:
		case <-cancel:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// attach makes write the light of the Jabra just connected, which may show
// anything, and writes it at once unless a capture stream is open or an
// operation runs. It returns the detach to call before the Jabra is closed.
// Both tell attachChanged.
func (d *captureDevice) attach(write func(on bool) error) (detach func()) {
	d.mu.Lock()
	d.light, d.written = write, false
	changed := d.attachChanged
	d.mu.Unlock()
	d.syncLight()
	if changed != nil {
		changed()
	}
	return func() {
		d.mu.Lock()
		d.light = nil
		changed := d.attachChanged
		d.mu.Unlock()
		if changed != nil {
			changed()
		}
	}
}

// jabraAttached reports whether a Jabra's mute button is attached: its light
// is.
func (d *captureDevice) jabraAttached() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.light != nil
}

// onAttachChanged makes changed run after each attach and detach.
func (d *captureDevice) onAttachChanged(changed func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attachChanged = changed
}

// refreshLight writes the light again even if it already shows the pause, as
// after a press the kernel stamped at: the Jabra may change its own light on
// one. A light written after at already overrides that change, since the
// kernel sends a written light to the Jabra after the write returns, so it is
// not asked for again.
func (d *captureDevice) refreshLight(at time.Duration) {
	d.mu.Lock()
	if d.lightAt < at {
		d.written = false
	}
	d.mu.Unlock()
	d.syncLight()
}

// syncLight writes the light if it may not show the state, unless a stream is
// open or a capture operation runs. It never waits for either: each capture
// operation, each listener close and each playback close writes the light
// once no stream is open.
func (d *captureDevice) syncLight() {
	if !d.op.TryLock() {
		return
	}
	defer d.op.Unlock()
	paused := d.ringMuted()
	d.mu.Lock()
	stale := !d.written || d.shown != paused
	d.mu.Unlock()
	if stale {
		d.writeLight()
	}
}

// writeLight writes the light as ringMuted wants it while no capture stream,
// the recording microphone's or the paused listener's, and no playback stream
// is open. It reports whether an open playback stream held the write back.
// The caller holds op.
func (d *captureDevice) writeLight() (heldForPlayback bool) {
	d.lightMu.Lock()
	defer d.lightMu.Unlock()
	d.mu.Lock()
	write, open, playing := d.light, d.open || d.listening, d.playing > 0
	d.mu.Unlock()
	if write == nil || open || playing {
		return write != nil && playing
	}
	on := d.ringMuted()
	err := write(on)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written, d.shown = err == nil, on
	d.lightAt = jabra.MonotonicNow()
	return false
}

// settleLight waits until jabraLightSettle has passed since the last light
// write. The caller holds op or lightMu, so no light write comes meanwhile.
func (d *captureDevice) settleLight() {
	d.mu.Lock()
	at := d.lightAt
	d.mu.Unlock()
	if at == 0 {
		return
	}
	if wait := at + jabraLightSettle - jabra.MonotonicNow(); wait > 0 {
		time.Sleep(wait)
	}
}

// opSpan is one stretch of operations on the capture device, on
// CLOCK_MONOTONIC: from the first one's begin until jabraOwnMuteWindow after
// the last one's end.
type opSpan struct{ since, until time.Duration }

// jabraSpansKept is how long a stretch of operations is kept once its cover
// has ended, so that a report the button loop reads late, after later
// operations began, is still checked against the operation it came with.
const jabraSpansKept = time.Minute

// ownOperationAt reports whether at, a CLOCK_MONOTONIC reading, falls within
// a recent operation on the capture device, or within jabraOwnMuteWindow after
// one ended.
func (d *captureDevice) ownOperationAt(at time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, span := range d.spans {
		if at < span.since {
			continue
		}
		if at <= span.until || (i == len(d.spans)-1 && d.busy > 0) {
			return true
		}
	}
	return false
}

func (d *captureDevice) begin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := jabra.MonotonicNow()
	if d.busy == 0 && (len(d.spans) == 0 || now > d.spans[len(d.spans)-1].until) {
		kept := d.spans[:0]
		for _, span := range d.spans {
			if now-span.until <= jabraSpansKept {
				kept = append(kept, span)
			}
		}
		d.spans = kept
		d.spans = append(d.spans, opSpan{since: now, until: now})
	}
	d.busy++
}

func (d *captureDevice) end() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.busy--
	last := &d.spans[len(d.spans)-1]
	if until := jabra.MonotonicNow() + jabraOwnMuteWindow; until > last.until {
		last.until = until
	}
}

func (d *captureDevice) setOpen(open bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open = open
}

// errRingShowsPause is the error orderedMic's Stream returns when audio comes
// while the Jabra's ring may show the pause: no audio passes then.
var errRingShowsPause = errors.New("the Jabra's ring may show the pause")

// ringMayShowPause reports whether the attached Jabra's ring may show Muted:
// its light was last written to show the pause, or its state is not known, as
// when the Jabra connected or a press came while a stream was open, or a light
// write failed.
func (d *captureDevice) ringMayShowPause() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.light != nil && (!d.written || d.shown)
}

// orderedMic runs each operation of a microphone that opens or closes its
// capture stream through a captureDevice. Stream reads the open stream, and
// refuses each buffer while the Jabra's ring may show the pause
// (ringMayShowPause), with errRingShowsPause: the light cannot change while
// the stream is open, so the mic loop closes the microphone, darkens the ring
// and opens it again before any audio passes. The capture gate's
// beforeMicOpen must be the device's prepareLightForOpen, as main sets it:
// that wait is what darkens the ring before the microphone opens again, and
// without it the mic loop would reopen at once while speech holds the light.
type orderedMic struct {
	micStreamer
	device *captureDevice
}

func (m orderedMic) Stream(w io.Writer) error {
	return m.micStreamer.Stream(ringGuard{dst: w, device: m.device})
}

// ringGuard passes audio to dst only while the Jabra's ring cannot show the
// pause.
type ringGuard struct {
	dst    io.Writer
	device *captureDevice
}

func (g ringGuard) Write(p []byte) (int, error) {
	if g.device.ringMayShowPause() {
		return 0, errRingShowsPause
	}
	return g.dst.Write(p)
}

// Start starts a stream the microphone already holds, which stays open if the
// start fails.
func (m orderedMic) Start() error {
	return m.device.openStream(func() (bool, error) { return true, m.micStreamer.Start() })
}

func (m orderedMic) Open() error {
	return m.device.openStream(func() (bool, error) {
		err := m.micStreamer.Open()
		return err == nil, err
	})
}

func (m orderedMic) Reopen() error {
	return m.device.openStream(func() (bool, error) {
		err := m.micStreamer.Reopen()
		return err == nil, err
	})
}

func (m orderedMic) Close() error {
	return m.device.closeStream(m.micStreamer.Close)
}
