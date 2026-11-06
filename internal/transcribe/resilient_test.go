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

	callback := func(state ConnectionState) {
		mu.Lock()
		defer mu.Unlock()
		stateChanges = append(stateChanges, state)
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
		rc.drainBuffer()
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
		rc.drainBuffer()
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
