package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/jabra"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// Linux input-event-codes.h values the fake decodes and encodes on its own,
// so it does not share the code under test.
const (
	fakeEventSize  = 24
	fakeEvKey      = 0x01
	fakeEvLed      = 0x11
	fakeKeyMicMute = 248
	fakeLedMute    = 0x07
)

// jabraFake is the Jabra's evdev node. Like the kernel, it passes a light
// write on to the device only when it changes the kernel's copy of the light,
// so a test can see what the device shows, and can change the device's own
// light behind the kernel's back as a device that lights itself would.
type jabraFake struct {
	presses *io.PipeWriter
	reader  *io.PipeReader

	mu          sync.Mutex
	kernelLight bool
	deviceLight bool
	reports     int
	closed      bool
	// writeErr, when set, fails each light write, as a device being removed.
	writeErr error
}

func newJabraFake() *jabraFake {
	reader, writer := io.Pipe()
	return &jabraFake{presses: writer, reader: reader}
}

func (d *jabraFake) Read(p []byte) (int, error) { return d.reader.Read(p) }

func (d *jabraFake) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	for record := p; len(record) >= fakeEventSize; record = record[fakeEventSize:] {
		if binary.LittleEndian.Uint16(record[16:18]) != fakeEvLed || binary.LittleEndian.Uint16(record[18:20]) != fakeLedMute {
			continue
		}
		on := binary.LittleEndian.Uint32(record[20:24]) != 0
		if on != d.kernelLight {
			d.kernelLight, d.deviceLight = on, on
			d.reports++
		}
	}
	return len(p), nil
}

func (d *jabraFake) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return d.reader.Close()
}

// press sends one press and release of the mute button.
func (d *jabraFake) press(t *testing.T) {
	t.Helper()
	d.key(t, 1)
	d.key(t, 0)
}

// key sends the mute key going down (1) or up (0), stamped now on the
// monotonic clock, as the kernel stamps the Jabra's events. The write returns
// once the reader has taken the event.
func (d *jabraFake) key(t *testing.T, value uint32) {
	t.Helper()
	record := make([]byte, fakeEventSize)
	now := jabra.MonotonicNow()
	binary.LittleEndian.PutUint64(record[0:8], uint64(now/time.Second))
	binary.LittleEndian.PutUint64(record[8:16], uint64(now%time.Second/time.Microsecond))
	binary.LittleEndian.PutUint16(record[16:18], fakeEvKey)
	binary.LittleEndian.PutUint16(record[18:20], fakeKeyMicMute)
	binary.LittleEndian.PutUint32(record[20:24], value)
	if _, err := d.presses.Write(record); err != nil {
		t.Fatalf("send the Jabra mute key: %v", err)
	}
}

// unplug ends the node's reads as a removed device does.
func (d *jabraFake) unplug() {
	_ = d.presses.CloseWithError(errors.New("no such device"))
}

func (d *jabraFake) shows() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deviceLight
}

func (d *jabraFake) reportsSent() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reports
}

// lightsItself changes the device's light without the kernel knowing.
func (d *jabraFake) lightsItself(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deviceLight = on
}

type jabraRig struct {
	capture  *captureGate
	device   *captureDevice
	controls *server.ControlHooks
	warnings *warningsRegistry
	devices  chan *jabraFake
	// connectErr, when set before start, fails every connection.
	connectErr error
	// checkEvery is how often the button compares the light with the pause.
	checkEvery time.Duration
}

// newJabraRig builds the Jabra mute button's surroundings: capture, its
// durable controls, the capture device and the warnings, paused if paused is
// set.
func newJabraRig(paused bool) *jabraRig {
	capture := newCaptureGate(nil, nil, nil)
	if paused {
		capture.Pause()
	}
	controls := newPersistentControls(&pauseStoreFake{}, capture.Pause, capture.RequestResume, nil)
	return &jabraRig{
		capture:    capture,
		device:     newCaptureDevice(capture.IsPaused),
		controls:   &server.ControlHooks{Pause: controls.Pause, Resume: controls.Resume},
		warnings:   newWarningsRegistry(),
		devices:    make(chan *jabraFake, 4),
		checkEvery: time.Millisecond,
	}
}

