package transcribe

import (
	"context"
	"io"
	"testing"
	"time"
)

const secondOfAudio = 16000 * bytesPerSample

// TestCapturedSinceCountsFromTheSocketsFirstByte fails if CapturedSince does
// not time Deepgram's results from the first byte the current socket was
// sent: the oldest audio buffered when it connected, not the first audio the
// client ever took.
func TestCapturedSinceCountsFromTheSocketsFirstByte(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := &mockWriter{}
	rc := NewResilientClient(ctx, func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
	rc.Mu.Lock()
	rc.State = StateReconnecting
	rc.Mu.Unlock()
	// 3 s the client took before this socket, of which the last 2 s wait
	// for it: the first second went to a socket that is gone.
	_, _ = rc.Write(make([]byte, secondOfAudio))
	rc.Mu.Lock()
	rc.Buffer.Reset()
	rc.Mu.Unlock()
	_, _ = rc.Write(make([]byte, 2*secondOfAudio))
	rc.reconnect(0)
	_, _ = rc.Write(make([]byte, secondOfAudio))
	// 1 s into the socket's stream is the second second it was sent; 2 s
	// were captured after that point.
	if got := rc.CapturedSince(time.Second); got != 2*time.Second {
		t.Fatalf("CapturedSince(1s) = %v, want 2s", got)
	}
	if got := rc.MaxBehind(); got != DefaultBufferSeconds*time.Second {
		t.Fatalf("MaxBehind() = %v, want %ds, the default buffer", got, DefaultBufferSeconds)
	}
}

// TestFinalizeTakesTheAudioItDropsOutOfTheStream fails if audio Finalize
// drops from the buffer while a new socket drains it still counts in that
// socket's stream: Deepgram never hears it, so the words it hears after are
// timed earlier in the stream than that audio's place among the captured
// audio.
func TestFinalizeTakesTheAudioItDropsOutOfTheStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := NewResilientClient(ctx, nil, ResilientConfig{}, nil, nil)
	// A socket was sent 1 s and is draining 2 s more, which the pause's
	// Finalize drops.
	rc.Mu.Lock()
	rc.State = StateDraining
	rc.accepted, rc.origin = 3*secondOfAudio, 0
	_, _ = rc.Buffer.Write(make([]byte, 2*secondOfAudio))
	rc.Mu.Unlock()
	if err := rc.Finalize(); err != nil {
		t.Fatal(err)
	}
	// The socket's next audio is captured after the resume.
	_, _ = rc.Write(make([]byte, secondOfAudio))
	if got := rc.CapturedSince(time.Second); got != time.Second {
		t.Fatalf("CapturedSince(1s) = %v, want 1s: the stream's second second is the audio after the resume", got)
	}
}
