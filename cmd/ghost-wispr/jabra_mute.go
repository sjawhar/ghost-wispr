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
// is paused, whichever control paused it.
type jabraMuteButton struct {
	capture  *captureGate
	mic      interface{ IsOpen() bool }
	controls *server.ControlHooks
	warnings *warningsRegistry
	// connect finds and opens the Jabra's evdev node; tests replace it.
	connect    func() (io.ReadWriteCloser, error)
	retryEvery time.Duration
	checkEvery time.Duration
}

func newJabraMuteButton(capture *captureGate, mic interface{ IsOpen() bool }, controls *server.ControlHooks, warnings *warningsRegistry) *jabraMuteButton {
	return &jabraMuteButton{
		capture:    capture,
		mic:        mic,
		controls:   controls,
		warnings:   warnings,
		connect:    func() (io.ReadWriteCloser, error) { return connectJabra(inputDevices, inputDevRoot) },
		retryEvery: hardwareRetry,
		checkEvery: jabraLightCheckEvery,
	}
}

func startJabraMuteButton(ctx context.Context, cfg *config.Config, capture *captureGate, mic interface{ IsOpen() bool }, controls *server.ControlHooks, warnings *warningsRegistry) {
	if cfg.JabraMuteButton {
		go newJabraMuteButton(capture, mic, controls, warnings).run(ctx)
	}
}

// connectJabra opens the Jabra's evdev node to read and write: the press is
// read and the light written on one node.
func connectJabra(procDevices, devRoot string) (io.ReadWriteCloser, error) {
	path, err := jabra.FindDevice(procDevices, devRoot)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

// run serves the Jabra until ctx ends, and looks for it again each
// retryEvery while it is missing or after it fails, as after an unplug. It
// logs a warning when the warning changes, not at each retry.
func (b *jabraMuteButton) run(ctx context.Context) {
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
// The light is sent again, even when the kernel holds the wanted state, when
// the device connects, after a press, and when the microphone opens or closes:
// a Jabra may change its own light on a press, when its microphone stream
// starts or stops, or while Ghost Wispr was not running, and the kernel passes
// a light to the device only when it differs from the kernel's own copy.
func (b *jabraMuteButton) serve(ctx context.Context, device io.ReadWriteCloser) error {
	// Ending deviceCtx frees a reader that holds a press nobody takes, and
	// closing the device ends a reader blocked in Read. Close ends that Read
	// only while the file stays non-blocking: never call Fd() on it (an ioctl
	// goes through SyscallConn), or an unplug or shutdown waits forever.
	deviceCtx, cancelDevice := context.WithCancel(ctx)
	presses := make(chan struct{})
	readDone := make(chan error, 1)
	var reader sync.WaitGroup
	reader.Go(func() { readDone <- jabra.ReadPresses(deviceCtx, device, presses) })
	defer func() {
		cancelDevice()
		_ = device.Close()
		reader.Wait()
	}()

	shown, micOpen := b.capture.IsPaused(), b.mic.IsOpen()
	if err := jabra.ResendMuteLight(device, shown); err != nil {
		return err
	}
	ticker := time.NewTicker(b.checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-presses:
			if err := toggleCapture(b.capture.IsPaused, b.controls.Pause, b.controls.Resume, b.controls.OnStatusChanged); err != nil {
				log.Printf("warning: Jabra mute toggle: %v", err)
			}
			shown = b.capture.IsPaused()
			if err := jabra.ResendMuteLight(device, shown); err != nil {
				return err
			}
		case <-ticker.C:
			paused, open := b.capture.IsPaused(), b.mic.IsOpen()
			var err error
			switch {
			case open != micOpen:
				err = jabra.ResendMuteLight(device, paused)
			case paused != shown:
				err = jabra.ShowMuteLight(device, paused)
			}
			if err != nil {
				return err
			}
			shown, micOpen = paused, open
		case err := <-readDone:
			if err == nil {
				err = io.EOF
			}
			return err
		}
	}
}

func (b *jabraMuteButton) waitForRetry(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(b.retryEvery):
	}
}

func jabraMuteButtonWarning(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "Jabra mute button is not accessible; check the input group and udev rule"
	}
	return "Jabra mute button not found; recording can be paused from the web page"
}
