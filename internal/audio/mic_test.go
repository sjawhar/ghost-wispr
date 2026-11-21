package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonklaus/portaudio"
)

// heldStream is a capture stream that holds the frames a test delivers.
// AvailableToRead reports how many it holds, and Read takes len(*window) of
// them. A Read of more frames than it holds waits, as a PortAudio read does,
// until they come or the stream is closed; on a device that delivers no
// frame a PortAudio read never returns, and closing the stream under it frees
// what it still uses. So such a wait is recorded as misuse, as is any call
// once the stream is closed.
type heldStream struct {
	mu      sync.Mutex
	changed *sync.Cond // broadcast (with mu) when frames come or the stream closes
	window  *[]int16
	frames  []int16
	polls   int
	started bool
	stopped bool
	closed  bool
	misuse  []string
	stall   chan struct{} // while set, AvailableToRead does not return until it is closed
	failErr error         // AvailableToRead returns it once
}

func newHeldStream() *heldStream {
	s := &heldStream{}
	s.changed = sync.NewCond(&s.mu)
	return s
}

// open is an openInputStreamFn that opens s.
func (s *heldStream) open(_ string, _, _ int, window *[]int16) (inputStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window = window
	return s, nil
}

func (s *heldStream) use(call string) {
	if s.closed {
		s.misuse = append(s.misuse, call+" after Close")
	}
}

func (s *heldStream) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.use("Start")
	s.started = true
	return nil
}

func (s *heldStream) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.use("Stop")
	s.stopped = true
	return nil
}

func (s *heldStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.use("Close")
	s.closed = true
	s.changed.Broadcast()
	return nil
}

// abandon ends a Read still waiting once the test is over, as the process
// exit does for a PortAudio read that never returns.
func (s *heldStream) abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.changed.Broadcast()
}

func (s *heldStream) AvailableToRead() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.use("AvailableToRead")
	s.polls++
	if stall := s.stall; stall != nil {
		s.mu.Unlock()
		<-stall
		s.mu.Lock()
	}
	if err := s.failErr; err != nil {
		s.failErr = nil
		return 0, err
	}
	return len(s.frames), nil
}

// stallCalls makes the next AvailableToRead calls not return, as a PortAudio
// call that hangs, until the returned func is called.
func (s *heldStream) stallCalls() (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stall := make(chan struct{})
	s.stall = stall
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stall == stall {
			s.stall = nil
		}
		close(stall)
	}
}

// failNext makes the next AvailableToRead call return err.
func (s *heldStream) failNext(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failErr = err
}

func (s *heldStream) Read() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.use("Read")
	n := len(*s.window)
	if n > len(s.frames) {
		s.misuse = append(s.misuse, fmt.Sprintf("Read of %d frames waited while the stream held %d", n, len(s.frames)))
	}
	for n > len(s.frames) && !s.closed {
		s.changed.Wait()
	}
	if n > len(s.frames) {
		return errors.New("stream closed under a read")
	}
	copy(*s.window, s.frames[:n])
	s.frames = s.frames[n:]
	return nil
}

func (s *heldStream) deliver(frames ...int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frames...)
	s.changed.Broadcast()
}

func (s *heldStream) snapshot() (held, polls int, closed bool, misuse []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames), s.polls, s.closed, slices.Clone(s.misuse)
}

// waitUntil fails the test if cond does not hold within a second.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// openHeldMic opens a Mic of framesPerBuffer frames on a heldStream, starts
// it and runs its Stream into w. It returns the stream, the mic and Stream's
// result.
func openHeldMic(t *testing.T, framesPerBuffer int, w io.Writer) (*heldStream, *Mic, <-chan error) {
	t.Helper()
	original := openInputStreamFn
	t.Cleanup(func() { openInputStreamFn = original })
	stream := newHeldStream()
	openInputStreamFn = stream.open
	mic, err := NewMic("", 16000, framesPerBuffer)
	if err != nil {
		t.Fatalf("NewMic: %v", err)
	}
	if err := mic.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	streamed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamed <- mic.Stream(w)
	}()
	t.Cleanup(func() {
		_ = mic.Close()
		stream.abandon()
		<-done
	})
	return stream, mic, streamed
}

// buffers is a writer that hands each buffer it gets to a channel.
type buffers chan []byte

func (b buffers) Write(p []byte) (int, error) {
	b <- bytes.Clone(p)
	return len(p), nil
}

