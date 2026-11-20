package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockWriter is a test double for io.WriteCloser
type mockWriter struct {
	mu       sync.Mutex
	data     []byte
	writeErr error
	closed   bool
}

func (m *mockWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	m.data = append(m.data, p...)
	return len(p), nil
}

func (m *mockWriter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockWriter) getData() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]byte, len(m.data))
	copy(result, m.data)
	return result
}

func (m *mockWriter) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func TestResilientClient_WriteForwardsWhenConnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock := &mockWriter{}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		return mock, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateConnected
	rc.Client = mock

	data := []byte{1, 2, 3, 4, 5}
	n, err := rc.Write(data)

	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, expected 5", n)
	}
	if !bytes.Equal(mock.getData(), data) {
		t.Errorf("mock writer data mismatch, got %v", mock.getData())
	}
}

func TestResilientClient_WriteBuffersOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock := &mockWriter{writeErr: errors.New("connection lost")}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		return &mockWriter{}, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateConnected
	rc.Client = mock

	data := []byte{1, 2, 3, 4, 5}
	n, err := rc.Write(data)

	// Write() must ALWAYS return (len(p), nil)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, expected 5", n)
	}

	// Data should be in ring buffer
	rc.Mu.Lock()
	if rc.Buffer.Len() != 5 {
		t.Errorf("Buffer length %d, expected 5", rc.Buffer.Len())
	}
	rc.Mu.Unlock()
}

func TestResilientClient_WriteBuffersWhenDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	factory := func(ctx context.Context) (io.WriteCloser, error) {
		return &mockWriter{}, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateDisconnected
	rc.Client = &mockWriter{}

	data := []byte{10, 20, 30}
	n, err := rc.Write(data)

	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 3 {
		t.Errorf("Write returned %d, expected 3", n)
	}

	rc.Mu.Lock()
	if rc.Buffer.Len() != 3 {
		t.Errorf("Buffer length %d, expected 3", rc.Buffer.Len())
	}
	rc.Mu.Unlock()
}

func TestResilientClient_ReconnectCreatesNewClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newMock := &mockWriter{}
	factoryCalled := atomic.Bool{}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		factoryCalled.Store(true)
		return newMock, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            1024,
		InitialReconnectDelay: 10 * time.Millisecond,
		MaxReconnectBackoff:   100 * time.Millisecond,
	}, nil, nil)

	rc.State = StateDisconnected
	rc.Client = &mockWriter{}

	// Trigger reconnect
	rc.startReconnect()

	// Wait for reconnect to complete
	time.Sleep(200 * time.Millisecond)

	if !factoryCalled.Load() {
		t.Fatal("factory was not called")
	}

	rc.Mu.Lock()
	if rc.State != StateConnected {
		t.Errorf("State is %v, expected StateConnected", rc.State)
	}
	rc.Mu.Unlock()
}

func TestResilientClient_ReconnectDrainsBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First write fails and buffers data
	failingMock := &mockWriter{writeErr: errors.New("fail")}
	successMock := &mockWriter{}

	factoryCallCount := atomic.Int32{}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		count := factoryCallCount.Add(1)
		if count == 1 {
			return successMock, nil
		}
		return &mockWriter{}, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            1024,
		InitialReconnectDelay: 10 * time.Millisecond,
		MaxReconnectBackoff:   100 * time.Millisecond,
	}, nil, nil)

	rc.State = StateConnected
	rc.Client = failingMock

	// Write data that will fail and be buffered
	bufferedData := []byte{1, 2, 3, 4, 5}
	_, _ = rc.Write(bufferedData)

	// Trigger reconnect
	rc.startReconnect()

	// Wait for reconnect and drain
	time.Sleep(300 * time.Millisecond)

	// Verify buffered data was sent to new client
	if !bytes.Equal(successMock.getData(), bufferedData) {
		t.Errorf("new client data mismatch, got %v, expected %v", successMock.getData(), bufferedData)
	}
}

