package main

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
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

// TestJabraStreamsOpenOnlyOnceTheLightHasSettled fails if a microphone or a
// playback stream opens, or the PortAudio scan, which opens streams on the
// Jabra, starts, less than jabraLightSettle after a light write. The kernel
// sends the light to the Jabra after the write returns, from a work queue, so
// a stream opened at once can be open when the light changes, and its next
// close would set off the Jabra's own Phone Mute.
func TestJabraStreamsOpenOnlyOnceTheLightHasSettled(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(d *captureDevice) time.Duration
	}{
		{name: "microphone", open: func(d *captureDevice) (openedAt time.Duration) {
			if err := d.openStream(func() (bool, error) { openedAt = jabra.MonotonicNow(); return true, nil }); err != nil {
				t.Fatal(err)
			}
			return openedAt
		}},
		{name: "TTS playback", open: func(d *captureDevice) time.Duration {
			d.playbackOpening()
			return jabra.MonotonicNow()
		}},
		{name: "PortAudio scan", open: func(d *captureDevice) (scannedAt time.Duration) {
			if err := d.probe(func() error { scannedAt = jabra.MonotonicNow(); return nil }); err != nil {
				t.Fatal(err)
			}
			return scannedAt
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newCaptureDevice(func() bool { return true })
			var mu sync.Mutex
			var writes []time.Duration
			detach := d.attach(func(bool) error {
				mu.Lock()
				defer mu.Unlock()
				writes = append(writes, jabra.MonotonicNow())
				return nil
			})
			t.Cleanup(detach)
			openedAt := tc.open(d)
			mu.Lock()
			defer mu.Unlock()
			var lastWrite time.Duration // the latest write before the open
			for _, w := range writes {
				if w <= openedAt && w > lastWrite {
					lastWrite = w
				}
			}
			if lastWrite == 0 {
				t.Fatal("control: no light was written before the open")
			}
			if gap := openedAt - lastWrite; gap < jabraLightSettle {
				t.Fatalf("the %s stream opened %v after a light write, want at least %v", tc.name, gap, jabraLightSettle)
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

// TestAPlaybackThatPanicsOnCloseHoldsNothingBack fails if a playback stream
// whose close panics (Speaker.Play recovers from it) stays counted open, or
// its close stays an operation under way: every later resume would then wait
// for that playback for good, with readiness reporting resuming, and every
// Jabra tap that would resume would be taken for the Jabra's own report.
func TestAPlaybackThatPanicsOnCloseHoldsNothingBack(t *testing.T) {
	d := newCaptureDevice(func() bool { return false })
	d.playbackOpening()
	var ring atomic.Bool
	ring.Store(true) // the Jabra shows Muted; the light cannot be written during the playback
	detach := d.attach(func(on bool) error { ring.Store(on); return nil })
	t.Cleanup(detach)
	if !ring.Load() {
		t.Fatal("control: the light was written while a playback stream was open")
	}
	func() {
		defer func() { _ = recover() }()
		d.playbackClosing(func() { panic("output stream close failed") })
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if !d.prepareLightForOpen(ctx, nil) {
		t.Fatal("a resume waited for a playback whose close panicked")
	}
	if ring.Load() {
		t.Fatal("the light was not written once the playback was gone")
	}
	if d.ownOperationAt(jabra.MonotonicNow() + time.Second) {
		t.Fatal("a tap a second from now is taken for the Jabra's own report: the panicked close is still under way")
	}
}