// TestMicStreamAssemblesEachBufferFromTheFramesTheStreamHolds fails if Stream
// reads more frames than the capture stream holds, so that a PortAudio read
// would wait for the device, or does not write the frames it reads as whole
// buffers of framesPerBuffer frames, in order.
func TestMicStreamAssemblesEachBufferFromTheFramesTheStreamHolds(t *testing.T) {
	written := make(buffers, 4)
	stream, _, _ := openHeldMic(t, 8, written)
	for _, piece := range [][]int16{{1, 2, 3}, {4, 5, 6}, {7, 8, 9, 10}, {11, 12, 13, 14, 15, 16}} {
		stream.deliver(piece...)
		waitUntil(t, "Stream to read the frames delivered", func() bool { held, _, _, _ := stream.snapshot(); return held == 0 })
	}
	for want := int16(1); want <= 16; want += 8 {
		select {
		case got := <-written:
			samples := make([]int16, len(got)/2)
			if err := binary.Read(bytes.NewReader(got), binary.LittleEndian, samples); err != nil {
				t.Fatal(err)
			}
			if wantSamples := []int16{want, want + 1, want + 2, want + 3, want + 4, want + 5, want + 6, want + 7}; !slices.Equal(samples, wantSamples) {
				t.Fatalf("buffer = %v, want %v", samples, wantSamples)
			}
		case <-time.After(time.Second):
			t.Fatalf("no buffer starting at frame %d was written", want)
		}
	}
	if _, _, _, misuse := stream.snapshot(); len(misuse) > 0 {
		t.Fatalf("capture stream misused: %v", misuse)
	}
}

