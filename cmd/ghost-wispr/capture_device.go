package main

import (
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/jabra"
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
// the Jabra: the microphone's capture stream and the speaker's playback
// streams. A Jabra whose mute light changes while one of its streams is open
// sends Phone Mute by itself just after its microphone stream next closes,
// which reads as a person's tap (INFRA-1027 trace, sections 9.6 and 9.7). So
// each operation that opens, closes or probes the capture device, and each
// light write, holds one lock, and the light is written only while no stream
// is open: after a close, and before an open. A playback stream is not opened
// through that lock, because a PortAudio teardown, which a probe runs, waits
// for playback to stop; a playback open waits only for a light write under
// way (lightMu).
//
// It also keeps the span of its operations on CLOCK_MONOTONIC, the clock the
// Jabra's events are stamped with (jabra.StampWithMonotonicClock), so that the
// Jabra mute button can tell a report the Jabra sent because of one from a
// person's tap (ownOperationAt). The close of a playback stream is one of
// those operations.
type captureDevice struct {
	paused  func() bool
	op      sync.Mutex    // held through each capture operation and each light write
	lightMu sync.Mutex    // held through each light write, and while a playback stream counts itself open
	mu      sync.Mutex    // guards the fields below
	open    bool          // a capture stream may be open
	playing int           // playback streams counted open (playbackOpening) and not yet closed
	busy    int           // operations under way
	since   time.Duration // CLOCK_MONOTONIC when the operations ownOperationAt covers began; 0 before any
	until   time.Duration // CLOCK_MONOTONIC when that cover ends, once no operation runs
	lightAt time.Duration // CLOCK_MONOTONIC of the last light write; 0 before any
	// light writes the connected Jabra's light, on while capture is paused;
	// nil without a Jabra.
	light   func(on bool) error
	written bool // light has written shown since it was attached or refreshed
	shown   bool
}

func newCaptureDevice(paused func() bool) *captureDevice {
	return &captureDevice{paused: paused}
}

// openStream runs open, which opens a capture stream, as one operation, and
// writes the light first, then waits for it to settle (jabraLightSettle). open
// reports whether a stream exists afterwards.
func (d *captureDevice) openStream(open func() (holds bool, err error)) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.begin()
	defer d.end()
	d.writeLight()
	d.settleLight()
	holds, err := open()
	d.setOpen(holds)
	return err
}

// closeStream runs closeFn, which closes the capture stream, as one operation,
// and writes the light afterwards.
func (d *captureDevice) closeStream(closeFn func() error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.begin()
	defer d.end()
	err := closeFn()
	d.setOpen(false)
	d.writeLight()
	return err
}

// probe runs fn, which opens and closes capture streams of its own while it
// lists the devices (PortAudio's initialization), as one operation, and
// writes the light afterwards unless a capture stream is open.
func (d *captureDevice) probe(fn func() error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.begin()
	defer d.end()
	err := fn()
	d.writeLight()
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
// the stream.
func (d *captureDevice) playbackClosing(closeStream func()) {
	d.begin()
	closeStream()
	d.mu.Lock()
	d.playing--
	d.mu.Unlock()
	d.end()
	d.syncLight()
}

// attach makes write the light of the Jabra just connected, which may show
// anything, and writes it at once unless a capture stream is open or an
// operation runs. It returns the detach to call before the Jabra is closed.
func (d *captureDevice) attach(write func(on bool) error) (detach func()) {
	d.mu.Lock()
	d.light, d.written = write, false
	d.mu.Unlock()
	d.syncLight()
	return func() {
		d.mu.Lock()
		d.light = nil
		d.mu.Unlock()
	}
}

// refreshLight writes the light again even if it already shows the pause, as
// after a press: the Jabra may change its own light on one.
func (d *captureDevice) refreshLight() {
	d.mu.Lock()
	d.written = false
	d.mu.Unlock()
	d.syncLight()
}

// syncLight writes the light if it may not show the pause, unless a stream is
// open or a capture operation runs. It never waits for either: each capture
// operation, and each playback close, writes the light once no stream is
// open.
func (d *captureDevice) syncLight() {
	if !d.op.TryLock() {
		return
	}
	defer d.op.Unlock()
	paused := d.paused()
	d.mu.Lock()
	stale := !d.written || d.shown != paused
	d.mu.Unlock()
	if stale {
		d.writeLight()
	}
}

// writeLight writes the light as the pause wants it while no capture or
// playback stream is open. The caller holds op.
func (d *captureDevice) writeLight() {
	d.lightMu.Lock()
	defer d.lightMu.Unlock()
	d.mu.Lock()
	write, open := d.light, d.open || d.playing > 0
	d.mu.Unlock()
	if write == nil || open {
		return
	}
	on := d.paused()
	err := write(on)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written, d.shown = err == nil, on
	d.lightAt = jabra.MonotonicNow()
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

// ownOperationAt reports whether at, a CLOCK_MONOTONIC reading, falls within
// an operation on the capture device, or within jabraOwnMuteWindow after one
// ended.
func (d *captureDevice) ownOperationAt(at time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.since == 0 || at < d.since {
		return false
	}
	return d.busy > 0 || at <= d.until
}

func (d *captureDevice) begin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now := jabra.MonotonicNow(); d.busy == 0 && now > d.until {
		d.since = now
	}
	d.busy++
}

func (d *captureDevice) end() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.busy--
	if until := jabra.MonotonicNow() + jabraOwnMuteWindow; until > d.until {
		d.until = until
	}
}

func (d *captureDevice) setOpen(open bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open = open
}

// orderedMic runs each operation of a microphone that opens or closes its
// capture stream through a captureDevice. Stream reads the open stream and
// passes straight through.
type orderedMic struct {
	micStreamer
	device *captureDevice
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
