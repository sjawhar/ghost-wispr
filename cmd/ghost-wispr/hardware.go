package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/powerbutton"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

const (
	blinkSysfsRoot    = "/sys"
	blinkDevRoot      = "/dev"
	inputDevices      = "/proc/bus/input/devices"
	inputDevRoot      = "/dev/input"
	hardwareRetry     = 10 * time.Second
	lightShutdownWait = 2 * time.Second
)

// The recording light's warnings. A unit with a light records only while it
// is on, so each says that the unit does not record.
const (
	recordingLightMissingWarning = "Recording light not found; the unit does not record until it is back"
	recordingLightNotRedWarning  = "Recording light does not show red; the unit does not record until it does"
	recordingLightFailedWarning  = "Recording light failed; the unit does not record until it shows red again"
)

func recordingLightWarning(err error) string {
	switch {
	case errors.Is(err, indicator.ErrNoBlink1):
		return recordingLightMissingWarning
	case errors.Is(err, indicator.ErrNotRed):
		return recordingLightNotRedWarning
	default:
		return recordingLightFailedWarning
	}
}

func listenAddress(address string) string {
	if address == "" {
		return defaultListenAddress
	}
	return address
}

var (
	openBlink1 = indicator.OpenBlink1
	// openBlink1Command opens the light for `light-off` and `light-state`:
	// their commands wait for the light longer than the recorder's, and let a
	// waiting recorder go first.
	openBlink1Command = indicator.OpenBlink1ForCommand
	errLightOther     = errors.New("recording light is neither red nor off")
	// lightCheckEvery is how often the controller checks the recording light;
	// lightRetryEvery is how long it waits before it looks again for a light
	// it lost. Tests shorten both.
	lightCheckEvery = time.Second
	lightRetryEvery = hardwareRetry
)

func runLightOff(stderr io.Writer) error {
	light, err := openBlink1Command(blinkSysfsRoot, blinkDevRoot)
	if err != nil {
		if errors.Is(err, indicator.ErrNoBlink1) {
			_, _ = fmt.Fprintf(stderr, "light-off: %v\n", err)
			return nil
		}
		return fmt.Errorf("open indicator: %w", err)
	}
	defer func() { _ = light.Close() }()
	if err := light.SetRGB(0, 0, 0); err != nil {
		return fmt.Errorf("turn indicator off: %w", err)
	}
	return nil
}

// runLightState prints the blink(1)'s colour: red, off, or other with its
// values. When it cannot read the light, as when another command had the
// blink(1) (indicator.Contended), it prints unknown and fails.
func runLightState(stdout io.Writer) error {
	light, err := openBlink1Command(blinkSysfsRoot, blinkDevRoot)
	if err != nil {
		if indicator.Contended(err) {
			_, _ = fmt.Fprintln(stdout, "unknown")
		}
		return fmt.Errorf("open indicator: %w", err)
	}
	defer func() { _ = light.Close() }()
	r, g, b, err := light.ReadRGB()
	if err != nil {
		_, _ = fmt.Fprintln(stdout, "unknown")
		return fmt.Errorf("read indicator: %w", err)
	}
	if r == 255 && g == 0 && b == 0 {
		_, err = fmt.Fprintln(stdout, "red")
		return err
	}
	if r == 0 && g == 0 && b == 0 {
		_, err = fmt.Fprintln(stdout, "off")
		return err
	}
	if _, err = fmt.Fprintf(stdout, "other %d,%d,%d\n", r, g, b); err != nil {
		return err
	}
	return errLightOther
}

func runLightCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: ghost-wispr <light-off|light-state>")
		return 2
	}

	var err error
	switch args[0] {
	case "light-off":
		err = runLightOff(stderr)
	case "light-state":
		err = runLightState(stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		_, _ = fmt.Fprintln(stderr, "usage: ghost-wispr <light-off|light-state>")
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func recordingSignal(capture *captureGate, mic interface{ IsOpen() bool }) bool {
	return capture.capturing(mic)
}

func toggleCapture(isPaused func() bool, pause, resume func() error, onStatusChanged func(bool)) error {
	if isPaused() {
		if err := resume(); err != nil {
			return err
		}
		if onStatusChanged != nil {
			onStatusChanged(false)
		}
		return nil
	}
	err := pause()
	// A Pause error still leaves capture paused, so clients must hear of it.
	if onStatusChanged != nil {
		onStatusChanged(true)
	}
	return err
}

// hardwareControl coordinates the indicator with the capture transitions.
type hardwareControl struct {
	light      *reconnectingLight
	controller *indicator.Controller
	done       <-chan struct{}
	redHolds   atomic.Int32
	stopped    atomic.Bool
	// reportLight hands a failed light check to the light guard; nil without
	// a light.
	reportLight func(error)
}

// LightUnconfirmed reports a red confirmation that failed outside the
// controller's own checks, so the light guard holds capture for the light.
// It never blocks.
func (h *hardwareControl) LightUnconfirmed(err error) {
	if h.reportLight != nil {
		h.reportLight(err)
	}
}

func (h *hardwareControl) Done() <-chan struct{} {
	return h.done
}

func (h *hardwareControl) Wake() {
	if h.controller != nil {
		h.controller.Wake()
	}
}

func (h *hardwareControl) holdsRed() bool {
	return h.redHolds.Load() > 0
}

func (h *hardwareControl) CaptureStarted() {
	h.Wake()
}

// lightOffs counts the times the recording light was written, or read back,
// as anything but red, or failed; 0 without a light.
func (h *hardwareControl) lightOffs() uint64 {
	if h.light == nil {
		return 0
	}
	return h.light.offs.Load()
}

// lightRedMark is lightOffs as the light's last red confirmation left it.
func (h *hardwareControl) lightRedMark() uint64 {
	if h.light == nil {
		return 0
	}
	return h.light.redMark.Load()
}

// checkLight reads the light back, which counts it in lightOffs unless it is
// red, so the gate's writer stops audio as soon as the light is off, whoever
// turned it off.
func (h *hardwareControl) checkLight() {
	if h.light != nil {
		_, _, _, _ = h.light.ReadRGB()
	}
}

func (h *hardwareControl) ReleaseRedHold() {
	for {
		holds := h.redHolds.Load()
		if holds == 0 || h.redHolds.CompareAndSwap(holds, holds-1) {
			h.Wake()
			return
		}
	}
}

func (h *hardwareControl) Stop() {
	h.stopped.Store(true)
}

// EnsureRed confirms the configured indicator is red before a microphone
// starts or capture opens. Each successful check owns a temporary red hold
// until that start attempt ends. A missing light fails the check like any
// other: the unit records only while its light is on.
func (h *hardwareControl) EnsureRed() error {
	if h.stopped.Load() {
		return fmt.Errorf("recording light stopped")
	}
	if h.light == nil {
		return nil
	}
	// Hold red before the write: reconnectingLight.SetRGB then refuses an off
	// write the controller decided before this confirmation.
	h.redHolds.Add(1)
	if err := h.light.ConfirmRed(); err != nil {
		h.ReleaseRedHold()
		return err
	}
	h.light.warnings.Clear("recording-light")
	return nil
}

// guardResumeWithLight checks red both when a resume is requested, so callers
// can receive a synchronous refusal, and again immediately before the gate
// opens. The first hold is released while the paused session settles; the
// second is released after the gate opens.
func guardResumeWithLight(capture *captureGate, warnings *warningsRegistry, onResumeRefused func(error)) {
	capture.beforeResumeRequest = func() error {
		if err := capture.confirmMicStart(); err != nil {
			warnings.Set("recording-light", recordingLightWarning(err))
			return err
		}
		capture.finishMicStart()
		return nil
	}
	capture.beforeGateOpen = func() error {
		if err := capture.confirmMicStart(); err != nil {
			warnings.Set("recording-light", recordingLightWarning(err))
			return err
		}
		return nil
	}
	capture.onResumeRefused = onResumeRefused
}

// wireLightConsent makes every microphone open, start and gate opening wait
// for the light to be confirmed red, and hold it red until it is done; and
// lets audio pass only while the light has stayed red since the microphone
// opened, which the gate's writer reads back for each buffer. The light stays
// red while a microphone is open (startHardware), so it goes off only once the
// mic loop has closed it, which wakes it.
func wireLightConsent(capture *captureGate, hardware *hardwareControl, warnings *warningsRegistry, onResumeRefused func(error)) {
	capture.beforeMicStart = hardware.EnsureRed
	capture.afterMicStart = hardware.ReleaseRedHold
	capture.afterGateOpen = hardware.ReleaseRedHold
	capture.onLightUnconfirmed = hardware.LightUnconfirmed
	capture.lightOffs = hardware.lightOffs
	capture.lightRedMark = hardware.lightRedMark
	capture.checkLight = hardware.checkLight
	capture.afterMicClose = hardware.Wake
	guardResumeWithLight(capture, warnings, onResumeRefused)
}

// pauseCapture closes the capture gate and wakes the light controller. It
// takes no red hold away: a microphone open under way holds the light red
// until it ends, and the light stays red while a microphone is open, so it
// goes off once the mic loop has closed it, never before.
func pauseCapture(capture *captureGate, hardware *hardwareControl) {
	capture.Pause()
	hardware.Wake()
}

// startHardware starts the configured recording light, and the guard that
// keeps capture closed while the light cannot be confirmed red. The light is
// red while a red confirmation holds it, and while a microphone is open, so
// it is never off while the microphone is open: a pause, no audio or a stream
// error turns it off only once the mic loop has closed the microphone. The
// button starts only after its durable controls are completely wired.
func startHardware(ctx context.Context, cfg *config.Config, capture *captureGate, mic interface{ IsOpen() bool }, warnings *warningsRegistry, hub *server.Hub) *hardwareControl {
	done := make(chan struct{})
	hardware := &hardwareControl{done: done}
	if cfg.IndicatorLight == config.IndicatorLightBlink1 {
		wantRed := func() bool {
			return hardware.holdsRed() || mic.IsOpen()
		}
		hardware.light = &reconnectingLight{
			warnings:   warnings,
			holdRed:    func() bool { return !hardware.stopped.Load() && wantRed() },
			retryEvery: lightRetryEvery,
		}
		guard := newLightGuard(capture, warnings, lightRetryEvery)
		hardware.reportLight = guard.report
		hardware.controller = indicator.NewController(hardware.light, wantRed, lightCheckEvery, guard.report)
		go guard.run(ctx)
		go func() {
			defer close(done)
			hardware.controller.Run(ctx)
		}()
		go wakeLightOnCaptureEvents(ctx, hub, hardware)
	} else {
		close(done)
	}
	return hardware
}

func startPowerButton(ctx context.Context, cfg *config.Config, capture *captureGate, controls *server.ControlHooks, warnings *warningsRegistry) {
	if cfg.PowerButton {
		go runPowerButton(ctx, capture, controls, warnings)
	}
}

func wakeLightOnCaptureEvents(ctx context.Context, hub *server.Hub, hardware *hardwareControl) {
	events := hub.Subscribe()
	defer hub.Unsubscribe(events)
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-events:
			if !ok {
				return
			}
			wakeLightOnCaptureEvent(raw, hardware)
		}
	}
}