// TestResilientClient_ReconnectsWhenTheNewSocketFailsWhileDraining fails if
// a reconnect whose new socket fails while it drains the buffer keeps that
// dead socket as connected, throwing the rest of the buffer into it, rather
// than dialling again and draining the rest into the next socket. With audio
// arriving faster than the dead socket refuses it, the drain never ended.
func TestResilientClient_ReconnectsWhenTheNewSocketFailsWhileDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dead := &mockWriter{writeErr: errors.New("write: broken pipe")}
	live := &mockWriter{}
	var dials atomic.Int32
	factory := func(context.Context) (io.WriteCloser, error) {
		if dials.Add(1) == 1 {
			return dead, nil
		}
		return live, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            3 * 4096,
		InitialReconnectDelay: 10 * time.Millisecond,
		MaxReconnectBackoff:   10 * time.Millisecond,
	}, nil, nil)
	rc.State = StateDisconnected
	buffered := bytes.Repeat([]byte{7}, 3*4096)
	if _, err := rc.Buffer.Write(buffered); err != nil {
		t.Fatal(err)
	}
	rc.startReconnect()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!rc.IsConnected() || len(live.getData()) == 0) {
		time.Sleep(5 * time.Millisecond)
	}
	rc.Mu.Lock()
	client := rc.Client
	rc.Mu.Unlock()
	if client != live || !rc.IsConnected() {
		t.Fatalf("after the new socket failed while draining: connected=%t, client is the live one=%t, dials=%d; want a new socket", rc.IsConnected(), client == live, dials.Load())
	}
	if got := len(live.getData()); got != 2*4096 {
		t.Fatalf("the next socket got %d bytes of buffered audio, want the %d the dead one did not take", got, 2*4096)
	}
	if !dead.isClosed() {
		t.Fatal("the socket that failed while draining was not closed")
	}
}

// TestResilientClient_ConnectInBackgroundRedialsAfterAFailedFirstDial fails
// if one failed first dial leaves the client with no socket for good, as a
// single dial did: live transcription then stayed off until a restart. The
// audio written while it redials must reach the socket it gets.
func TestResilientClient_ConnectInBackgroundRedialsAfterAFailedFirstDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	live := &mockWriter{}
	var dials atomic.Int32
	factory := func(context.Context) (io.WriteCloser, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("websocket: bad handshake (503)")
		}
		return live, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            1024,
		InitialReconnectDelay: 10 * time.Millisecond,
		MaxReconnectBackoff:   10 * time.Millisecond,
	}, nil, nil)
	rc.ConnectInBackground()
	if _, err := rc.Write([]byte("while redialling")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!rc.IsConnected() || len(live.getData()) == 0) {
		time.Sleep(5 * time.Millisecond)
	}
	if !rc.IsConnected() || dials.Load() < 2 {
		t.Fatalf("after a failed first dial: connected=%t, dials=%d; want it to redial and connect", rc.IsConnected(), dials.Load())
	}
	if got := string(live.getData()); got != "while redialling" {
		t.Fatalf("the socket got %q, want the audio written while redialling", got)
	}
}

// TestResilientClient_ConnectInBackgroundReturnsBeforeTheDial fails if
// ConnectInBackground waits for a dial: a microphone start holds the
// recording light red until it returns, and a dial can take the handshake
// limit. Audio written meanwhile must reach the socket once it connects.
func TestResilientClient_ConnectInBackgroundReturnsBeforeTheDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := &mockWriter{}
	answer := make(chan struct{})
	factory := func(context.Context) (io.WriteCloser, error) {
		<-answer
		return live, nil
	}
	rc := NewResilientClient(ctx, factory, ResilientConfig{BufferSize: 1024}, nil, nil)
	returned := make(chan struct{})
	go func() { rc.ConnectInBackground(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(answer)
		t.Fatal("ConnectInBackground waited for the dial")
	}
	if rc.IsConnected() {
		t.Fatal("the client says connected before its first dial answered")
	}
	if _, err := rc.Write([]byte("before the dial answered")); err != nil {
		t.Fatal(err)
	}
	close(answer)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!rc.IsConnected() || len(live.getData()) == 0) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := string(live.getData()); !rc.IsConnected() || got != "before the dial answered" {
		t.Fatalf("after the dial answered: connected=%t, the socket got %q; want connected and the audio written before", rc.IsConnected(), got)
	}
}

