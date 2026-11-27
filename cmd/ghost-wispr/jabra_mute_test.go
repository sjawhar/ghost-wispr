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
	"sync/atomic"
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
	capture    *captureGate
	device     *captureDevice
	persistent *persistentControls
	controls   *server.ControlHooks
	warnings   *warningsRegistry
	devices    chan *jabraFake
	// connectErr, when set before start, fails every connection.
	connectErr error
	// checkEvery is how often the button compares the light with the pause.
	checkEvery time.Duration
}

// newJabraRig builds the Jabra mute button's surroundings: capture, its
// durable controls, the capture device and the warnings, paused if paused is
// set. With no listener, the unit is the two-state machine (useListening).
func newJabraRig(paused bool) *jabraRig {
	capture := newCaptureGate(nil, nil, nil)
	if paused {
		capture.Pause()
	}
	controls := newPersistentControls(&pauseStoreFake{}, capture.Pause, capture, nil)
	return &jabraRig{
		capture:    capture,
		device:     newCaptureDevice(capture.inClosedState),
		persistent: controls,
		controls:   &server.ControlHooks{Pause: controls.Pause, Close: controls.Close, Resume: controls.Resume},
		warnings:   newWarningsRegistry(),
		devices:    make(chan *jabraFake, 4),
		checkEvery: time.Millisecond,
	}
}

// useListening gives the rig's controls the three states, with a listener
// that can always run: a pause goes to listening.
func (r *jabraRig) useListening() {
	r.persistent.useListener(r.capture, func() {}, func() bool { return true }, r.device.closeListener)
}

