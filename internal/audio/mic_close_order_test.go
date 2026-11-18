package audio

import (
	"io"
	"sync"
	"testing"
	"time"
)

// TestMicCloseEndsAStreamThatBeganWhileCloseWaited fails if a Stream that
// begins while Close waits for a PortAudio call to return keeps calling the
// capture stream Close then closed: with PortAudio, Pa_GetStreamReadAvailable
// on a stream Pa_CloseStream freed, every 10 ms. Close stopped the Stream it
// found running, waited, then took the stream, so a Stream that registered in
// between kept a stopping channel nothing closed. A pause, a light hold or
// shutdown that lands as the mic loop enters Stream calls Close so (ReviewPR77
// on #77, round 2, finding 1). The control: a Stream already running when
// Close begins.
func TestMicCloseEndsAStreamThatBeganWhileCloseWaited(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closeFirst bool
	}{
		{name: "control: Stream is under way when Close begins"},
		{name: "Close begins, then Stream registers before Close takes the stream", closeFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := openInputStreamFn
			t.Cleanup(func() { openInputStreamFn = original })
			stream := newHeldStream()
			openInputStreamFn = stream.open
			mic, err := NewMic("", 16000, 4)
			if err != nil {
				t.Fatalf("NewMic: %v", err)
			}
			if err := mic.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			streamed := make(chan error, 1)
			closed := make(chan error, 1)
			if tc.closeFirst {
				pa := mic.paLock()
				pa <- struct{}{} // a PortAudio call under way holds pa
				go func() { closed <- mic.Close() }()
				// Close has looked for a Stream to stop, found none, and
				// waits for pa.
				time.Sleep(50 * time.Millisecond)
				go func() { streamed <- mic.Stream(io.Discard) }()
				// Stream has taken the stream and registered its own
				// stopping channel, and waits for pa too.
				time.Sleep(50 * time.Millisecond)
				<-pa
			} else {
				go func() { streamed <- mic.Stream(io.Discard) }()
				waitUntil(t, "Stream to poll", func() bool { _, polls, _, _ := stream.snapshot(); return polls > 0 })
				go func() { closed <- mic.Close() }()
			}
			t.Cleanup(func() {
				_ = mic.Close() // ends a Stream the first Close missed
				stream.abandon()
				select {
				case <-streamed:
				case <-time.After(time.Second):
					t.Error("Stream still running after the cleanup's Close")
				}
			})
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Close = %v, want nil", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not return")
			}
			if _, _, closedStream, _ := stream.snapshot(); !closedStream {
				t.Fatal("Close left the capture stream open")
			}
			// Stream polls every micReadPoll (10 ms): give it 20 polls.
			time.Sleep(200 * time.Millisecond)
			if _, _, _, misuse := stream.snapshot(); len(misuse) > 0 {
				t.Fatalf("Stream called the capture stream after Close closed it: %v", misuse)
			}
		})
	}
}

// slowCloseStream is a capture stream whose Stop or Close blocks until
// release, as Pa_StopStream and Pa_CloseStream take some time on a device.
type slowCloseStream struct {
	blockStop bool
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *slowCloseStream) block() {
	s.once.Do(func() { close(s.entered) })
	<-s.release
}

func (s *slowCloseStream) Start() error { return nil }

func (s *slowCloseStream) Stop() error {
	if s.blockStop {
		s.block()
	}
	return nil
}

func (s *slowCloseStream) Close() error {
	if !s.blockStop {
		s.block()
	}
	return nil
}

func (s *slowCloseStream) AvailableToRead() (int, error) { return 0, nil }
func (s *slowCloseStream) Read() error                   { return nil }

// TestMicIsOpenUntilItsCaptureStreamHasClosed fails if IsOpen, which the
// light controller reads, reports the mic closed while Close is still
// stopping or closing its capture stream: the input device is open then, and
// the light could go off before the microphone closes (ReviewPR77 on #77,
// round 2, finding 2).
func TestMicIsOpenUntilItsCaptureStreamHasClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		blockStop bool
	}{{"during Stop", true}, {"during Close", false}} {
		t.Run(tc.name, func(t *testing.T) {
			original := openInputStreamFn
			t.Cleanup(func() { openInputStreamFn = original })
			s := &slowCloseStream{blockStop: tc.blockStop, entered: make(chan struct{}), release: make(chan struct{})}
			openInputStreamFn = func(_ string, _, _ int, _ *[]int16) (inputStream, error) { return s, nil }
			mic, err := NewMic("", 16000, 4000)
			if err != nil {
				t.Fatal(err)
			}
			if !mic.IsOpen() {
				t.Fatal("control: a mic just opened reports closed")
			}
			done := make(chan error, 1)
			go func() { done <- mic.Close() }()
			select {
			case <-s.entered:
			case <-time.After(time.Second):
				t.Fatal("Close did not reach the stream")
			}
			openDuringClose := mic.IsOpen()
			close(s.release)
			if err := <-done; err != nil {
				t.Fatalf("Close = %v", err)
			}
			if mic.IsOpen() {
				t.Fatal("the mic reports open once Close returned")
			}
			if !openDuringClose {
				t.Fatalf("IsOpen was false %s, while the capture stream was still open", tc.name)
			}
		})
	}
}