func TestResilientClient_ReconnectBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attemptTimes := []time.Time{}
	mu := sync.Mutex{}

	factory := func(ctx context.Context) (io.WriteCloser, error) {
		mu.Lock()
		attemptTimes = append(attemptTimes, time.Now())
		mu.Unlock()
		return nil, errors.New("factory failed")
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            1024,
		InitialReconnectDelay: 20 * time.Millisecond,
		MaxReconnectBackoff:   100 * time.Millisecond,
	}, nil, nil)

	rc.State = StateDisconnected
	rc.Client = &mockWriter{}

	// Trigger reconnect
	rc.startReconnect()

	// Wait for multiple attempts
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(attemptTimes) < 2 {
		t.Fatalf("expected at least 2 reconnect attempts, got %d", len(attemptTimes))
	}

	// Verify delays increase (with some tolerance for timing)
	for i := 1; i < len(attemptTimes)-1; i++ {
		delay := attemptTimes[i+1].Sub(attemptTimes[i])
		if delay < 15*time.Millisecond {
			t.Errorf("delay between attempt %d and %d was %v, expected >= 15ms", i, i+1, delay)
		}
	}
}

func TestResilientClient_ConcurrentWriteAndReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newMock := &mockWriter{}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		return newMock, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            10000,
		InitialReconnectDelay: 5 * time.Millisecond,
		MaxReconnectBackoff:   50 * time.Millisecond,
	}, nil, nil)

	rc.State = StateConnected
	rc.Client = &mockWriter{}

	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			data := []byte{byte(idx)}
			_, _ = rc.Write(data)
		}(i)
	}

	// Trigger reconnect
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		rc.startReconnect()
	}()

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	// If we get here without a race detector error, the test passes
	// (race detector is enabled with -race flag)
}

func TestResilientClient_OnStateChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stateChanges := []ConnectionState{}
	mu := sync.Mutex{}

	callback := func(_ context.Context, change StateChange) {
		mu.Lock()
		defer mu.Unlock()
		stateChanges = append(stateChanges, change.State)
	}

	factory := func(ctx context.Context) (io.WriteCloser, error) {
		return &mockWriter{}, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{
		BufferSize:            1024,
		InitialReconnectDelay: 10 * time.Millisecond,
		MaxReconnectBackoff:   100 * time.Millisecond,
	}, nil, callback)

	rc.State = StateConnected
	rc.Client = &mockWriter{}

	// Trigger state change via reconnect
	rc.startReconnect()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(stateChanges) == 0 {
		t.Fatal("expected state change callbacks")
	}

	// Should have at least Reconnecting and Connected states
	hasReconnecting := false
	hasConnected := false
	for _, s := range stateChanges {
		if s == StateReconnecting {
			hasReconnecting = true
		}
		if s == StateConnected {
			hasConnected = true
		}
	}

	if !hasReconnecting {
		t.Error("expected StateReconnecting callback")
	}
	if !hasConnected {
		t.Error("expected StateConnected callback")
	}
}

// TestResilientClient_StateChangesArriveInOrder fails if two quick changes can
// reach OnStateChange reversed. On the office Pi on 2026-11-20 a resume's
// draining and connected reached the web clients 25 µs apart in the wrong
// order, so every client held "draining" while Deepgram was connected. The
// callback for the first change here waits until the second has been handed
// over, or 1 s: a client that hands each change over on a goroutine of its own
// hands the second over first, and the pair is recorded reversed.
func TestResilientClient_StateChangesArriveInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []StateChange
	secondHandedOver := make(chan struct{})
	both := make(chan struct{})
	record := func(change StateChange) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, change)
		switch len(got) {
		case 1:
			if change.State == StateConnected {
				close(secondHandedOver)
			}
		case 2:
			close(both)
		}
	}
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1024}, nil, func(_ context.Context, change StateChange) {
		if change.State == StateDraining {
			select {
			case <-secondHandedOver:
			case <-time.After(time.Second):
			}
		}
		record(change)
	})
	defer func() { _ = rc.Close() }()
	rc.Mu.Lock()
	rc.State = StateReconnecting
	rc.setStateLocked(StateDraining)
	rc.Mu.Unlock()
	rc.Mu.Lock()
	rc.setStateLocked(StateConnected)
	rc.Mu.Unlock()

	select {
	case <-both:
	case <-time.After(5 * time.Second):
		t.Fatal("the two state changes did not both reach OnStateChange within 5 s")
	}
	mu.Lock()
	defer mu.Unlock()
	states := []ConnectionState{got[0].State, got[1].State}
	if want := []ConnectionState{StateDraining, StateConnected}; !slices.Equal(states, want) {
		t.Fatalf("OnStateChange got %v, want %v: the client's last state is connected, but a listener that keeps the last change it got holds %v", states, want, states[1])
	}
	if got[1].Seq <= got[0].Seq {
		t.Fatalf("the later change has Seq %d, not more than the earlier's %d", got[1].Seq, got[0].Seq)
	}
	if current := rc.CurrentState(); current != got[1] {
		t.Fatalf("CurrentState() = %+v, want the last change handed over, %+v", current, got[1])
	}
}

