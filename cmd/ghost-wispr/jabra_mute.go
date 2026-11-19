package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/jabra"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

const (
	jabraWarningKey = "jabra-mute-button"
	// jabraLightCheckEvery is how often the Jabra's mute light is compared
	// with the pause, so a pause from the web page, the API or the power
	// button reaches it.
	jabraLightCheckEvery = 250 * time.Millisecond
)

// jabraMuteButton toggles capture on each press of the Jabra's mute button,
// as the power button does, and keeps the Jabra's mute light on while capture
// is paused, whichever control paused it. The light is written through device,
// so it never changes while a capture stream is open.
type jabraMuteButton struct {
	capture  *captureGate
	device   *captureDevice
	controls *server.ControlHooks
	warnings *warningsRegistry
	toggle   *buttonToggle
	// presses counts the presses whose resume or last light write still
	// runs; run waits for them.
	presses sync.WaitGroup
	// connect finds and opens the Jabra's evdev node; tests replace it.
	connect    func() (io.ReadWriteCloser, error)
	retryEvery time.Duration
	checkEvery time.Duration
}

func newJabraMuteButton(capture *captureGate, device *captureDevice, controls *server.ControlHooks, warnings *warningsRegistry) *jabraMuteButton {
	return &jabraMuteButton{
		capture:    capture,
		device:     device,
		controls:   controls,
		warnings:   warnings,
		toggle:     newButtonToggle(capture, controls, "Jabra mute"),
		connect:    func() (io.ReadWriteCloser, error) { return connectJabra(inputDevices, inputDevRoot) },
		retryEvery: hardwareRetry,
		checkEvery: jabraLightCheckEvery,
	}
}

func startJabraMuteButton(ctx context.Context, cfg *config.Config, capture *captureGate, device *captureDevice, controls *server.ControlHooks, warnings *warningsRegistry) {
	if cfg.JabraMuteButton {
		go newJabraMuteButton(capture, device, controls, warnings).run(ctx)
	}
}

// connectJabra opens the Jabra's evdev node (openJabra) and has the kernel
// stamp its events with the monotonic clock, which the press guard compares
// with its own operations (captureDevice.ownOperationAt). A node that refuses
// the clock is not used.
func connectJabra(procDevices, devRoot string) (io.ReadWriteCloser, error) {
	device, err := openJabra(procDevices, devRoot)
	if err != nil {
		return nil, err
	}
	if err := jabra.StampWithMonotonicClock(device); err != nil {
		_ = device.Close()
		return nil, err
	}
	return device, nil
}

// openJabra opens the Jabra's evdev node to read and write: the press is read
// and the light written on one node.
func openJabra(procDevices, devRoot string) (*os.File, error) {
	path, err := jabra.FindDevice(procDevices, devRoot)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

// run serves the Jabra until ctx ends, and looks for it again each
// retryEvery while it is missing or after it fails, as after an unplug. It
// logs a warning when the warning changes, not at each retry. It returns once
// every press, its background resume included, is over.
func (b *jabraMuteButton) run(ctx context.Context) {
	defer b.presses.Wait()
	warned := ""
	for ctx.Err() == nil {
		device, err := b.connect()
		if err == nil {
			warned = ""
			b.warnings.Clear(jabraWarningKey)
			err = fmt.Errorf("device lost: %w", b.serve(ctx, device))
		}
		if ctx.Err() != nil {
			return
		}
		if text := jabraMuteButtonWarning(err); text != warned {
			log.Printf("warning: Jabra mute button: %v", err)
			warned = text
		}
		b.warnings.Set(jabraWarningKey, warned)
		b.waitForRetry(ctx)
	}
}

// serve toggles capture on each press and keeps the light on while capture
// is paused, until ctx ends or the device fails. It closes the device, and
// returns only once the device's reader has ended.
//
// The light is written through b.device: at connect, after a press, and
// within checkEvery of any other pause or resume, but only while no capture
// stream is open, and otherwise when the microphone closes or before it
// opens. A Jabra whose light changes while its microphone stream is open
// reports Phone Mute by itself as that stream closes. The light is sent again,
// even when the kernel holds the wanted state, at connect and after a press: a
// Jabra may change its own light on a press, or while Ghost Wispr was not
// running, and the kernel passes a light to the device only when it differs
// from the kernel's own copy.
func (b *jabraMuteButton) serve(ctx context.Context, device io.ReadWriteCloser) error {
	// Ending deviceCtx frees a reader that holds a press nobody takes, and
	// closing the device ends a reader blocked in Read. Close ends that Read
	// only while the file stays non-blocking: never call Fd() on it (an ioctl
	// goes through SyscallConn), or an unplug or shutdown waits forever.
	deviceCtx, cancelDevice := context.WithCancel(ctx)
	presses := make(chan time.Duration)
	readDone := make(chan error, 1)
	lightFailed := make(chan error, 1)
	var reader sync.WaitGroup
	reader.Go(func() { readDone <- jabra.ReadPresses(deviceCtx, device, presses) })
	detach := b.device.attach(func(on bool) error {
		err := jabra.ResendMuteLight(device, on)
		if err != nil {
			select {
			case lightFailed <- err:
			default:
			}
		}
		return err
	})
	defer func() {
		detach()
		cancelDevice()
		_ = device.Close()
		reader.Wait()
	}()

	ticker := time.NewTicker(b.checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case at := <-presses:
			b.press(at)
		case <-ticker.C:
			b.device.syncLight()
		case err := <-lightFailed:
			return err
		case err := <-readDone:
			if err == nil {
				err = io.EOF
			}
			return err
		}
	}
}

// press toggles capture for a press of the mute button the kernel stamped at,
// a CLOCK_MONOTONIC reading, except one that would resume capture and came
// during an operation on the capture device or within jabraOwnMuteWindow after
// one: the Jabra sends that report by itself as its microphone stream closes,
// and capture stays paused. A press that would pause always pauses, and does
// so at once; a press that resumes runs its light check in the background, so
// the next press is taken meanwhile (buttonToggle). The light is written
// again once the press has asked, and again when its resume ends.
func (b *jabraMuteButton) press(at time.Duration) {
	done := b.toggle.press(
		func() bool { return b.device.ownOperationAt(at) },
		func() { b.device.refreshLight(at) },
	)
	b.presses.Add(1)
	go func() {
		defer b.presses.Done()
		<-done
		b.device.refreshLight(at)
	}()
}

func (b *jabraMuteButton) waitForRetry(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(b.retryEvery):
	}
}

func jabraMuteButtonWarning(err error) string {
	if errors.Is(err, jabra.ErrNoMonotonicClock) {
		return "Jabra mute button is off: its input node refused the monotonic clock that tells its own mute reports from taps; recording can be paused from the web page"
	}
	if errors.Is(err, os.ErrPermission) {
		return "Jabra mute button is not accessible; check the input group and udev rule"
	}
	return "Jabra mute button not found; recording can be paused from the web page"
}
