package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// readTimes is a writer behind the capture gate that records when the gate
// marked a buffer read and when it wrote it.
type readTimes struct {
	marked, wrote time.Time
}

func (r *readTimes) markRead(at time.Time) { r.marked = at }

func (r *readTimes) Write(p []byte) (int, error) {
	r.wrote = time.Now()
	return len(p), nil
}

// TestTheMicrophonesReadTimeIsMarkedBeforeTheLightCheck fails if the capture
// gate marks a buffer's read time only after it read the light back: a slow
// read of the light then made every word in the buffer look said later, and
// the voice took the end of the question it answers for talk-over.
func TestTheMicrophonesReadTimeIsMarkedBeforeTheLightCheck(t *testing.T) {
	runParallel(t)
	capture := newCaptureGate(nil, nil, nil)
	var lightAt time.Time
	capture.checkLight = func() { lightAt = time.Now(); time.Sleep(300 * time.Millisecond) } // a slow read of the light
	dst := &readTimes{}
	if _, err := capture.Writer(dst).Write(make([]byte, 320)); err != nil {
		t.Fatal(err)
	}
	if lightAt.IsZero() {
		t.Fatal("control: the gate did not read the light")
	}
	if dst.marked.IsZero() || dst.marked.After(lightAt) {
		t.Fatalf("the buffer was marked read at %v, after the light's read began at %v", dst.marked.Format(time.StampMicro), lightAt.Format(time.StampMicro))
	}
}

// TestTheMicLoopsWriterGivesTheLiveClientTheReadTime fails if the writer the
// mic loop gets does not take the microphone's read time to the live client,
// which times each word by it (transcribe.ResilientClient.CapturedAt).
func TestTheMicLoopsWriterGivesTheLiveClientTheReadTime(t *testing.T) {
	original := streamMicLoopStarter
	t.Cleanup(func() { streamMicLoopStarter = original })
	var writer io.Writer
	streamMicLoopStarter = func(_ context.Context, _ *sync.WaitGroup, _ *captureGate, _ micStreamer, w io.Writer, _ func(time.Duration), _ func(string, ...any), _ *server.Hub) {
		writer = w
	}
	rc := transcribe.NewResilientClient(t.Context(), nil, transcribe.ResilientConfig{SampleRate: 16000}, nil, nil)
	var loops sync.WaitGroup
	startMicMonitoring(t.Context(), &loops, newCaptureGate(nil, nil, nil), newScriptedMic(), audio.NewRecorder(t.TempDir()), rc, nil, func(time.Duration) {}, nopLogf, nil)
	marker, ok := writer.(readMarker)
	if !ok {
		t.Fatalf("the mic loop's writer %T takes no read time", writer)
	}
	read := time.Now()
	marker.markRead(read)
	time.Sleep(200 * time.Millisecond) // the gate's checks
	if _, err := writer.Write(make([]byte, 16000/4*2)); err != nil {
		t.Fatal(err)
	}
	at, _, ok := rc.CapturedAt(125 * time.Millisecond) // halfway through the buffer
	if want := read.Add(-125 * time.Millisecond); !ok || !at.Equal(want) {
		t.Fatalf("a word halfway through the buffer captured at %v (%v), want %v", at.Format(time.StampMicro), ok, want.Format(time.StampMicro))
	}
}

// countingTap counts the bytes written to the voice's tap.
type countingTap struct {
	mu sync.Mutex
	n  int
}

func (c *countingTap) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += len(p)
	return len(p), nil
}

// TestTheMicLoopsWriterDropsTheTapCopyOfAWriteAPauseMadeOld fails if the
// writer the mic loop gets does not pass the capture gate's closing count of
// each write on to the voice's tap: a write the gate let in before a pause
// and a resume, still under way when they came, reached the tap after its
// reset (Sol's review of #133 at 0dfeaad found that the read-time wrapper
// could drop the count and still pass the other tests). The control's write
// sees no closing, and reaches the tap.
func TestTheMicLoopsWriterDropsTheTapCopyOfAWriteAPauseMadeOld(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("a pause and resume during the write %v", paused), func(t *testing.T) {
			original := streamMicLoopStarter
			t.Cleanup(func() { streamMicLoopStarter = original })
			var writer io.Writer
			streamMicLoopStarter = func(_ context.Context, _ *sync.WaitGroup, _ *captureGate, _ micStreamer, w io.Writer, _ func(time.Duration), _ func(string, ...any), _ *server.Hub) {
				writer = w
			}
			rc := transcribe.NewResilientClient(t.Context(), nil, transcribe.ResilientConfig{SampleRate: 16000}, nil, nil)
			capture := newCaptureGate(nil, nil, nil)
			tap := &countingTap{}
			var loops sync.WaitGroup
			startMicMonitoring(t.Context(), &loops, capture, newScriptedMic(), audio.NewRecorder(t.TempDir()), rc, tap, func(time.Duration) {}, nopLogf, nil)
			if paused {
				capture.checkLight = func() { // a pause and a resume, while the write is under way
					capture.mu.Lock()
					capture.closings++
					capture.mu.Unlock()
				}
			}
			if _, err := capture.Writer(writer).Write(make([]byte, 320)); err != nil {
				t.Fatal(err)
			}
			tap.mu.Lock()
			got := tap.n
			tap.mu.Unlock()
			if want := map[bool]int{false: 320, true: 0}[paused]; got != want {
				t.Fatalf("the tap took %d bytes of the write, want %d", got, want)
			}
		})
	}
}