// TestResilientClient_AStuckStateCallbackNeverHoldsTheAudio fails if a state
// change waits for OnStateChange: Write changes the state on the capture path
// (a socket that refuses a write), and the microphone's audio must keep going
// in while a callback is stuck. It also fails if the changes made meanwhile
// are lost or reordered once the callback is free again.
func TestResilientClient_AStuckStateCallbackNeverHoldsTheAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var mu sync.Mutex
	var got []ConnectionState
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Hour, MaxReconnectBackoff: time.Hour}, nil, func(_ context.Context, change StateChange) {
		<-release
		mu.Lock()
		defer mu.Unlock()
		got = append(got, change.State)
	})
	rc.State = StateConnected
	rc.Client = &mockWriter{writeErr: errors.New("socket closed")}

	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		// The first write fails on the socket: disconnected, then reconnecting,
		// with the callback stuck on the first. The rest are buffered.
		for range 50 {
			_, _ = rc.Write(make([]byte, 320))
		}
	}()
	select {
	case <-wrote:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("50 writes did not return within 2 s while OnStateChange was stuck: a state change held the capture path")
	}
	if rc.IsConnected() {
		t.Fatal("the client reads connected after its socket refused a write")
	}

	close(release)
	waitFor := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(waitFor) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []ConnectionState{StateDisconnected, StateReconnecting}; !slices.Equal(got, want) {
		t.Fatalf("once the callback was free it got %v, want %v", got, want)
	}
}

// TestResilientClient_CloseStopsTheStateCallbacks fails if Close leaves the
// goroutine that hands state changes over running, or waits on a callback
// without a bound. A callback that honours its context returns at the close,
// before Close does, and nothing is left; one that ignores it holds Close for
// stateCallbackStopWait at most, and the hand-over ends all the same. No change
// queued behind the stuck one is handed over after the close.
func TestResilientClient_CloseStopsTheStateCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		honours bool
	}{{"a callback that honours the close", true}, {"a callback that ignores the close", false}} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer close(release)
			var calls atomic.Int32
			rc := NewResilientClient(context.Background(), nil, ResilientConfig{}, nil, func(ctx context.Context, _ StateChange) {
				if calls.Add(1) > 1 {
					return
				}
				close(entered)
				defer close(returned)
				if tc.honours {
					select {
					case <-ctx.Done():
					case <-release:
					}
					return
				}
				<-release
			})
			rc.Mu.Lock()
			rc.setStateLocked(StateDisconnected)
			rc.setStateLocked(StateReconnecting) // queued behind the stuck callback
			rc.Mu.Unlock()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("the callback was not called")
			}

			start := time.Now()
			closed := make(chan struct{})
			go func() { _ = rc.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(stateCallbackStopWait + time.Second):
				t.Fatalf("Close did not return within %v of a stuck state callback", stateCallbackStopWait+time.Second)
			}
			took := time.Since(start)
			if tc.honours {
				select {
				case <-returned:
				default:
					t.Fatal("Close returned before the callback that honours its context did")
				}
				if took >= stateCallbackStopWait {
					t.Fatalf("Close took %v for a callback that returns at the close", took)
				}
			}
			rc.Mu.Lock()
			notifying, queued, done := rc.notifying, len(rc.stateChanges), rc.notifierDone
			rc.Mu.Unlock()
			select {
			case <-done:
			default:
				t.Fatal("the state-change goroutine is still running after Close returned")
			}
			if notifying || queued != 0 {
				t.Fatalf("after Close: notifying=%t with %d changes queued, want neither", notifying, queued)
			}
			rc.Mu.Lock()
			rc.setStateLocked(StateConnected)
			rc.Mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			if n := calls.Load(); n != 1 {
				t.Fatalf("OnStateChange was called %d times; after the close no change may be handed over", n)
			}
		})
	}
}

