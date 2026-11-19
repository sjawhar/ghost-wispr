package transcribe

import (
	"context"
	"io"
	"runtime"
	"testing"
)

// TestAudioWrittenAsADrainEndsIsNotLeftBuffered fails if audio written as a
// reconnect finishes draining its buffer stays in the buffer once the client
// is connected. The drain found the buffer empty, then a write still saw the
// client draining and buffered its audio, then the client became connected:
// nothing drains the buffer while connected, so Deepgram did not hear that
// audio until the next reconnect sent it first, ahead of audio captured
// before it was sent. The race is narrow, so the test runs many reconnects
// against a writer that never stops.
func TestAudioWrittenAsADrainEndsIsNotLeftBuffered(t *testing.T) {
	for range 2000 {
		ctx, cancel := context.WithCancel(context.Background())
		socket := &mockWriter{}
		rc := NewResilientClient(ctx, func(context.Context) (io.WriteCloser, error) { return socket, nil }, ResilientConfig{}, nil, nil)
		rc.Mu.Lock()
		rc.State = StateReconnecting
		rc.Mu.Unlock()
		_, _ = rc.Write([]byte{1, 2})

		stop, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = rc.Write([]byte{1, 2})
				runtime.Gosched()
			}
		}()
		rc.reconnect(0)
		close(stop)
		<-stopped

		rc.Mu.Lock()
		state, left := rc.State, rc.Buffer.Len()
		rc.Mu.Unlock()
		cancel()
		if state != StateConnected {
			t.Fatalf("state after the reconnect = %v, want connected", state)
		}
		if left != 0 {
			t.Fatalf("%d bytes are left in the buffer of a connected client: written as the drain ended, they never reach the socket", left)
		}
	}
}
