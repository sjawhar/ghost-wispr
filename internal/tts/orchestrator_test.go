package tts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// ---------------------------------------------------------------------------
// Mock provider
// ---------------------------------------------------------------------------

type mockProvider struct {
	mu          sync.Mutex
	name        string
	audio       []byte
	format      audio.AudioFormat
	durationMs  int64
	err         error
	calls       int
	synthesized []SpeechRequest
	delay       time.Duration
}

func (m *mockProvider) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error) {
	m.mu.Lock()
	m.calls++
	m.synthesized = append(m.synthesized, req)
	err := m.err
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if err != nil {
		return nil, err
	}
	return &SpeechResponse{
		Audio:      m.audio,
		Format:     m.format,
		DurationMs: m.durationMs,
	}, nil
}

func (m *mockProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mock"
}

// blockingMockProvider wraps mockProvider but blocks Synthesize on a gate channel.
type blockingMockProvider struct {
	*mockProvider
	gate chan struct{}
}

func (b *blockingMockProvider) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error) {
	select {
	case <-b.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.mockProvider.Synthesize(ctx, req)
}

func (b *blockingMockProvider) Name() string { return b.mockProvider.Name() }

// ---------------------------------------------------------------------------
// Test helper: create speaker with mock stream
// ---------------------------------------------------------------------------

// mockOutputStream implements audio.outputStream for testing.
type mockOutputStream struct {
	started  bool
	stopped  bool
	closed   bool
	writeErr error
}

func (m *mockOutputStream) Start() error { m.started = true; return nil }
func (m *mockOutputStream) Stop() error  { m.stopped = true; return nil }
func (m *mockOutputStream) Write() error { return m.writeErr }
func (m *mockOutputStream) Close() error { m.closed = true; return nil }

func newTestSpeaker() *audio.Speaker {
	s := audio.NewSpeaker("")
	// Override openStream to avoid PortAudio dependency.
	s.SetOpenStreamForTest(func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (audio.OutputStream, error) {
		return &mockOutputStream{}, nil
	})
	return s
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOrchestrator_SpeakAndStatus(t *testing.T) {
	provider := &mockProvider{
		audio:      []byte{0x01, 0x02, 0x03, 0x04}, // 2 PCM16 samples
		format:     audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16},
		durationMs: 100,
	}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		QueueDepth:        5,
		MaxRequestsPerMin: 100, // high limit for test
		SynthesisTimeout:  5 * time.Second,
	})
	defer o.Stop()

	id, err := o.Speak("hello world", "default", "mock")
	if err != nil {
		t.Fatalf("Speak failed: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	// Wait for processing.
	deadline := time.After(3 * time.Second)
	for {
		status, ok := o.Status(id)
		if !ok {
			t.Fatal("status not found for id")
		}
		if status.Status == StatusCompleted {
			if status.BytesWritten <= 0 {
				t.Errorf("expected bytes_written > 0, got %d", status.BytesWritten)
			}
			break
		}
		if status.Status == StatusFailed {
			t.Fatalf("request failed: %s", status.Error)
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for completion, current status: %s", status.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestOrchestrator_SpeakEmptyText(t *testing.T) {
	provider := &mockProvider{}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()

	_, err := o.Speak("", "default", "mock")
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestOrchestrator_QueueFull(t *testing.T) {
	// Use a gate channel so the mock provider blocks until we release it.
	gate := make(chan struct{})
	provider := &mockProvider{
		audio:  []byte{0x01, 0x02},
		format: audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16},
	}
	// Override Synthesize to block on gate.
	origSynth := provider.Synthesize
	_ = origSynth // suppress unused
	blockingProvider := &blockingMockProvider{gate: gate, mockProvider: provider}
	speaker := newTestSpeaker()
	o := NewOrchestrator(blockingProvider, speaker, OrchestratorOpts{
		QueueDepth:        2,
		MaxRequestsPerMin: 100,
		SynthesisTimeout:  30 * time.Second,
	})
	defer o.Stop()

	// First Speak is picked up by the worker and blocks on gate.
	_, err := o.Speak("text 0", "", "")
	if err != nil {
		t.Fatalf("Speak 0 failed: %v", err)
	}
	// Give the worker time to pick it up.
	time.Sleep(50 * time.Millisecond)

	// Next 2 fill the channel buffer (depth=2).
	for i := 1; i <= 2; i++ {
		_, err := o.Speak(fmt.Sprintf("text %d", i), "", "")
		if err != nil {
			t.Fatalf("Speak %d failed unexpectedly: %v", i, err)
		}
	}

	// Next should fail with queue full.
	_, err = o.Speak("overflow", "", "")
	if err != ErrQueueFull {
		t.Fatalf("expected ErrQueueFull, got: %v", err)
	}

	// Release the gate so goroutines can finish.
	close(gate)
}

func TestOrchestrator_RateLimit(t *testing.T) {
	provider := &mockProvider{
		audio:  []byte{0x01, 0x02},
		format: audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16},
	}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		QueueDepth:        100,
		MaxRequestsPerMin: 3,
		SynthesisTimeout:  5 * time.Second,
	})
	defer o.Stop()

	// Use up all rate limit slots.
	for i := 0; i < 3; i++ {
		_, err := o.Speak(fmt.Sprintf("text %d", i), "", "")
		if err != nil {
			t.Fatalf("Speak %d failed: %v", i, err)
		}
	}

	// Next should be rate limited.
	_, err := o.Speak("rate limited", "", "")
	if err != ErrRateLimited {
		t.Fatalf("expected ErrRateLimited, got: %v", err)
	}
}

