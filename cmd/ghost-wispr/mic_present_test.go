package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheLightStaysOffWhileNoCaptureDeviceIsPresent fails if a microphone
// start, a start-up retry or the mic loop's reconnect turns the light red
// while no capture device is present, or opens anything then. The spec's
// light decision (INFRA-1027, "Light") says the light is off while the
// microphone is missing; every 15 s the start-up retry lit it red for its
// attempt. The control: once a capture device is present, each lights it red
// and opens.
func TestTheLightStaysOffWhileNoCaptureDeviceIsPresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// run makes one attempt and reports whether it reached the device.
		run func(t *testing.T, r *lightRig) (opened bool, err error)
	}{
		{name: "start-up", run: func(t *testing.T, r *lightRig) (bool, error) {
			var loops sync.WaitGroup
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); loops.Wait() }()
			opened := false
			_, err := startMicrophone(ctx, &loops, r.capture, make(chan struct{}), func(bool) error { opened = true; return nil }, func() {})
			return opened, err
		}},
		{name: "start-up retry", run: func(t *testing.T, r *lightRig) (bool, error) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opened := false
			var err error
			retryMicStartup(ctx, time.Second, oneShotAfter(), r.capture, nil,
				func() error { opened = true; return nil },
				func(e error) { err = e; cancel() }, nil)
			return opened, err
		}},
		{name: "mic loop reconnect", run: func(t *testing.T, r *lightRig) (bool, error) {
			mic := newScriptedMic()
			_, err := reopenMic(r.capture, mic, nopLogf, nil)
			opened := false
			for {
				select {
				case event := <-mic.events:
					opened = opened || event == "reopen"
					continue
				default:
				}
				return opened, err
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startLightRig(t, false)
			var present atomic.Bool
			r.capture.capturePresent = present.Load

			opened, err := tc.run(t, r)
			if ops := r.device.snapshot(); slices.ContainsFunc(ops, isRedOp) {
				t.Fatalf("the light was lit red with no capture device present: operations %v", ops)
			}
			if opened {
				t.Fatal("the attempt reached the device although no capture device is present")
			}
			if !errors.Is(err, errNoCaptureDevice) {
				t.Fatalf("attempt with no capture device = %v, want errNoCaptureDevice", err)
			}

			present.Store(true)
			if opened, err := tc.run(t, r); !opened || err != nil {
				t.Fatalf("control: with a capture device present the attempt opened=%t, err=%v; want it opened", opened, err)
			}
			if ops := r.device.snapshot(); !slices.ContainsFunc(ops, isRedReadBack) {
				t.Fatalf("control: the attempt opened without the light read back red: operations %v", ops)
			}
		})
	}
}
