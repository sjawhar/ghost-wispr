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

// captureDevice orders the Jabra's mute light against the microphone's capture
// stream. A Jabra whose mute light changes while its microphone stream is open
// sends Phone Mute by itself just after that stream closes, which reads as a
// person's tap. So each operation that opens, closes or probes the capture
// device, and each light write, holds one lock, and the light is written only
// while no capture stream is open: after a close, and before an open.
//
// It also keeps the span of its operations on CLOCK_MONOTONIC, the clock the
// Jabra's events are stamped with (jabra.StampWithMonotonicClock), so that the
// Jabra mute button can tell a report the Jabra sent because of one from a
// person's tap (ownOperationAt).
type captureDevice struct {
	paused func() bool
	op     sync.Mutex    // held through each operation and each light write
	mu     sync.Mutex    // guards the fields below
	open   bool          // a capture stream may be open
	busy   bool          // an operation runs
	since  time.Duration // CLOCK_MONOTONIC when the operations ownOperationAt covers began; 0 before any
	until  time.Duration // CLOCK_MONOTONIC when that cover ends, once no operation runs
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
// writes the light first. open reports whether a stream exists afterwards.
func (d *captureDevice) openStream(open func() (holds bool, err error)) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.begin()
	defer d.end()
	d.writeLight()
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

// syncLight writes the light if it may not show the pause, unless a capture
// stream is open or an operation runs. It never waits for either: each
// operation writes the light once no stream is open.
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

// writeLight writes the light as the pause wants it while no capture stream
// is open. The caller holds op.
func (d *captureDevice) writeLight() {
	d.mu.Lock()
	write, open := d.light, d.open
	d.mu.Unlock()
	if write == nil || open {
		return
	}
	on := d.paused()
	err := write(on)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written, d.shown = err == nil, on
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
	return d.busy || at <= d.until
}

func (d *captureDevice) begin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now := jabra.MonotonicNow(); now > d.until {
		d.since = now
	}
	d.busy = true
}

func (d *captureDevice) end() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.busy = false
	d.until = jabra.MonotonicNow() + jabraOwnMuteWindow
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
