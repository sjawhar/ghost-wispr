package transcribe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rejectingSocket is a socket whose first write takes only its first accept
// bytes and then fails, as a socket does that breaks during a drain: with
// accept 0 it takes none, as the app's Deepgram socket does when Deepgram
// closed the stream. When held, that first write waits for release first.
type rejectingSocket struct {
	optionsSocket
	accept int

	held    chan struct{} // closed once the first write is under way, when holding
	release chan struct{}
	first   sync.Once
}

func (s *rejectingSocket) Write(p []byte) (int, error) {
	failed := false
	s.first.Do(func() {
		failed = true
		if s.held != nil {
			close(s.held)
			<-s.release
		}
	})
	if !failed {
		return s.optionsSocket.Write(p)
	}
	n := min(s.accept, len(p))
	if n > 0 {
		if _, err := s.optionsSocket.Write(p[:n]); err != nil {
			return 0, err
		}
	}
	return n, errors.New("the socket broke during the drain")
}

// TestADrainWriteThatFailsKeepsTheBytesItDidNotSend fails if a drain write
// that a socket refuses, wholly or in part, loses the bytes it did not send:
// the drain reads a chunk out of the buffer before it writes it, so the next
// socket got only the audio after that chunk. The bytes the failed socket did
// not take go to the next socket first, before any later audio, so every byte
// arrives once and in order; with an options change pending as well, the next
// socket is dialled with the new options.
func TestADrainWriteThatFailsKeepsTheBytesItDidNotSend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept int
		change bool
	}{
		{name: "zero bytes taken", accept: 0},
		{name: "zero bytes taken, options changed", accept: 0, change: true},
		{name: "part taken", accept: 17},
		{name: "part taken, options changed", accept: 17, change: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failed := &rejectingSocket{accept: tc.accept, held: make(chan struct{}), release: make(chan struct{})}
			var noDelay atomic.Bool
			var counter dialCounter
			var (
				mu    sync.Mutex
				dials []*optionsSocket
			)
			factory := func(context.Context) (io.WriteCloser, error) {
				counter.enter()
				defer counter.leave()
				mu.Lock()
				defer mu.Unlock()
				if len(dials) == 0 {
					dials = append(dials, &failed.optionsSocket)
					return failed, nil
				}
				s := &optionsSocket{noDelay: noDelay.Load()}
				dials = append(dials, s)
				return s, nil
			}
			rc := NewResilientClient(ctx, factory, ResilientConfig{
				BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond,
			}, nil, nil)
			rc.State = StateDisconnected
			audio := audioBytes(15003)
			_, _ = rc.Write(audio[:10000]) // buffered: no socket yet
			rc.startReconnect()
			select {
			case <-failed.held:
			case <-time.After(3 * time.Second):
				t.Fatal("the drain never wrote to the socket that fails")
			}
			if tc.change {
				noDelay.Store(true)
				rc.OptionsChanged()
			}
			_, _ = rc.Write(audio[10000:]) // later audio, buffered during the drain
			close(failed.release)
			waitConnected(t, rc)

			mu.Lock()
			defer mu.Unlock()
			if len(dials) != 2 || counter.maxInDial.Load() != 1 {
				t.Fatalf("the factory dialled %d sockets, at most %d at once, want 2 and 1", len(dials), counter.maxInDial.Load())
			}
			if next := dials[1]; next.noDelay != tc.change {
				t.Errorf("the next socket was dialled with no_delay %t, want %t", next.noDelay, tc.change)
			}
			if !failed.closed || failed.graceful {
				t.Errorf("the socket that failed closed %t, gracefully %t, want closed at once", failed.closed, failed.graceful)
			}
			got := append(failed.got(), dials[1].got()...)
			if !bytes.Equal(got, audio) {
				at := 0
				for at < min(len(got), len(audio)) && got[at] == audio[at] {
					at++
				}
				t.Errorf("the sockets got %d bytes, want the %d written, each once and in order; they differ from byte %d", len(got), len(audio), at)
			}
		})
	}
}

// TestAPauseDropsTheChunkAFailedDrainKept fails if a capture pause leaves the
// bytes a failed drain write kept, from before the pause, for the next
// socket: Finalize drops the audio buffered while disconnected, and the chunk
// the drain kept is part of it.
func TestAPauseDropsTheChunkAFailedDrainKept(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := &rejectingSocket{accept: 17}
	next := &optionsSocket{}
	redial, releaseRedial := make(chan struct{}), make(chan struct{})
	var dials atomic.Int32
	factory := func(context.Context) (io.WriteCloser, error) {
		if dials.Add(1) == 1 {
			return failed, nil
		}
		close(redial)
		<-releaseRedial
		return next, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize: 1 << 16, InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond,
	}, nil, nil)
	rc.State = StateDisconnected
	_, _ = rc.Write(audioBytes(3000)) // before the pause
	rc.startReconnect()
	select {
	case <-redial: // the first socket failed its drain write
	case <-time.After(3 * time.Second):
		t.Fatal("the owner never redialled after the drain failed")
	}
	rc.Mu.Lock()
	kept := len(rc.pending)
	rc.Mu.Unlock()
	if kept != 3000-17 {
		t.Fatalf("control: the failed drain kept %d bytes, want the %d the socket did not take", kept, 3000-17)
	}
	if err := rc.Finalize(); err != nil { // the pause
		t.Fatal(err)
	}
	after := bytes.Repeat([]byte{0xAB}, 500)
	_, _ = rc.Write(after) // after the pause
	close(releaseRedial)
	waitConnected(t, rc)
	if got := next.got(); !bytes.Equal(got, after) {
		t.Errorf("the next socket got %d bytes, want only the %d written after the pause", len(got), len(after))
	}
}