// openListener opens the paused listener's stream through the rig's capture
// device, as the listener does in listening.
func (r *jabraRig) openListener(t *testing.T) {
	t.Helper()
	if err := r.device.openListener(func() bool { return r.capture.micState() == micListening }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// closeListener closes the listener's stream, as the listener does when the
// unit leaves listening.
func (r *jabraRig) closeListener(t *testing.T) {
	t.Helper()
	if err := r.device.closeListenerStream(func() error { return nil }); err != nil {
		t.Fatal(err)
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
	ctx, cancel := context.WithCancel(context.Background())
	done := r.run(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// run runs the Jabra mute button against fakes until ctx ends, as start does.
// The returned channel closes once the button's run has returned.
func (r *jabraRig) run(ctx context.Context) <-chan struct{} {
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		button.run(ctx)
	}()
	return done
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
// mute button does not close the microphone, from recording and from
// listening, and resume a closed unit, or the ring written after a press is
// not ringMuted, true in closed only. The light check runs too seldom here to
// repair it: the light must come from the press and the listener's close
// themselves. Each wait is on the state it names, bounded only by testWait.
func TestJabraPressTogglesCaptureAndTheLight(t *testing.T) {
	for _, from := range []micState{micRecording, micListening} {
		t.Run(from.String(), func(t *testing.T) {
			wait := func(what string, cond func() bool) { t.Helper(); waitForHardwareWithin(t, testWait, what, cond) }
			rig := newJabraRig(false)
			rig.checkEvery = time.Hour
			rig.start(t)
			device := rig.plugIn()
			wait("the Jabra connection to clear its warning", func() bool { return rig.warning() == "" && device.reportsSent() > 0 })
			if from == micListening {
				rig.useListening()
				if err := rig.controls.Pause(); err != nil {
					t.Fatal(err)
				}
				rig.openListener(t)
			}
			if device.shows() || rig.capture.micState() != from {
				t.Fatalf("control: in %v, the ring shows Muted %t", rig.capture.micState(), device.shows())
			}

			device.press(t)
			if from == micListening {
				wait("the press to close the microphone", rig.capture.inClosedState)
				if device.shows() {
					t.Fatal("the ring changed while the listener's stream was open")
				}
				rig.closeListener(t) // the listener leaves at once
			}
			wait("the first press to close the microphone and light the Jabra", func() bool { return rig.capture.inClosedState() && device.shows() })
			// A person's second press, outside the listener close's cover: the
			// Jabra's own report would come within jabraOwnMuteWindow of it.
			wait("the listener close's cover to end", func() bool { return !rig.device.ownOperationAt(jabra.MonotonicNow()) })
			device.press(t)
			wait("the second press to resume capture and darken the Jabra", func() bool { return !rig.capture.IsPaused() && !device.shows() })
		})
	}
}

// TestJabraLostDeviceEndsItsReader fails if the reader of a Jabra that failed
// while it held a press nobody took outlives the device: the button would
// wait for it and never look for the Jabra again.
func TestJabraLostDeviceEndsItsReader(t *testing.T) {
	rig := newJabraRig(false)
	rig.checkEvery = time.Hour
	entered, release := make(chan struct{}), make(chan struct{})
	// A press on a recording unit closes the microphone (the Jabra's off).
	closeMic := rig.controls.Close
	rig.controls.Close = func() error {
		close(entered)
		<-release
		return closeMic()
	}
	rig.start(t)
	freeClose := sync.OnceFunc(func() { close(release) })
	t.Cleanup(freeClose) // before the rig's cleanup, which waits for the press
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })

	device.press(t)
	select {
	case <-entered:
	case <-time.After(testWait):
		t.Fatal("the press on a recording unit did not close the microphone")
	}
	// The reader takes this press while the first one's close runs, and holds it.
	device.key(t, 1)
	device.mu.Lock()
	device.writeErr = errors.New("no such device")
	device.mu.Unlock()
	freeClose()
	waitForHardware(t, "the button to report the lost Jabra", func() bool { return strings.Contains(rig.warning(), "Jabra mute button not found") })
}

// TestTheJabraButtonEndsOnlyOnceItsPressesHave fails if the Jabra button's run
// returns while a press's resume, which runs in the background, still runs:
// nothing would then wait for it or for the light written after it, as
// runPowerButton waits for its own (review 5967147417 of #79).
func TestTheJabraButtonEndsOnlyOnceItsPressesHave(t *testing.T) {
	rig := newJabraRig(true)
	rig.checkEvery = time.Hour
	resuming, release := make(chan struct{}), make(chan struct{})
	resume := rig.controls.Resume
	rig.controls.Resume = func() error {
		close(resuming)
		<-release
		return resume()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := rig.run(ctx)
	released := false
	t.Cleanup(func() {
		cancel()
		if !released {
			close(release)
		}
		<-done
	})
	device := rig.plugIn()
	waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })

	device.press(t)
	select {
	case <-resuming:
	case <-time.After(5 * time.Second):
		t.Fatal("the press started no resume within 5s")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("the Jabra button's run returned while its press's resume still ran")
	case <-time.After(200 * time.Millisecond):
	}
	released = true
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the Jabra button's run did not return within 5s of its press's resume")
	}
	if rig.capture.IsPaused() {
		t.Fatal("the press's resume left capture paused")
	}
}

// TestJabraLightFollowsEveryPauseSource fails if a pause, close or resume from
// the web page, the API or the power button leaves the Jabra's ring behind
// ringMuted: Muted in closed only. With no listener every pause closes, so it
// shows Muted; with one, a pause listens, and the ring stays unmuted.
func TestJabraLightFollowsEveryPauseSource(t *testing.T) {
	for _, listener := range []bool{false, true} {
		t.Run(fmt.Sprintf("listener %t", listener), func(t *testing.T) {
			rig := startJabraRig(t, false)
			if listener {
				rig.useListening()
			}
			device := rig.plugIn()
			waitForHardware(t, "the Jabra to connect", func() bool { return device.reportsSent() > 0 })

			if err := rig.controls.Pause(); err != nil {
				t.Fatal(err)
			}
			if listener {
				time.Sleep(50 * time.Millisecond) // about fifty light checks
				if device.shows() || rig.capture.micState() != micListening {
					t.Fatalf("a web pause with the listener: %v, ring Muted %t; want listening, unmuted", rig.capture.micState(), device.shows())
				}
				if err := rig.controls.Close(); err != nil {
					t.Fatal(err)
				}
			}
			waitForHardware(t, "the Jabra light to follow a web pause or close", device.shows)
			if err := rig.controls.Resume(); err != nil {
				t.Fatal(err)
			}
			waitForHardware(t, "the Jabra light to follow a web resume", func() bool { return !device.shows() })
		})
	}
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
// Wispr's own operation on the capture device is dropped when it would close
// the microphone: anyone may close it at any time, from recording or from
// listening, and only a press that would move the unit to recording is held
// back.
func TestJabraOwnReportWindowNeverHoldsAPause(t *testing.T) {
	t.Run("recording", func(t *testing.T) {
		rig := startJabraRig(t, false)
		device := rig.plugIn()
		waitForHardware(t, "the Jabra to connect", rig.attached)
		rig.openMic(t)
		rig.closeMic(t) // a reconnect, with capture still on
		device.press(t)
		waitForHardware(t, "a press within the window to close the microphone", rig.capture.inClosedState)
	})
	t.Run("listening", func(t *testing.T) {
		rig := startJabraRig(t, false)
		rig.useListening()
		device := rig.plugIn()
		waitForHardware(t, "the Jabra to connect", rig.attached)
		if err := rig.controls.Pause(); err != nil {
			t.Fatal(err)
		}
		rig.openListener(t) // the listener's open, an own operation
		if !rig.device.ownOperationAt(jabra.MonotonicNow()) {
			t.Fatal("control: the listener's open is not an own operation")
		}
		device.press(t)
		waitForHardware(t, "a press within the window to close a listening microphone", rig.capture.inClosedState)
	})
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

// TestJabraClockRefusalWarningNamesTheClock fails if a Jabra node that is
// found and opened, but refuses the monotonic clock the press guard needs, is
// reported as a missing Jabra. The button stays off then, which is right, but
// the page must name the cause the log names.
func TestJabraClockRefusalWarningNamesTheClock(t *testing.T) {
	dir := t.TempDir()
	devices := filepath.Join(dir, "devices")
	block := "N: Name=\"Jabra Speak2 75\"\nH: Handlers=kbd leds event1\nB: KEY=fff 0 0 0 40 100000000000000 0 0 0\nB: LED=80\n"
	if err := os.WriteFile(devices, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	// A regular file is no evdev node: it refuses the clock request.
	if err := os.WriteFile(filepath.Join(dir, "event1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, clockErr := connectJabra(devices, dir)
	if clockErr == nil {
		t.Fatal("control: a regular file took the evdev clock request")
	}
	rig := newJabraRig(false)
	rig.connectErr = clockErr
	rig.start(t)
	waitForHardware(t, "a warning that names the clock", func() bool {
		return strings.Contains(rig.warning(), "clock") && !strings.Contains(rig.warning(), "not found")
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

// TestJabraOwnReportReadLateStaysCovered fails if the Jabra's own Phone Mute,
// stamped 12 ms after the pause's microphone close but read by the button loop
// only after a later operation began (here a TTS playback close 150 ms later,
// while the loop was still busy with the tap that paused), resumes the unit:
// each recent operation's cover must hold for a report stamped in it, not only
// the latest one's. The control has no later operation.
func TestJabraOwnReportReadLateStaysCovered(t *testing.T) {
	runParallel(t)
	for _, speechEnds := range []bool{false, true} {
		name := "control, no later operation"
		if speechEnds {
			name = "a speech ends before the report is read"
		}
		t.Run(name, func(t *testing.T) {
			rig := newJabraRig(false)
			button := newJabraMuteButton(rig.capture, rig.device, rig.controls, rig.warnings)
			rig.openMic(t) // recording
			if speechEnds {
				rig.device.playbackOpening() // speech plays through the pause
			}
			button.press(jabra.MonotonicNow()) // a person's tap pauses
			if !rig.capture.IsPaused() {
				t.Fatal("control: the tap did not pause the unit")
			}
			rig.closeMic(t) // the mic loop closes the microphone for the pause
			report := jabra.MonotonicNow() + 12*time.Millisecond
			time.Sleep(150 * time.Millisecond) // the button loop is still busy with the tap
			if speechEnds {
				rig.device.playbackClosing(func() error { return nil })
			}
			button.press(report)
			if !rig.capture.IsPaused() {
				t.Fatal("the Jabra's own report, read late, resumed the paused unit")
			}
		})
	}
}

// TestAResumeTapCostsNoReopen fails if a person's Jabra tap that resumes the
// unit makes the mic loop refuse the first buffer and reopen the microphone,
// although the resume already darkened the ring: press resumes, the controls
// store the status change, and only then does press ask for the light again,
// which can come after the microphone opened. statusWrite stands for that
// SQLite write, and beforeGateOpen's 5 ms for the blink(1)'s red confirmation.
// The control has a status write quicker than the red confirmation. A press
// that resumes returns before its resume ends (buttonToggle), so the tap's
// resume is waited for.
func TestAResumeTapCostsNoReopen(t *testing.T) {
	runParallel(t)
	for _, statusWrite := range []time.Duration{0, 30 * time.Millisecond} {
		name := "control, the status write is quicker than the red confirmation"
		if statusWrite > 0 {
			name = "the status write is slower than the red confirmation"
		}
		t.Run(name, func(t *testing.T) {
			capture := newCaptureGate(nil, nil, nil)
			capture.Pause()
			capture.beforeGateOpen = func() error { time.Sleep(5 * time.Millisecond); return nil }
			device := newCaptureDevice(capture.IsPaused)
			capture.beforeMicOpen = device.prepareLightForOpen
			var ring atomic.Bool
			detach := device.attach(func(on bool) error { ring.Store(on); return nil })
			t.Cleanup(detach)
			controls := newPersistentControls(&pauseStoreFake{}, capture.Pause, capture, func(bool) { time.Sleep(statusWrite) })
			button := newJabraMuteButton(capture, device, &server.ControlHooks{Pause: controls.Pause, Close: controls.Close, Resume: controls.Resume}, newWarningsRegistry())
			mic := newScriptedMic()
			recording := &ringCheckingRecording{ring: &ring}
			runMicLoopOn(t, capture, orderedMic{micStreamer: mic, device: device}, mic, recording, (&recordedWaits{}).wait)
			mic.expectEvents(t, "close")
			time.Sleep(2 * jabraOwnMuteWindow) // the tap is a person's, outside every operation's cover

			button.press(jabra.MonotonicNow())
			waitForHardware(t, "the tap to resume the unit", func() bool { return !capture.IsPaused() })
			mic.expectEvents(t, "open", "stream")
			if ring.Load() {
				t.Fatal("control: the microphone opened with the ring showing Muted")
			}
			mic.capture(t, make([]byte, 320))
			var calls []string
			deadline := time.After(100 * time.Millisecond)
		watch:
			for {
				select {
				case call := <-mic.events:
					calls = append(calls, call)
				case <-deadline:
					break watch
				}
			}
			if len(calls) > 0 || recording.bytes.Load() != 320 {
				t.Fatalf("a resume tap cost a microphone reopen with the ring dark: device calls %v, the recording got %d of the first 320 bytes", calls, recording.bytes.Load())
			}
		})
	}
}
