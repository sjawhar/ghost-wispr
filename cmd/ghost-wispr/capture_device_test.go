package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/jabra"
)

// TestOwnOperationCoversTheJabraReportWindow fails if a press the kernel
// stamped during an operation on the capture device, or within
// jabraOwnMuteWindow after it (the Jabra's own report came 12-15 ms after its
// microphone stream closed), is not taken for the Jabra's own report, or if
// one stamped before the operation or after the window is. The close of a TTS
// playback stream counts as an operation: the trace did not test whether a
// playback close also makes the Jabra report Phone Mute.
func TestOwnOperationCoversTheJabraReportWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(d *captureDevice, during func()) error
	}{
		{name: "close", run: func(d *captureDevice, during func()) error {
			return d.closeStream(func() error { during(); return nil })
		}},
		{name: "open", run: func(d *captureDevice, during func()) error {
			return d.openStream(func() (bool, error) { during(); return true, nil })
		}},
		{name: "PortAudio probe", run: func(d *captureDevice, during func()) error {
			return d.probe(func() error { during(); return nil })
		}},
		{name: "TTS playback close", run: func(d *captureDevice, during func()) error {
			d.playbackOpening()
			d.playbackClosing(during)
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newCaptureDevice(func() bool { return true })
			before := jabra.MonotonicNow()
			if d.ownOperationAt(before) {
				t.Fatal("a press before any operation was taken for the Jabra's own report")
			}
			time.Sleep(time.Millisecond)
			inside := false
			if err := tc.run(d, func() { inside = d.ownOperationAt(jabra.MonotonicNow()) }); err != nil {
				t.Fatal(err)
			}
			ended := jabra.MonotonicNow()
			if !inside {
				t.Error("a press during the operation was taken for a person's tap")
			}
			if d.ownOperationAt(before) {
				t.Error("a press from before the operation was taken for the Jabra's own report")
			}
			if !d.ownOperationAt(ended + 15*time.Millisecond) {
				t.Error("a press 15 ms after the operation was taken for a person's tap")
			}
			if d.ownOperationAt(ended + jabraOwnMuteWindow + 5*time.Millisecond) {
				t.Errorf("a press past the %v window was taken for the Jabra's own report", jabraOwnMuteWindow)
			}
		})
	}
}

// TestMicLoopOrdersTheJabraLightAroundTheCaptureStream fails if the mic loop,
// through the capture device, changes the Jabra's light while the capture
// stream is open: on a pause it must close the stream, then light the Jabra;
// on a resume it must darken the Jabra, then open the stream.
func TestMicLoopOrdersTheJabraLightAroundTheCaptureStream(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	mic := newScriptedMic()
	device := newCaptureDevice(capture.IsPaused)
	detach := device.attach(func(on bool) error {
		if on {
			mic.events <- "light on"
		} else {
			mic.events <- "light off"
		}
		return nil
	})
	t.Cleanup(detach)
	mic.expectEvents(t, "light off")
	// The microphone opened at start-up, through the capture device.
	if err := device.openStream(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	mic.expectEvents(t, "light off")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamMicWithRetry(ctx, capture, orderedMic{micStreamer: mic, device: device}, io.Discard, func(time.Duration) {}, nopLogf, nil)
	}()
	t.Cleanup(func() {
		cancel()
		close(mic.chunks)
		<-done
	})
	mic.expectEvents(t, "stream")

	capture.Pause()
	mic.capture(t, []byte{1, 2})
	mic.expectEvents(t, "close", "light on")
	mic.expectNoEvents(t)

	capture.Resume()
	mic.expectEvents(t, "light off", "open", "stream")
}
