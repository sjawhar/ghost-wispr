package audio

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// TestMicCloseReleasesTheStreamWhileAWriteDoesNotReturn fails if Close, while
// Stream waits on a write of its audio that does not return (a stuck
// recording file), leaves the capture stream open: the input device then
// stays open after a pause, and the light must stay red while it is. Close
// may close the stream at once, since Stream makes no PortAudio call while it
// writes; Stream must then make none after the write returns.
func TestMicCloseReleasesTheStreamWhileAWriteDoesNotReturn(t *testing.T) {
	writer := &blockedWriter{writing: make(chan struct{}), release: make(chan struct{})}
	stream, mic, streamed := openHeldMic(t, 4, writer)
	release := sync.OnceFunc(func() { close(writer.release) })
	t.Cleanup(release) // before openHeldMic's, which waits for Stream
	stream.deliver(1, 2, 3, 4)
	select {
	case <-writer.writing:
	case <-time.After(time.Second):
		t.Fatal("Stream did not write the buffer")
	}

	closed := make(chan error, 1)
	go func() { closed <- mic.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close during a write that does not return = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close waited for a write that does not return")
	}
	if _, _, closedStream, _ := stream.snapshot(); !closedStream {
		t.Fatal("Close left the capture stream open while Stream waited on its write")
	}
	if mic.IsOpen() {
		t.Fatal("the mic reports open after Close")
	}

	release()
	select {
	case err := <-streamed:
		if !errors.Is(err, errMicClosed) {
			t.Fatalf("Stream = %v, want it ended by Close", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream still running once its write returned")
	}
	if _, _, _, misuse := stream.snapshot(); len(misuse) > 0 {
		t.Fatalf("capture stream misused: %v", misuse)
	}
}

// TestMicCloseLeavesTheStreamToAPortAudioCallThatDoesNotReturn fails if
// Close closes the capture stream under a PortAudio call of Stream's that
// does not return, which frees memory the call still uses, or waits for it
// without a bound, or does not report that the stream stays open; or if a
// Close once the call has returned does not close it.
func TestMicCloseLeavesTheStreamToAPortAudioCallThatDoesNotReturn(t *testing.T) {
	originalWait := micCloseWait
	micCloseWait = 50 * time.Millisecond
	t.Cleanup(func() { micCloseWait = originalWait })
	stream, mic, streamed := openHeldMic(t, 4, io.Discard)
	_, before, _, _ := stream.snapshot()
	stuck := sync.OnceFunc(stream.stallCalls())
	t.Cleanup(stuck) // before openHeldMic's, which waits for Stream
	waitUntil(t, "Stream to enter a PortAudio call that does not return", func() bool { _, polls, _, _ := stream.snapshot(); return polls > before })

	if err := mic.Close(); !errors.Is(err, ErrMicStillReading) {
		t.Fatalf("Close during a PortAudio call that does not return = %v, want ErrMicStillReading", err)
	}
	if _, _, closedStream, _ := stream.snapshot(); closedStream {
		t.Fatal("Close closed the capture stream under a PortAudio call")
	}
	if !mic.IsOpen() {
		t.Fatal("the mic reports closed while its capture stream stays open")
	}

	stuck()
	select {
	case err := <-streamed:
		if !errors.Is(err, errMicClosed) {
			t.Fatalf("Stream = %v, want it ended by the earlier Close", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream still running once its call returned")
	}
	if err := mic.Close(); err != nil {
		t.Fatalf("Close once the call returned = %v, want nil", err)
	}
	if _, _, closedStream, misuse := stream.snapshot(); !closedStream || len(misuse) > 0 {
		t.Fatalf("after the second Close: stream closed=%t, misuse %v; want closed, none", closedStream, misuse)
	}
}

// TestMicIsOpenUntilItsStreamCloses fails if the mic reports closed while it
// still holds its capture stream, as after a read error, before the mic loop
// closes it: the light, which follows the open microphone, would go off with
// the input device open.
func TestMicIsOpenUntilItsStreamCloses(t *testing.T) {
	stream, mic, streamed := openHeldMic(t, 4, io.Discard)
	stream.failNext(errors.New("Input overflowed"))
	select {
	case err := <-streamed:
		if err == nil || errors.Is(err, errMicClosed) {
			t.Fatalf("Stream = %v, want the read error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream did not return the read error")
	}
	if !mic.IsOpen() {
		t.Fatal("the mic reports closed after a read error while it still holds its capture stream")
	}
	if err := mic.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if mic.IsOpen() {
		t.Fatal("the mic reports open after Close")
	}
}