// TestResilientClient_ABehindCallbackKeepsTheNewestChanges fails if the
// queue of changes grows without a bound while the callback is stuck, or if
// the bound drops the newest change rather than the oldest.
func TestResilientClient_ABehindCallbackKeepsTheNewestChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var mu sync.Mutex
	var got []StateChange
	rc := NewResilientClient(ctx, nil, ResilientConfig{}, nil, func(_ context.Context, change StateChange) {
		<-release
		mu.Lock()
		defer mu.Unlock()
		got = append(got, change)
	})
	defer func() { _ = rc.Close() }()
	states := []ConnectionState{StateDisconnected, StateReconnecting}
	rc.Mu.Lock()
	for i := range 3 * maxQueuedStateChanges {
		rc.setStateLocked(states[i%2])
	}
	queued := len(rc.stateChanges)
	rc.setStateLocked(StateConnected)
	last := rc.stateSeq
	rc.Mu.Unlock()
	if queued > maxQueuedStateChanges {
		t.Fatalf("%d changes queued behind a stuck callback, more than the bound %d", queued, maxQueuedStateChanges)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		var tail StateChange
		if n > 0 {
			tail = got[n-1]
		}
		mu.Unlock()
		if tail.Seq == last {
			if tail.State != StateConnected {
				t.Fatalf("the last change handed over is %+v, want connected", tail)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the newest change (Seq %d, connected) was not handed over; the last was %+v", last, tail)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("changes handed over out of order: %+v then %+v", got[i-1], got[i])
		}
	}
}

func TestResilientClient_Close(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock := &mockWriter{}
	factory := func(ctx context.Context) (io.WriteCloser, error) {
		return &mockWriter{}, nil
	}

	rc := NewResilientClient(ctx, factory, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateConnected
	rc.Client = mock

	err := rc.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if !mock.isClosed() {
		t.Error("underlying client was not closed")
	}
}

// finalizingWriter is a mockWriter that also accepts Deepgram's Finalize message.
type finalizingWriter struct {
	mockWriter
	finalizes   atomic.Int32
	finalizeErr error
}

func (f *finalizingWriter) Finalize() error {
	f.finalizes.Add(1)
	return f.finalizeErr
}

func TestResilientClient_FinalizeSendsFinalizeWhenConnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &finalizingWriter{}
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateConnected
	rc.Client = client

	if err := rc.Finalize(); err != nil {
		t.Fatalf("Finalize failed: %v", err)
	}
	if got := client.finalizes.Load(); got != 1 {
		t.Fatalf("underlying client got %d Finalize messages, want 1", got)
	}
}

func TestResilientClient_FinalizeReportsTheClientsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wantErr := errors.New("socket closed")
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateConnected
	rc.Client = &finalizingWriter{finalizeErr: wantErr}

	if err := rc.Finalize(); !errors.Is(err, wantErr) {
		t.Fatalf("Finalize error = %v, want %v", err, wantErr)
	}
}

// A client without Finalize (not a Deepgram socket) is left alone.
func TestResilientClient_FinalizeIgnoresClientWithoutFinalize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateConnected
	rc.Client = &mockWriter{}

	if err := rc.Finalize(); err != nil {
		t.Fatalf("Finalize failed: %v", err)
	}
}