func TestOrchestrator_SynthesisError(t *testing.T) {
	provider := &mockProvider{err: fmt.Errorf("synthesis failed")}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		MaxRequestsPerMin: 100,
		SynthesisTimeout:  5 * time.Second,
	})
	defer o.Stop()

	id, err := o.Speak("hello", "", "")
	if err != nil {
		t.Fatalf("Speak failed: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		status, ok := o.Status(id)
		if !ok {
			t.Fatal("status not found")
		}
		if status.Status == StatusFailed {
			if status.Error == "" {
				t.Error("expected non-empty error message")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for failure, status: %s", status.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestOrchestrator_SynthesisTimeout(t *testing.T) {
	provider := &mockProvider{
		audio:  []byte{0x01, 0x02},
		format: audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16},
		delay:  5 * time.Second,
	}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		MaxRequestsPerMin: 100,
		SynthesisTimeout:  50 * time.Millisecond,
	})
	defer o.Stop()

	id, err := o.Speak("hello", "", "")
	if err != nil {
		t.Fatalf("Speak failed: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		status, ok := o.Status(id)
		if !ok {
			t.Fatal("status not found")
		}
		if status.Status == StatusFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for failure, status: %s", status.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestOrchestrator_StatusUnknownID(t *testing.T) {
	provider := &mockProvider{}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()

	_, ok := o.Status("nonexistent")
	if ok {
		t.Fatal("expected false for unknown ID")
	}
}

func TestOrchestrator_HasProviderAndSpeaker(t *testing.T) {
	provider := &mockProvider{}
	speaker := newTestSpeaker()
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{})
	defer o.Stop()

	if !o.HasProvider() {
		t.Error("expected HasProvider to be true")
	}
	if !o.HasSpeaker() {
		t.Error("expected HasSpeaker to be true")
	}
}

func TestOrchestrator_DefaultOpts(t *testing.T) {
	opts := OrchestratorOpts{}
	resolved := opts.withDefaults()
	if resolved.QueueDepth != DefaultQueueDepth {
		t.Errorf("expected QueueDepth %d, got %d", DefaultQueueDepth, resolved.QueueDepth)
	}
	if resolved.MaxRequestsPerMin != DefaultMaxRequestsPerMin {
		t.Errorf("expected MaxRequestsPerMin %d, got %d", DefaultMaxRequestsPerMin, resolved.MaxRequestsPerMin)
	}
	if resolved.SynthesisTimeout != DefaultSynthesisTimeout {
		t.Errorf("expected SynthesisTimeout %v, got %v", DefaultSynthesisTimeout, resolved.SynthesisTimeout)
	}
}