// openMic opens a capture stream on the Jabra through the rig's capture
// device, as the mic loop does.
func (r *jabraRig) openMic(t *testing.T) {
	t.Helper()
	if err := r.device.openStream(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
}

// closeMic closes the capture stream through the rig's capture device, as the
// mic loop does on a pause.
func (r *jabraRig) closeMic(t *testing.T) {
	t.Helper()
	if err := r.device.closeStream(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// attached reports whether a connected Jabra's light is attached to the
// capture device.
func (r *jabraRig) attached() bool {
	r.device.mu.Lock()
	defer r.device.mu.Unlock()
	return r.device.light != nil
}

// start runs the Jabra mute button against fakes until the test ends. Each
// connection fails with connectErr if it is set, else opens the next device
// plugged in.
func (r *jabraRig) start(t *testing.T) {
	t.Helper()
	button := newJabraMuteButton(r.capture, r.device, r.controls, r.warnings)
	button.connect = func() (io.ReadWriteCloser, error) {
		if r.connectErr != nil {
			return nil, r.connectErr
		}
		select {
		case device := <-r.devices:
			return device, nil
		default:
			return nil, jabra.ErrNoMuteButton
		}
	}
	button.retryEvery, button.checkEvery = time.Millisecond, r.checkEvery
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		button.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func startJabraRig(t *testing.T, paused bool) *jabraRig {
	t.Helper()
	rig := newJabraRig(paused)
	rig.start(t)
	return rig
}

func (r *jabraRig) plugIn() *jabraFake {
	device := newJabraFake()
	r.devices <- device
	return device
}

func (r *jabraRig) warning() string {
	return strings.Join(r.warnings.List(), "; ")
}

// TestJabraPressTogglesCaptureAndTheLight fails if a press of the Jabra's
// mute button does not pause and resume capture, or the light written after
// a press is not the pause that press left. The light check runs too seldom
// here to repair it.
func TestJabraPressTogglesCaptureAndTheLight(t *testing.T) {
	rig := newJabraRig(false)
	rig.checkEvery = time.Hour
	rig.start(t)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra connection to clear its warning", func() bool { return rig.warning() == "" && device.reportsSent() > 0 })
	if device.shows() {
		t.Fatal("the Jabra mute light is on while capture records")
	}

	device.press(t)
	waitForHardware(t, "the first press to pause capture and light the Jabra", func() bool { return rig.capture.IsPaused() && device.shows() })
	device.press(t)
	waitForHardware(t, "the second press to resume capture and darken the Jabra", func() bool { return !rig.capture.IsPaused() && !device.shows() })
}

// TestJabraLostDeviceEndsItsReader fails if the reader of a Jabra that failed
// while it held a press nobody took outlives the device: the button would
// wait for it and never look for the Jabra again.
func TestJabraLostDeviceEndsItsReader(t *testing.T) {
	rig := newJabraRig(false)
	rig.checkEvery = time.Hour
	entered, release := make(chan struct{}), make(chan struct{})
	pause := rig.controls.Pause
	rig.controls.Pause = func() error {
		close(entered)
		<-release
		return pause()
	}
	rig.start(t)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })

	device.press(t)
	<-entered
	// The reader takes this press while the first one's pause runs, and holds it.
	device.key(t, 1)
	device.mu.Lock()
	device.writeErr = errors.New("no such device")
	device.mu.Unlock()
	close(release)
	waitForHardware(t, "the button to report the lost Jabra", func() bool { return strings.Contains(rig.warning(), "Jabra mute button not found") })
}

// TestJabraLightFollowsEveryPauseSource fails if a pause or resume from the
// web page, the API or the power button leaves the Jabra's light behind.
func TestJabraLightFollowsEveryPauseSource(t *testing.T) {
	rig := startJabraRig(t, false)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })

	if err := rig.controls.Pause(); err != nil {
		t.Fatal(err)
	}
	waitForHardware(t, "the Jabra light to follow a web pause", device.shows)
	if err := rig.controls.Resume(); err != nil {
		t.Fatal(err)
	}
	waitForHardware(t, "the Jabra light to follow a web resume", func() bool { return !device.shows() })
}

// TestJabraLightShowsAPauseAtConnect fails if a unit that starts paused, or
// a Jabra plugged in again while paused, does not show the pause, including
// when the device holds a light the kernel does not know of.
func TestJabraLightShowsAPauseAtConnect(t *testing.T) {
	rig := startJabraRig(t, true)
	first := rig.plugIn()
	waitForHardware(t, "the Jabra light to show the pause at start", first.shows)

	first.unplug()
	waitForHardware(t, "a warning that the Jabra is gone", func() bool { return strings.Contains(rig.warning(), "Jabra mute button not found") })
	second := rig.plugIn()
	waitForHardware(t, "the Jabra light to show the pause after a reconnect", func() bool { return second.shows() && rig.warning() == "" })

	// A Jabra the kernel believes lit, while it shows dark: Ghost Wispr
	// restarted after the device changed its own light.
	second.unplug()
	third := newJabraFake()
	third.kernelLight, third.deviceLight = true, false
	rig.devices <- third
	waitForHardware(t, "the light to be sent again to a device that turned it off", third.shows)
}