// TestMicCloseEndsAStreamOnADeviceThatDeliversNoFrame fails if a Close from
// another goroutine, as a pause's, does not end a Stream on a device that
// delivers no frame and close the capture stream at once, or if Stream waits
// inside a read for a frame that never comes. On the office Pi (2026-11-17) a
// capture device that delivered no frame kept Mic.Stream inside Pa_ReadStream,
// and the paused unit held its microphone until the process exited.
func TestMicCloseEndsAStreamOnADeviceThatDeliversNoFrame(t *testing.T) {
	stream, mic, streamed := openHeldMic(t, 4000, io.Discard)
	waitUntil(t, "Stream to look for frames", func() bool { _, polls, _, _ := stream.snapshot(); return polls > 0 })

	closed := make(chan error, 1)
	go func() { closed <- mic.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return within 1 s")
	}
	select {
	case err := <-streamed:
		if !errors.Is(err, errMicClosed) {
			t.Fatalf("Stream = %v, want it ended by Close", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream still running after Close")
	}
	_, _, closedStream, misuse := stream.snapshot()
	if !closedStream {
		t.Fatal("Close left the capture stream open")
	}
	if len(misuse) > 0 {
		t.Fatalf("capture stream misused: %v", misuse)
	}
	if mic.IsOpen() {
		t.Fatal("the mic reports open after Close")
	}
}

// blockedWriter blocks each write until release closes.
type blockedWriter struct {
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.release
	return len(p), nil
}

func TestNewMicUsesOpenInputStreamFn(t *testing.T) {
	original := openInputStreamFn
	t.Cleanup(func() {
		openInputStreamFn = original
	})

	wantErr := errors.New("boom")
	openInputStreamFn = func(_ string, sampleRate, framesPerBuffer int, window *[]int16) (inputStream, error) {
		if sampleRate != 16000 {
			t.Fatalf("unexpected sample rate: %d", sampleRate)
		}
		if framesPerBuffer != 4000 {
			t.Fatalf("unexpected framesPerBuffer: %d", framesPerBuffer)
		}
		if window == nil {
			t.Fatal("NewMic gave the stream no buffer to read into")
		}
		return nil, wantErr
	}

	_, err := NewMic("", 16000, 4000)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}

	openInputStreamFn = func(string, int, int, *[]int16) (inputStream, error) { return newHeldStream(), nil }
	mic, err := NewMic("", 16000, 4000)
	if err != nil {
		t.Fatalf("NewMic: %v", err)
	}
	if len(mic.buf) != 4000 {
		t.Fatalf("buffer of %d frames, want 4000", len(mic.buf))
	}
}

// stubPortAudioLifecycle makes PortAudio's terminate and initialize do
// nothing for the rest of the test: a real initialization connects to the
// sound server and lists its devices, which a test may not do (realDevice).
func stubPortAudioLifecycle(t *testing.T) {
	t.Helper()
	originalInitialize, originalTerminate := paInitialize, paTerminate
	t.Cleanup(func() { paInitialize, paTerminate = originalInitialize, originalTerminate })
	paInitialize = func() error { return nil }
	paTerminate = func() error { return nil }
}

func TestMicReopenFailureClearsStreamAndMarksInactive(t *testing.T) {
	original := openInputStreamFn
	t.Cleanup(func() {
		openInputStreamFn = original
	})
	stubPortAudioLifecycle(t)

	openInputStreamFn = func(string, int, int, *[]int16) (inputStream, error) {
		return nil, errors.New("open failed")
	}

	mic := &Mic{
		sampleRate:      16000,
		framesPerBuffer: 4000,
		buf:             make([]int16, 4000),
	}
	mic.held.Store(true)

	err := mic.Reopen()
	if err == nil {
		t.Fatal("expected reopen error")
	}
	if !strings.Contains(err.Error(), "open failed") {
		t.Fatalf("expected open failure in error, got %v", err)
	}
	if mic.stream != nil {
		t.Fatal("expected nil stream after failed reopen")
	}
	if mic.IsOpen() {
		t.Fatal("expected a closed mic after failed reopen")
	}
}

func TestMicStreamReturnsErrorWhenStreamUnavailable(t *testing.T) {
	mic := &Mic{}

	err := mic.Stream(bytes.NewBuffer(nil))
	if err == nil {
		t.Fatal("expected stream error")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMicStopIsSafeWhenStreamUnavailable(t *testing.T) {
	mic := &Mic{}
	if err := mic.Stop(); err != nil {
		t.Fatalf("expected nil stop error, got %v", err)
	}
	if mic.IsOpen() {
		t.Fatal("a mic with no stream reports open")
	}
}

// TestMicCloseReleasesTheStream fails if Close leaves the mic holding its
// capture stream, so the input device stays open, or reporting open.
func TestMicCloseReleasesTheStream(t *testing.T) {
	mic := &Mic{stream: &portaudio.Stream{}, sampleRate: 16000, framesPerBuffer: 4000, buf: make([]int16, 4000)}
	mic.held.Store(true)

	_ = mic.Close() // the stand-in stream has no PortAudio stream behind it
	if mic.stream != nil {
		t.Fatal("the mic still holds its capture stream after Close")
	}
	if mic.IsOpen() {
		t.Fatal("the mic reports open after Close")
	}
	if err := mic.Start(); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Start after Close = %v, want the stream unavailable until Open or Reopen", err)
	}
	if err := mic.Close(); err != nil {
		t.Fatalf("a second Close = %v, want nil", err)
	}
}

// IsOpen is read from HTTP handlers (status "capturing", health) while the mic
// loop reopens the stream; under -race this must not be a data race.
func TestMicIsOpenIsSafeDuringReopen(t *testing.T) {
	original := openInputStreamFn
	t.Cleanup(func() {
		openInputStreamFn = original
	})
	stubPortAudioLifecycle(t)
	// A stream that opens but cannot start: each Reopen opens and closes one.
	openInputStreamFn = func(string, int, int, *[]int16) (inputStream, error) {
		return &portaudio.Stream{}, nil
	}

	mic := &Mic{sampleRate: 16000, framesPerBuffer: 4000, buf: make([]int16, 4000)}
	mic.held.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_ = mic.IsOpen()
		}
	}()
	for range 5 {
		_ = mic.Reopen()
	}
	<-done

	if mic.IsOpen() {
		t.Fatal("expected a mic whose reopen failed to report closed")
	}
}

// TestMicOpenDoesNotReinitializePortAudio fails if Open, which a resume uses
// after a pause closed the stream, tears PortAudio down and initializes it
// again, which lists every device, or keeps a stream that did not start.
func TestMicOpenDoesNotReinitializePortAudio(t *testing.T) {
	originalOpen, originalInitialize, originalTerminate := openInputStreamFn, paInitialize, paTerminate
	t.Cleanup(func() {
		openInputStreamFn, paInitialize, paTerminate = originalOpen, originalInitialize, originalTerminate
	})
	opens := 0
	// A stand-in stream opens but cannot start.
	openInputStreamFn = func(string, int, int, *[]int16) (inputStream, error) {
		opens++
		return &portaudio.Stream{}, nil
	}
	paInitialize = func() error { t.Error("Open initialized PortAudio"); return nil }
	paTerminate = func() error { t.Error("Open terminated PortAudio"); return nil }

	mic := &Mic{sampleRate: 16000, framesPerBuffer: 4000, buf: make([]int16, 4000)}
	if err := mic.Open(); err == nil {
		t.Fatal("Open of a stream that cannot start succeeded")
	}
	if opens != 1 {
		t.Fatalf("Open opened %d streams, want 1", opens)
	}
	if mic.stream != nil || mic.IsOpen() {
		t.Fatal("a failed Open left the mic holding its stream")
	}
}
