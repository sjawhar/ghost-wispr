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

func listenAddress(address string) string {
	if address == "" {
		return defaultListenAddress
	}
	return address
}

var (
	openBlink1    = indicator.OpenBlink1
	errLightOther = errors.New("recording light is neither red nor off")
)

func runLightOff(stderr io.Writer) error {
	light, err := openBlink1(blinkSysfsRoot, blinkDevRoot)
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

func runLightState(stdout io.Writer) error {
	light, err := openBlink1(blinkSysfsRoot, blinkDevRoot)
	if err != nil {
		return fmt.Errorf("open indicator: %w", err)
	}
	defer func() { _ = light.Close() }()
	r, g, b, err := light.ReadRGB()
	if err != nil {
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
	forceRed   atomic.Bool
	stopped    atomic.Bool
}

func (h *hardwareControl) Done() <-chan struct{} {
	return h.done
}

func (h *hardwareControl) Wake() {
	if h.controller != nil {
		h.controller.Wake()
	}
}

func (h *hardwareControl) CaptureStarted() {
	h.forceRed.Store(false)
	h.Wake()
}

func (h *hardwareControl) CaptureStopped() {
	h.forceRed.Store(false)
	h.Wake()
}

func (h *hardwareControl) Stop() {
	h.stopped.Store(true)
}

// EnsureRed confirms the configured and connected indicator is red before a
// microphone starts. A missing light preserves the approved warning-and-record
// behavior; an available light must show red.
func (h *hardwareControl) EnsureRed() error {
	if h.stopped.Load() {
		return fmt.Errorf("recording light stopped")
	}
	if h.light == nil {
		return nil
	}
	light, err := h.light.current()
	if err != nil {
		if errors.Is(err, indicator.ErrNoBlink1) {
			return nil
		}
		return err
	}
	if err := light.SetRGB(255, 0, 0); err != nil {
		return h.light.failed(err)
	}
	r, g, b, err := light.ReadRGB()
	if err != nil {
		return h.light.failed(err)
	}
	if r != 255 || g != 0 || b != 0 {
		return fmt.Errorf("recording light readback is %d,%d,%d, want red", r, g, b)
	}
	h.forceRed.Store(true)
	h.light.warnings.Clear("recording-light")
	return nil
}

// startHardware starts the configured recording light. The button starts only
// after its durable controls are completely wired.
func startHardware(ctx context.Context, cfg *config.Config, capture *captureGate, mic interface{ IsOpen() bool }, _ *server.ControlHooks, warnings *warningsRegistry, hub *server.Hub) *hardwareControl {
	done := make(chan struct{})
	hardware := &hardwareControl{done: done}
	if cfg.IndicatorLight == config.IndicatorLightBlink1 {
		hardware.light = &reconnectingLight{warnings: warnings}
		hardware.controller = indicator.NewController(hardware.light, func() bool {
			return hardware.forceRed.Load() || recordingSignal(capture, mic)
		}, time.Second)
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
			var event struct {
				Type      string `json:"type"`
				Component string `json:"component"`
			}
			if json.Unmarshal(raw, &event) != nil {
				continue
			}
			if event.Type == "status_changed" {
				hardware.Wake()
			} else if event.Type == "component_status" && event.Component == "mic" {
				hardware.CaptureStarted()
			}
		}
	}
}

type reconnectingLight struct {
	mu          sync.Mutex
	light       indicator.Light
	nextAttempt time.Time
	lastErr     error
	warnings    *warningsRegistry
}

func (l *reconnectingLight) current() (indicator.Light, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.light != nil {
		return l.light, nil
	}
	if time.Now().Before(l.nextAttempt) {
		return nil, l.lastErr
	}
	light, err := openBlink1(blinkSysfsRoot, blinkDevRoot)
	if err != nil {
		l.lastErr = err
		l.nextAttempt = time.Now().Add(hardwareRetry)
		l.warnings.Set("recording-light", "Recording light not found; the unit records without its light")
		return nil, err
	}
	if err := light.BlackPattern(); err != nil {
		_ = light.Close()
		l.lastErr = err
		l.nextAttempt = time.Now().Add(hardwareRetry)
		l.warnings.Set("recording-light", "Recording light not found; the unit records without its light")
		return nil, err
	}
	l.light = light
	l.lastErr = nil
	l.warnings.Clear("recording-light")
	return light, nil
}

func (l *reconnectingLight) failed(err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.light != nil {
		_ = l.light.Close()
		l.light = nil
	}
	l.lastErr = err
	l.nextAttempt = time.Time{}
	l.warnings.Set("recording-light", "Recording light not found; the unit records without its light")
	return err
}

func (l *reconnectingLight) SetRGB(r, g, b uint8) error {
	light, err := l.current()
	if err != nil {
		return err
	}
	if err := light.SetRGB(r, g, b); err != nil {
		return l.failed(err)
	}
	return nil
}

func (l *reconnectingLight) ReadRGB() (uint8, uint8, uint8, error) {
	light, err := l.current()
	if err != nil {
		return 0, 0, 0, err
	}
	r, g, b, err := light.ReadRGB()
	if err != nil {
		return 0, 0, 0, l.failed(err)
	}
	return r, g, b, nil
}

func (l *reconnectingLight) ServerDown(timeout time.Duration) error {
	light, err := l.current()
	if err != nil {
		return err
	}
	if err := light.ServerDown(timeout); err != nil {
		return l.failed(err)
	}
	return nil
}

func (l *reconnectingLight) BlackPattern() error {
	light, err := l.current()
	if err != nil {
		return err
	}
	if err := light.BlackPattern(); err != nil {
		return l.failed(err)
	}
	return nil
}

func (l *reconnectingLight) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.light == nil {
		return nil
	}
	err := l.light.Close()
	l.light = nil
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
				warnings.Set("power-button", "Power button not found; recording can be paused from the web page")
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