// TestJabraRefusedResumeRelightsTheJabra fails if a press whose resume is
// refused (no recording light) leaves the Jabra dark while capture stays
// paused: a Jabra that lights itself on a press has already changed its light.
func TestJabraRefusedResumeRelightsTheJabra(t *testing.T) {
	rig := newJabraRig(true)
	rig.controls.Resume = func() error { return errors.New("recording light not red") }
	rig.start(t)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra light to show the pause", device.shows)

	device.lightsItself(false)
	device.press(t)
	waitForHardware(t, "the refused resume to light the Jabra again", device.shows)
	if !rig.capture.IsPaused() {
		t.Fatal("capture resumed after the resume was refused")
	}
}

// TestJabraLightChangesOnlyWhileTheMicrophoneIsClosed fails if the Jabra's
// light changes while a capture stream is open, or is not written after the
// stream closes and before it opens: a Jabra whose light changed while its
// microphone stream was open sends Phone Mute by itself as the stream closes.
// The pause here comes from the web page; the power button's goes through the
// same controls.
func TestJabraLightChangesOnlyWhileTheMicrophoneIsClosed(t *testing.T) {
	rig := startJabraRig(t, false)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })
	rig.openMic(t)

	reports := device.reportsSent()
	if err := rig.controls.Pause(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // about fifty light checks
	if device.reportsSent() != reports || device.shows() {
		t.Fatalf("the light changed while the microphone was open: %d light reports, shows %t", device.reportsSent()-reports, device.shows())
	}
	rig.closeMic(t)
	if !device.shows() {
		t.Fatal("the Jabra light does not show the pause once the microphone closed")
	}

	if err := rig.controls.Resume(); err != nil {
		t.Fatal(err)
	}
	waitForHardware(t, "the Jabra light to follow the resume while the microphone is closed", func() bool { return !device.shows() })
	device.lightsItself(true) // the Jabra lights itself; the open must write the light first
	rig.openMic(t)
	if device.shows() {
		t.Fatal("the microphone opened before the Jabra light was written")
	}
	reports = device.reportsSent()
	device.lightsItself(true)
	time.Sleep(50 * time.Millisecond)
	if device.reportsSent() != reports {
		t.Fatalf("the light was written %d times while the microphone was open", device.reportsSent()-reports)
	}
}

// TestJabraOwnReportWindowNeverHoldsAPause fails if a press just after Ghost
// Wispr closed the microphone is dropped while capture records: anyone may
// pause at any time, and only a press that would resume is held back.
func TestJabraOwnReportWindowNeverHoldsAPause(t *testing.T) {
	rig := startJabraRig(t, false)
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", rig.attached)
	rig.openMic(t)
	rig.closeMic(t) // a reconnect, with capture still on
	device.press(t)
	waitForHardware(t, "a press within the window to pause capture", rig.capture.IsPaused)
}

// TestJabraPermissionWarningNamesTheUdevRule fails if a Jabra node the
// service may not open is reported as a missing Jabra, which hides the fix.
func TestJabraPermissionWarningNamesTheUdevRule(t *testing.T) {
	rig := newJabraRig(false)
	rig.connectErr = fmt.Errorf("open /dev/input/event1: %w", os.ErrPermission)
	rig.start(t)
	waitForHardware(t, "a permission warning", func() bool {
		return strings.Contains(rig.warning(), "not accessible") && strings.Contains(rig.warning(), "udev rule")
	})
}

// TestOpenJabraOpensTheNodeToWrite fails if the Jabra's node is opened
// read-only: the light write would then fail on the unit, and the button
// would never toggle. It also fails if connectJabra uses a node that refuses
// the monotonic clock, whose stamps the press guard could not compare.
func TestOpenJabraOpensTheNodeToWrite(t *testing.T) {
	dir := t.TempDir()
	devices := filepath.Join(dir, "devices")
	block := "N: Name=\"Jabra Speak2 75\"\nH: Handlers=kbd leds event1\nB: KEY=fff 0 0 0 40 100000000000000 0 0 0\nB: LED=80\n"
	if err := os.WriteFile(devices, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "event1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	device, err := openJabra(devices, dir)
	if err != nil {
		t.Fatalf("open the Jabra: %v", err)
	}
	t.Cleanup(func() { _ = device.Close() })
	if _, err := device.Write(make([]byte, fakeEventSize)); err != nil {
		t.Fatalf("write to the Jabra's node: %v", err)
	}
	// A regular file is no evdev node: it refuses the clock.
	if connected, err := connectJabra(devices, dir); err == nil {
		_ = connected.Close()
		t.Fatal("connectJabra used a node that refused the monotonic clock")
	}
}
