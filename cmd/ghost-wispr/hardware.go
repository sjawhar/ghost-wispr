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
	// recordingLightBusyWarning names a light another program had
	// (indicator.Contended), and recordingLightUncheckedWarning a unit whose
	// stored pattern is not checked yet (errLightUnchecked); neither light
	// failed. The light guard clears them at the next check that passes.
	recordingLightBusyWarning      = "Recording light busy: another program is using it; the unit does not record until it shows red again"
	recordingLightUncheckedWarning = "Recording light not checked yet: its stored pattern is being checked; the unit does not record until it shows red"
)

func recordingLightWarning(err error) string {
	switch {
	case errors.Is(err, indicator.ErrNoBlink1):
		return recordingLightMissingWarning
	case errors.Is(err, indicator.ErrNotRed):
		return recordingLightNotRedWarning
	case indicator.Contended(err):
		return recordingLightBusyWarning
	case errors.Is(err, errLightUnchecked):
		return recordingLightUncheckedWarning
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
// blink(1) (indicator.Contended), it prints unknown and fails. Its open does
// not fail for another command: the version waits (OpenBlink1ForCommand).
func runLightState(stdout io.Writer) error {
	light, err := openBlink1Command(blinkSysfsRoot, blinkDevRoot)
	if err != nil {
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

// buttonToggle is a hardware button's presses: the power button's taps and
// the Jabra's mute presses. A press reads the gate's switch, except while this
// button's latest resume still runs and no pause has come since: the switch
// moves only once that resume's light check has passed, so the press then
// counts the unit as resumed and pauses. A press that pauses pauses at once;
// a press that resumes runs in the background, so the button takes the next
// press while the resume's light check runs (review 5963682292 of #79). It
// remembers nothing else: a resume that was refused, an own Jabra report, or a
// pause or resume from anywhere else leaves the next press reading the switch
// (review 5967147417 of #79).
type buttonToggle struct {
	capture  *captureGate
	controls *server.ControlHooks
	name     string

	mu sync.Mutex
	// resuming is set while the resume this button last started runs and no
	// pause has come since. pausesAt is the gate's pause count when it
	// started: a pause from anywhere moves it. generation tells that resume
	// from a later one.
	resuming   bool
	pausesAt   int
	generation int
	running    sync.WaitGroup
}

func newButtonToggle(capture *captureGate, controls *server.ControlHooks, name string) *buttonToggle {
	return &buttonToggle{capture: capture, controls: controls, name: name}
}

// press toggles capture for one press. keepPaused, when set and true, keeps a
// paused unit paused: the Jabra's guard for its own mute report. after runs
// once the press has paused or has started its resume, before the resume
// ends. The returned channel closes once the press, its resume included, is
// over.
func (t *buttonToggle) press(keepPaused func() bool, after func()) <-chan struct{} {
	paused := t.counted()
	done := make(chan struct{})
	switch {
	case paused && keepPaused != nil && keepPaused():
		log.Printf("%s report came with Ghost Wispr's own microphone operation; capture stays paused", t.name)
		close(done)
	case paused:
		// A resume can take seconds: its light check reads the blink(1)'s
		// stored pattern. The button must take the next press meanwhile.
		generation := t.startResume()
		t.running.Add(1)
		go func() {
			defer t.running.Done()
			defer close(done)
			defer t.endResume(generation)
			if err := t.controls.Resume(); err != nil {
				log.Printf("warning: %s toggle: %v", t.name, err)
			} else if t.controls.OnStatusChanged != nil {
				t.controls.OnStatusChanged(false)
			}
		}()
	default:
		t.stopResume()
		err := t.controls.Pause()
		// A Pause error still leaves capture paused, so clients must hear of it.
		if t.controls.OnStatusChanged != nil {
			t.controls.OnStatusChanged(true)
		}
		if err != nil {
			log.Printf("warning: %s toggle: %v", t.name, err)
		}
		close(done)
	}
	if after != nil {
		after()
	}
	return done
}

// counted reports the unit as paused or not for a press: resumed while this
// button's own resume runs with no pause since, else the gate's switch. The
// Jabra's ring shows the Jabra button's count (captureDevice.showPaused), so
// a press does what the ring it is pressed on shows.
func (t *buttonToggle) counted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resuming && t.capture.pauseCount() == t.pausesAt {
		return false
	}
	return t.capture.IsPaused()
}

// startResume records that this button's resume runs from now, and returns
// its generation.
func (t *buttonToggle) startResume() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.resuming, t.pausesAt = true, t.capture.pauseCount()
	return t.generation
}

// endResume records that the resume of generation has ended, unless a later
// one runs.
func (t *buttonToggle) endResume(generation int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.generation == generation {
		t.resuming = false
	}
}

// stopResume records a pause by this button: no resume of it counts any more.
func (t *buttonToggle) stopResume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resuming = false
}

// wait returns once every resume this button started is over.
func (t *buttonToggle) wait() { t.running.Wait() }

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

// CheckLight checks the stored pattern of a light whose unit is not yet known
// black (reconnectingLight.CheckPattern), one command at a time. It can take
// seconds, so it runs before the capture gate's control is taken
// (captureGate.checkLightPattern), never under it: a pause meanwhile is not
// held up, and the resume it overtakes does nothing.
func (h *hardwareControl) CheckLight() error {
	if h.stopped.Load() {
		return fmt.Errorf("recording light stopped")
	}
	if h.light == nil {
		return nil
	}
	if err := h.light.CheckPattern(); err != nil {
		h.Wake()
		return err
	}
	return nil
}

// EnsureRed confirms the configured indicator is red before a microphone
// starts or capture opens. Each successful check owns a temporary red hold
// until that start attempt ends. A missing light fails the check like any
// other: the unit records only while its light is on. It checks no stored
// pattern: CheckLight comes first, and a unit still unchecked is refused
// (errLightUnchecked).
func (h *hardwareControl) EnsureRed() error {
	return h.EnsureRedWhile(nil)
}

// EnsureRedWhile is EnsureRed for a microphone open: it calls allowed, if not
// nil, under the light's lock just before the red write, and if allowed
// returns an error it writes nothing, takes no hold and returns that error.
// A pause after allowed returned does not stop the write (captureGate's
// confirmMicOpenRed says why, and how soon the light then goes off).
func (h *hardwareControl) EnsureRedWhile(allowed func() error) error {
	if h.stopped.Load() {
		return fmt.Errorf("recording light stopped")
	}
	if h.light == nil {
		return nil
	}
	held := false
	err := h.light.ConfirmRedWhile(func() error {
		if allowed != nil {
			if err := allowed(); err != nil {
				return err
			}
		}
		// Hold red before the write, under the light's lock:
		// reconnectingLight.SetRGB then refuses an off write the controller
		// decided before this confirmation. A refused confirmation takes no
		// hold, so the controller, which keeps the light red while one is
		// held, does not light it for that confirmation either.
		h.redHolds.Add(1)
		held = true
		return nil
	})
	if err != nil {
		if held {
			h.ReleaseRedHold()
		}
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
	capture.checkLightPattern = func() error {
		if err := hardware.CheckLight(); err != nil {
			warnings.Set("recording-light", recordingLightWarning(err))
			return err
		}
		return nil
	}
	capture.beforeMicStart = hardware.EnsureRed
	capture.beforeMicOpenRed = hardware.EnsureRedWhile
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
			checked:    hardware.Wake,
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
	// patternUnchecked marks a light whose stored pattern the recorder has not
	// found black: a unit it does not know (indicator.KnownBlack). It is kept,
	// so off writes, which go without the lock, still reach it; it gets no
	// watchdog tickle and no red until CheckPattern finds its pattern black,
	// since its watchdog would play that pattern (review 5954543056 of #79).
	// Under deviceMu.
	patternUnchecked bool
	// checkMu lets one pattern check run at a time.
	checkMu sync.Mutex
	// checking is set while a check that a red write started runs
	// (checkInBackground); checked, when set, is called once such a check
	// passes, so the controller looks at the light again (hardwareControl.Wake).
	checking    atomic.Bool
	checked     func()
	nextAttempt time.Time
	lastErr     error
	warnings    *warningsRegistry
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
	// A unit this process found black before, as the same blink(1) after a
	// device error, needs no check. Another is kept unchecked: the red path
	// checks it (CheckPattern), one command at a time. Checked here, under
	// deviceMu, it kept a pause's off write from the light for the whole
	// check (review 5956273705 of #79).
	unchecked := !indicator.KnownBlack(light)
	l.patternUnchecked = unchecked
	l.light = light
	l.offs.Add(1)
	l.lastErr = nil
	if !unchecked {
		l.warnings.Clear("recording-light")
	}
	return light, nil
}

// errLightUnchecked refuses a watchdog tickle, a red write or a red
// confirmation for a light whose stored pattern is not found black
// (patternUnchecked).
var errLightUnchecked = errors.New("the recording light's stored pattern is not checked black yet")

// CheckPattern checks the stored pattern of a light kept unchecked
// (patternUnchecked), opening the light first if there is none. It holds
// deviceMu for one command of the check at a time (indicator.NewPatternCheck):
// a pause's off write waits for at most one of them, the recorder's other
// commands never meet the check on the handle, and a Close or a failure
// closes the handle only between two of them, after which the check stops
// and fails (review 5956273705 of #79). It runs only on the red path:
// EnsureRed runs it before it holds the light red, a red write that found the
// light unchecked starts it (checkInBackground), and the controller runs it
// before its first update (BlackPattern). It fails while another command
// keeps the check from the light.
func (l *reconnectingLight) CheckPattern() error {
	l.deviceMu.Lock()
	light, err := l.currentForConfirmation()
	l.deviceMu.Unlock()
	if err != nil {
		return err
	}
	return l.checkPattern(light)
}

// checkPattern runs light's stepwise check to the end, one at a time.
func (l *reconnectingLight) checkPattern(light indicator.Light) error {
	l.checkMu.Lock()
	defer l.checkMu.Unlock()
	check := indicator.NewPatternCheck(light)
	for {
		done, err := l.checkStep(light, check)
		if err != nil || done {
			return err
		}
	}
}

// checkStep sends check's next command to light under deviceMu, unless light
// was closed or opened again since the check began, or is no longer
// unchecked.
func (l *reconnectingLight) checkStep(light indicator.Light, check indicator.PatternCheck) (bool, error) {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	if l.light != light {
		return false, errLightUnchecked
	}
	if !l.patternUnchecked {
		return true, nil
	}
	done, err := check.Step()
	if err != nil {
		if indicator.Contended(err) {
			return false, err
		}
		return false, l.failed(err)
	}
	if done {
		l.patternUnchecked = false
		l.warnings.Clear("recording-light")
	}
	return done, nil
}

// checkInBackground starts the check of light for a tickle or a red write that
// found it unchecked, unless one such check runs. The command is refused at
// once, so the controller never waits for the check and a pause meanwhile
// still reaches the light. The tickle comes with every controller update, so
// a unit that needs no red write (it still reads red) is checked too. Once
// the check passes, checked wakes the controller, whose next update tickles
// the light and lights it. It checks only light: once light is closed it
// stops, and it opens no other.
func (l *reconnectingLight) checkInBackground(light indicator.Light) {
	if !l.checking.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer l.checking.Store(false)
		if err := l.checkPattern(light); err == nil && l.checked != nil {
			l.checked()
		}
	}()
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
	} else if l.patternUnchecked {
		l.checkInBackground(light)
		return errLightUnchecked
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

// ServerDown tickles the light's watchdog, or with timeout 0 turns it off. A
// light whose stored pattern is not found black (patternUnchecked) gets no
// tickle: its watchdog would play that pattern if the recorder stopped. The
// refused tickle starts the light's check instead (checkInBackground). Its
// watchdog may still be turned off.
func (l *reconnectingLight) ServerDown(timeout time.Duration) error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	light, err := l.current()
	if err != nil {
		return err
	}
	if timeout > 0 && l.patternUnchecked {
		l.checkInBackground(light)
		return errLightUnchecked
	}
	if err := light.ServerDown(timeout); err != nil {
		return l.failed(err)
	}
	return nil
}

// BlackPattern is the controller's check before its first update:
// CheckPattern.
func (l *reconnectingLight) BlackPattern() error {
	return l.CheckPattern()
}

func (l *reconnectingLight) ConfirmRed() error {
	return l.ConfirmRedWhile(nil)
}

// ConfirmRedWhile is ConfirmRed, but first calls before, if not nil, under the
// light's lock, and returns before's error, having written nothing, unless it
// is nil.
func (l *reconnectingLight) ConfirmRedWhile(before func() error) error {
	l.deviceMu.Lock()
	defer l.deviceMu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	light, err := l.currentForConfirmation()
	if err != nil {
		return err
	}
	if l.patternUnchecked {
		return errLightUnchecked // CheckPattern comes first (EnsureRed)
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
	toggle := newButtonToggle(capture, controls, "power-button")
	defer toggle.wait()
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
				toggle.press(nil, nil)
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