func wakeLightOnCaptureEvent(raw []byte, hardware *hardwareControl) {
	var event struct {
		Type      string `json:"type"`
		Component string `json:"component"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	if event.Type == "status_changed" || (event.Type == "component_status" && event.Component == "mic") {
		hardware.Wake()
	}
}

type reconnectingLight struct {
	mu       sync.Mutex
	deviceMu sync.Mutex
	light    indicator.Light
	// patternUnchecked marks a light opened while its black pattern could not
	// be checked, because another command had the light (indicator.Contended).
	// It is kept, so tickles and off writes, which go without the lock, still
	// reach it; it lights no red until checkPattern has checked the pattern.
	// Under deviceMu.
	patternUnchecked bool
	nextAttempt      time.Time
	lastErr          error
	warnings         *warningsRegistry
	// holdRed reports that the light must stay red. SetRGB checks it under
	// deviceMu, so an off write decided before a red confirmation cannot land
	// after it.
	holdRed func() bool
	// retryEvery is how long a failed discovery is remembered before the next
	// one; zero means hardwareRetry.
	retryEvery time.Duration
	// offs counts, under deviceMu, each write of a colour other than red, each
	// read-back of one, each light just found (it shows what it showed before)
	// and each failure: each time the light may have stopped showing red.
	offs atomic.Uint64
	// redMark is offs as the last red confirmation left it, set under
	// deviceMu, so no failure counted after the confirmation is in it.
	redMark atomic.Uint64
}

func (l *reconnectingLight) retryDelay() time.Duration {
	if l.retryEvery > 0 {
		return l.retryEvery
	}
	return hardwareRetry
}

func (l *reconnectingLight) current() (indicator.Light, error) {
	return l.currentWithRetry(false)
}

func (l *reconnectingLight) currentForConfirmation() (indicator.Light, error) {
	return l.currentWithRetry(true)
}

func (l *reconnectingLight) currentWithRetry(force bool) (indicator.Light, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.light != nil {
		return l.light, nil
	}
	if !force && time.Now().Before(l.nextAttempt) {
		return nil, l.lastErr
	}
	light, err := openBlink1(blinkSysfsRoot, blinkDevRoot)
	if err != nil {
		l.lastErr = err
		l.nextAttempt = time.Now().Add(l.retryDelay())
		l.warnings.Set("recording-light", recordingLightWarning(err))
		return nil, err
	}
	if err := light.BlackPattern(); err != nil {
		if indicator.Contended(err) {
			// Another command had the light. Closed, the light would wait
			// retryDelay for the next open, with no tickle and no off write
			// meanwhile (review 5953011789 of #79).
			l.light = light
			l.patternUnchecked = true
			l.offs.Add(1)
			l.lastErr = nil
			l.warnings.Set("recording-light", recordingLightWarning(err))
			return light, nil
		}
		_ = light.Close()
		l.lastErr = err
		l.nextAttempt = time.Now().Add(l.retryDelay())
		l.warnings.Set("recording-light", recordingLightWarning(err))
		return nil, err
	}
	l.patternUnchecked = false
	l.light = light
	l.offs.Add(1)
	l.lastErr = nil
	l.warnings.Clear("recording-light")
	return light, nil
}

// checkPattern checks the black pattern of a light kept with it unchecked
// (patternUnchecked), and reports an error while it is still unchecked. Call
// it under deviceMu.
func (l *reconnectingLight) checkPattern(light indicator.Light) error {
	if !l.patternUnchecked {
		return nil
	}
	if err := light.BlackPattern(); err != nil {
		return err
	}
	l.patternUnchecked = false
	l.warnings.Clear("recording-light")
	return nil
}

// failed counts a failed command as a time the light may not be red, and,
// unless another command only got in its way (indicator.Contended), closes
// the handle, so the next command opens the light again. Closed, the light
// would have to be opened again, with a version query and the black pattern's
// reads, each waiting on the same lock, before the next watchdog tickle could
// be sent.
func (l *reconnectingLight) failed(err error) error {
	l.offs.Add(1)
	if indicator.Contended(err) {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.light != nil {
		_ = l.light.Close()
		l.light = nil
		l.patternUnchecked = false
	}
	l.lastErr = err
	l.nextAttempt = time.Time{}
	l.warnings.Set("recording-light", recordingLightWarning(err))
	return err
}

func (l *reconnectingLight) SetRGB(r, g, b uint8) error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	red := r == 255 && g == 0 && b == 0
	if !red && l.holdRed != nil && l.holdRed() {
		return nil
	}
	light, err := l.current()
	if err != nil {
		return err
	}
	if !red {
		l.offs.Add(1)
	} else if err := l.checkPattern(light); err != nil {
		return l.failed(err)
	}
	if err := light.SetRGB(r, g, b); err != nil {
		return l.failed(err)
	}
	return nil
}

func (l *reconnectingLight) ReadRGB() (uint8, uint8, uint8, error) {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	light, err := l.current()
	if err != nil {
		return 0, 0, 0, err
	}
	r, g, b, err := light.ReadRGB()
	if err != nil {
		return 0, 0, 0, l.failed(err)
	}
	if r != 255 || g != 0 || b != 0 {
		l.offs.Add(1)
	}
	return r, g, b, nil
}

func (l *reconnectingLight) ServerDown(timeout time.Duration) error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	light, err := l.current()
	if err != nil {
		return err
	}
	if err := light.ServerDown(timeout); err != nil {
		return l.failed(err)
	}
	// After the tickle, so a pattern check that meets the lock never delays it.
	if err := l.checkPattern(light); err != nil && !indicator.Contended(err) {
		return l.failed(err)
	}
	return nil
}

func (l *reconnectingLight) BlackPattern() error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	light, err := l.current()
	if err != nil {
		return err
	}
	if err := light.BlackPattern(); err != nil {
		return l.failed(err)
	}
	l.patternUnchecked = false
	return nil
}

func (l *reconnectingLight) ConfirmRed() error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	light, err := l.currentForConfirmation()
	if err != nil {
		return err
	}
	if err := l.checkPattern(light); err != nil {
		return l.failed(err)
	}
	if err := light.SetRGB(255, 0, 0); err != nil {
		return l.failed(err)
	}
	r, g, b, err := light.ReadRGB()
	if err != nil {
		return l.failed(err)
	}
	if r != 255 || g != 0 || b != 0 {
		l.offs.Add(1)
		return fmt.Errorf("%w: it reads %d,%d,%d", indicator.ErrNotRed, r, g, b)
	}
	l.redMark.Store(l.offs.Load())
	return nil
}

func (l *reconnectingLight) Close() error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.light == nil {
		return nil
	}
	err := l.light.Close()
	l.light = nil
	l.patternUnchecked = false
	return err
}

func runPowerButton(ctx context.Context, capture *captureGate, controls *server.ControlHooks, warnings *warningsRegistry) {
	for ctx.Err() == nil {
		path, err := powerbutton.FindDevice(inputDevices, inputDevRoot)
		if err != nil {
			log.Printf("warning: find power button: %v", err)
			warnings.Set("power-button", powerButtonWarning(err))
			waitForHardwareRetry(ctx)
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			log.Printf("warning: open power button: %v", err)
			warnings.Set("power-button", powerButtonWarning(err))
			waitForHardwareRetry(ctx)
			continue
		}
		warnings.Clear("power-button")
		// deviceCtx ends this device's reader and tap goroutines when the
		// device fails, so a tap left unsent does not strand them.
		deviceCtx, cancelDevice := context.WithCancel(ctx)
		events := make(chan powerbutton.Event)
		readDone := make(chan error, 1)
		go func() {
			readDone <- powerbutton.ReadEvents(deviceCtx, file, events)
			close(events)
		}()
		taps := make(chan struct{})
		go powerbutton.Taps(deviceCtx, events, 1500*time.Millisecond, taps)

	readLoop:
		for {
			select {
			case <-ctx.Done():
				cancelDevice()
				_ = file.Close()
				return
			case <-taps:
				if err := toggleCapture(capture.IsPaused, controls.Pause, controls.Resume, controls.OnStatusChanged); err != nil {
					log.Printf("warning: power-button toggle: %v", err)
				}
			case err := <-readDone:
				cancelDevice()
				_ = file.Close()
				if err != nil {
					log.Printf("warning: read power button: %v", err)
				}
				warnings.Set("power-button", powerButtonWarning(err))
				waitForHardwareRetry(ctx)
				break readLoop
			}
		}
	}
}

func waitForHardwareRetry(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(hardwareRetry):
	}
}

func powerButtonWarning(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "Power button is not accessible; check the input group and udev rule"
	}
	return "Power button not found; recording can be paused from the web page"
}