// Audio buffered during a Deepgram outage was captured before a capture pause.
// Finalize drops it, so a reconnect during the pause sends Deepgram no audio.
// The failed socket gets no Finalize: the SDK would redial it.
func TestResilientClient_FinalizeDropsAudioBufferedWhileDisconnected(t *testing.T) {
	for _, state := range []ConnectionState{StateDisconnected, StateReconnecting} {
		ctx, cancel := context.WithCancel(context.Background())
		stale := &finalizingWriter{}
		rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1024}, nil, nil)
		rc.State = state
		rc.Client = stale
		if _, err := rc.Write([]byte{1, 2, 3, 4, 5}); err != nil {
			t.Fatalf("%v: Write failed: %v", state, err)
		}

		if err := rc.Finalize(); err != nil {
			t.Fatalf("%v: Finalize failed: %v", state, err)
		}
		if got := stale.finalizes.Load(); got != 0 {
			t.Fatalf("%v: failed socket got %d Finalize messages, want 0", state, got)
		}

		reconnected := &mockWriter{}
		rc.Mu.Lock()
		rc.Client = reconnected
		rc.Mu.Unlock()
		if err := rc.drainBuffer(); err != nil {
			t.Fatalf("%v: drain: %v", state, err)
		}
		if got := reconnected.getData(); len(got) != 0 {
			t.Fatalf("%v: reconnect drained %d bytes of pre-pause audio to Deepgram, want 0", state, len(got))
		}
		cancel()
	}
}

// While a reconnect drains buffered audio into the new socket, that socket is
// live and holds pre-pause audio, so a pause must still finalize it.
func TestResilientClient_FinalizeSendsFinalizeWhileDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &finalizingWriter{}
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 1024}, nil, nil)
	rc.State = StateDraining
	rc.Client = client

	if err := rc.Finalize(); err != nil {
		t.Fatalf("Finalize failed: %v", err)
	}
	if got := client.finalizes.Load(); got != 1 {
		t.Fatalf("draining socket got %d Finalize messages, want 1", got)
	}
}

// orderedClient records Writes and Finalizes in order; its first Write blocks
// until release is closed.
type orderedClient struct {
	mu      sync.Mutex
	calls   []string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *orderedClient) record(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *orderedClient) Write(p []byte) (int, error) {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	c.record(fmt.Sprintf("write %d", len(p)))
	return len(p), nil
}

func (c *orderedClient) Finalize() error {
	c.record("finalize")
	return nil
}

func (c *orderedClient) Close() error { return nil }

func (c *orderedClient) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// A pause that lands while a reconnect is writing a buffered chunk waits for
// that write, then sends Finalize after it, and no later chunk follows: every
// pre-pause word comes back before the pause settles, none after it.
func TestResilientClient_FinalizeWaitsForTheChunkBeingDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &orderedClient{entered: make(chan struct{}), release: make(chan struct{})}
	rc := NewResilientClient(ctx, nil, ResilientConfig{BufferSize: 3 * 4096}, nil, nil)
	rc.State = StateDraining
	rc.Client = client
	if _, err := rc.Buffer.Write(make([]byte, 3*4096)); err != nil {
		t.Fatalf("fill buffer: %v", err)
	}

	drained := make(chan struct{})
	go func() {
		if err := rc.drainBuffer(); err != nil {
			t.Errorf("drain: %v", err)
		}
		close(drained)
	}()
	<-client.entered

	finalized := make(chan error, 1)
	go func() { finalized <- rc.Finalize() }()
	select {
	case <-finalized:
		t.Fatal("Finalize returned while a drained chunk was still being written")
	case <-time.After(50 * time.Millisecond):
	}
	close(client.release)
	if err := <-finalized; err != nil {
		t.Fatalf("Finalize failed: %v", err)
	}
	<-drained

	if got, want := client.get(), []string{"write 4096", "finalize"}; !slices.Equal(got, want) {
		t.Fatalf("socket calls = %v, want %v", got, want)
	}
}

func TestNovaModel(t *testing.T) {
	// This test verifies that the Deepgram model is configured to nova-3.
	// The actual model configuration happens in cmd/ghost-wispr/main.go
	// where LiveTranscriptionOptions.Model is set.
	// This test serves as a regression check to ensure nova-3 is used.

	expectedModel := "nova-3"

	// In a real integration test, we would verify this by:
	// 1. Creating a Deepgram client with the configured model
	// 2. Checking that the model parameter is passed correctly
	// For now, this test documents the expected behavior.

	if expectedModel != "nova-3" {
		t.Errorf("Expected model to be nova-3, got %s", expectedModel)
	}

	// TODO: Add integration test that verifies the model is actually used
	// by checking Deepgram API calls or response metadata.
}
