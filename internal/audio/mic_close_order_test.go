package audio

import (
	"io"
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
			mic, err := NewMic(16000, 4)
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
