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
	model       string
	voice       string
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
		Model:      m.model,
		Voice:      m.voice,
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
// Test helper: an Output on a stream that plays in no time
// ---------------------------------------------------------------------------

// mockOutputStream is a speaker stream that takes each frame at once.
type mockOutputStream struct{}

func (mockOutputStream) Write([]int16) error { return nil }
func (mockOutputStream) Drop() (int, error)  { return 0, nil }
func (mockOutputStream) Close() error        { return nil }

// blockingOutputStream reports its first write on entered, then holds it
// until release closes.
type blockingOutputStream struct {
	mockOutputStream
	once    sync.Once
	entered chan<- struct{}
	release <-chan struct{}
}

func (s *blockingOutputStream) Write([]int16) error {
	s.once.Do(func() { s.entered <- struct{}{} })
	<-s.release
	return nil
}

// newOutputOn makes a speaker Output on stream, closed when the test ends.
func newOutputOn(t *testing.T, stream audio.PlaybackStream) *audio.Output {
	t.Helper()
	out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) {
		return stream, 48000, nil
	}})
	t.Cleanup(func() { _ = out.Close() })
	return out
}

func newTestSpeaker(t *testing.T) *audio.Output {
	return newOutputOn(t, mockOutputStream{})
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
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		QueueDepth:        5,
		MaxRequestsPerMin: 100, // high limit for test
		SynthesisTimeout:  5 * time.Second,
	})
	defer o.Stop()

	id, err := o.Speak("hello world", "default")
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

// TestOrchestratorReportsEveryBytePlayed fails if a speech that played all of
// its audio reports fewer bytes than it had: 2401 frames at 24 kHz last a
// duration that is no whole number of nanoseconds, and truncating it loses a
// frame.
func TestOrchestratorReportsEveryBytePlayed(t *testing.T) {
	provider := &mockProvider{
		audio:  make([]byte, 2*2401),
		format: audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16},
	}
	o := NewOrchestrator(provider, newTestSpeaker(t), OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()
	id, err := o.Speak("hello", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, _ := o.Status(id)
		if status.Status == StatusCompleted {
			if status.BytesWritten != 2*2401 {
				t.Fatalf("bytes_written = %d, want all %d", status.BytesWritten, 2*2401)
			}
			return
		}
		if status.Status == StatusFailed || time.Now().After(deadline) {
			t.Fatalf("status = %s (%s), want completed", status.Status, status.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOrchestratorWaitForSynthesisUsesProviderModelBeforePlaybackEnds(t *testing.T) {
	provider := &mockProvider{
		audio:  []byte{0x01, 0x00},
		format: audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16},
		model:  "provider-tts-model",
		voice:  "provider-tts-voice",
	}
	playEntered := make(chan struct{}, 1)
	releasePlay := make(chan struct{})
	speaker := newOutputOn(t, &blockingOutputStream{entered: playEntered, release: releasePlay})
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()

	id, err := o.Speak("model acceptance", "")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	type report struct{ model, voice string }
	waited := make(chan report, 1)
	go func() {
		status, waitErr := o.WaitForSynthesis(context.Background(), id)
		if waitErr != nil {
			t.Errorf("WaitForSynthesis: %v", waitErr)
			return
		}
		waited <- report{model: status.Model, voice: status.Voice}
	}()
	select {
	case <-playEntered:
	case <-time.After(time.Second):
		t.Fatal("playback did not start")
	}
	select {
	case status := <-waited:
		if status.model != provider.model || status.voice != provider.voice {
			t.Fatalf("provider report = %q/%q, want %q/%q", status.model, status.voice, provider.model, provider.voice)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForSynthesis did not return while Play was blocked")
	}
	close(releasePlay)
}

func TestOrchestrator_SpeakEmptyText(t *testing.T) {
	provider := &mockProvider{}
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()

	_, err := o.Speak("", "default")
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
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(blockingProvider, speaker, OrchestratorOpts{
		QueueDepth:        2,
		MaxRequestsPerMin: 100,
		SynthesisTimeout:  30 * time.Second,
	})
	defer o.Stop()

	// First Speak is picked up by the worker and blocks on gate.
	_, err := o.Speak("text 0", "")
	if err != nil {
		t.Fatalf("Speak 0 failed: %v", err)
	}
	// Give the worker time to pick it up.
	time.Sleep(50 * time.Millisecond)

	// Next 2 fill the channel buffer (depth=2).
	for i := 1; i <= 2; i++ {
		_, err := o.Speak(fmt.Sprintf("text %d", i), "")
		if err != nil {
			t.Fatalf("Speak %d failed unexpectedly: %v", i, err)
		}
	}

	// Next should fail with queue full.
	_, err = o.Speak("overflow", "")
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
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		QueueDepth:        100,
		MaxRequestsPerMin: 3,
		SynthesisTimeout:  5 * time.Second,
	})
	defer o.Stop()

	// Use up all rate limit slots.
	for i := 0; i < 3; i++ {
		_, err := o.Speak(fmt.Sprintf("text %d", i), "")
		if err != nil {
			t.Fatalf("Speak %d failed: %v", i, err)
		}
	}

	// Next should be rate limited.
	_, err := o.Speak("rate limited", "")
	if err != ErrRateLimited {
		t.Fatalf("expected ErrRateLimited, got: %v", err)
	}
}

func TestOrchestrator_SynthesisError(t *testing.T) {
	provider := &mockProvider{err: fmt.Errorf("synthesis failed")}
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		MaxRequestsPerMin: 100,
		SynthesisTimeout:  5 * time.Second,
	})
	defer o.Stop()

	id, err := o.Speak("hello", "")
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
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		MaxRequestsPerMin: 100,
		SynthesisTimeout:  50 * time.Millisecond,
	})
	defer o.Stop()

	id, err := o.Speak("hello", "")
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
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()

	_, ok := o.Status("nonexistent")
	if ok {
		t.Fatal("expected false for unknown ID")
	}
}

func TestOrchestrator_HasProviderAndSpeaker(t *testing.T) {
	provider := &mockProvider{}
	speaker := newTestSpeaker(t)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{})
	defer o.Stop()

	if !o.HasProvider() {
		t.Error("expected HasProvider to be true")
	}
	if !o.HasSpeaker() {
		t.Error("expected HasSpeaker to be true")
	}
}

// switchedProvider is a mockProvider that says whether it is configured, as
// cmd's speechSource does by tts.model.
type switchedProvider struct {
	mockProvider
	configured bool
}

func (p *switchedProvider) Configured() bool { return p.configured }

// TestAnUnconfiguredProviderIsNoProvider fails if the Orchestrator counts a
// provider whose Configured is false as a provider, or one whose Configured
// is true as none: with text-to-speech off (tts.model ""), POST
// /api/tts/speak would queue the request, spend a rate-limit slot and answer
// 502, where it answers 501 for no provider.
func TestAnUnconfiguredProviderIsNoProvider(t *testing.T) {
	provider := &switchedProvider{}
	o := NewOrchestrator(provider, newTestSpeaker(t), OrchestratorOpts{})
	defer o.Stop()
	if o.HasProvider() {
		t.Error("HasProvider is true for a provider that is not configured")
	}
	provider.configured = true
	if !o.HasProvider() {
		t.Error("HasProvider is false for a configured provider")
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

// pacedOutputStream is a speaker stream that plays in real time at 48 kHz and
// keeps what it played.
type pacedOutputStream struct {
	mockOutputStream
	mu     sync.Mutex
	played []int16
}

func (s *pacedOutputStream) Write(frame []int16) error {
	s.mu.Lock()
	s.played = append(s.played, frame...)
	s.mu.Unlock()
	time.Sleep(time.Duration(len(frame)) * time.Second / 48000)
	return nil
}

// TestOrchestratorSpeechStopsOnACancelOfTheVoice fails if a speech does not
// play on the speaker's voice input, or plays on after Cancel of the voice,
// as a barge-in makes, or is then reported failed rather than completed with
// the part that played.
func TestOrchestratorSpeechStopsOnACancelOfTheVoice(t *testing.T) {
	pcm := make([]byte, 2*48000*2) // 2 s at 48 kHz
	for i := 0; i < len(pcm); i += 2 {
		pcm[i] = 0xE8 // 1000
		pcm[i+1] = 0x03
	}
	provider := &mockProvider{audio: pcm, format: audio.AudioFormat{SampleRate: 48000, Channels: 1, Encoding: audio.EncodingPCM16}}
	stream := &pacedOutputStream{}
	out := newOutputOn(t, stream)
	started := make(chan struct{}, 1)
	remove := out.OnSpeech(func(e audio.SpeechEvent) {
		if e.Input == audio.InputVoice && e.Started {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	})
	defer remove()
	o := NewOrchestrator(provider, out, OrchestratorOpts{MaxRequestsPerMin: 100})
	defer o.Stop()

	id, err := o.Speak("a long answer", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the speech did not start on the voice input")
	}
	time.Sleep(200 * time.Millisecond)
	out.Cancel(audio.InputVoice)

	deadline := time.Now().Add(3 * time.Second)
	for {
		status, _ := o.Status(id)
		if status.Status == StatusCompleted {
			if status.DurationMs <= 0 || status.DurationMs >= 1000 {
				t.Fatalf("a speech cancelled after 200 ms reports %d ms played", status.DurationMs)
			}
			if want := status.DurationMs * 48 * 2; status.BytesWritten < want-96 || status.BytesWritten > want+96 {
				t.Fatalf("bytes_written = %d for %d ms at 48 kHz mono, want about %d", status.BytesWritten, status.DurationMs, want)
			}
			break
		}
		if status.Status == StatusFailed || time.Now().After(deadline) {
			t.Fatalf("status after Cancel of the voice = %s (%s), want completed", status.Status, status.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stream.mu.Lock()
	heard := len(stream.played)
	stream.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for _, s := range stream.played[heard:] {
		if s != 0 {
			t.Fatal("the speech played on after Cancel of the voice")
		}
	}
}

// heldDropStream holds the mixer's second write until release closes, having
// closed entered, and gives back back frames on a Drop.
type heldDropStream struct {
	mockOutputStream
	writes           int // only the mixer touches it
	entered, release chan struct{}
	back             int
}

func (s *heldDropStream) Write([]int16) error {
	if s.writes++; s.writes == 2 {
		close(s.entered)
		<-s.release
	}
	return nil
}

func (s *heldDropStream) Drop() (int, error) { return s.back, nil }

// TestOrchestratorReportsWhatPlayedOfASpeechCancelledInItsWrite fails if a
// speech longer than the voice input queues, cancelled while its write
// waits for room, reports what played before the Cancel's take-back was
// counted: 20 ms reported, though the device gave both frames back unheard
// (Sol's review of #153 at 4b9830e). The control's Drop gives nothing back,
// and the 20 ms played stay.
func TestOrchestratorReportsWhatPlayedOfASpeechCancelledInItsWrite(t *testing.T) {
	for _, tc := range []struct{ back, wantMs int }{{0, 20}, {960, 0}} {
		t.Run(fmt.Sprintf("the drop gives back %d frames", tc.back), func(t *testing.T) {
			pcm := make([]byte, 2*48000*4/10) // 400 ms at 48 kHz, more than the input queues
			for i := 0; i < len(pcm); i += 2 {
				pcm[i], pcm[i+1] = 0xE8, 0x03 // 1000
			}
			provider := &mockProvider{audio: pcm, format: audio.AudioFormat{SampleRate: 48000, Channels: 1, Encoding: audio.EncodingPCM16}}
			stream := &heldDropStream{entered: make(chan struct{}), release: make(chan struct{}), back: tc.back}
			released := false
			release := func() {
				if !released {
					released = true
					close(stream.release)
				}
			}
			t.Cleanup(release)
			out := newOutputOn(t, stream)
			o := NewOrchestrator(provider, out, OrchestratorOpts{MaxRequestsPerMin: 100})
			defer o.Stop()
			id, err := o.Speak("a long answer", "")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-stream.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("the mixer never reached its second write")
			}
			out.Cancel(audio.InputVoice)
			time.Sleep(100 * time.Millisecond) // a play that does not wait reads Played now
			release()
			deadline := time.Now().Add(3 * time.Second)
			for {
				status, _ := o.Status(id)
				if status.Status == StatusCompleted {
					if status.DurationMs != int64(tc.wantMs) || status.BytesWritten != int64(tc.wantMs*48*2) {
						t.Fatalf("the cancelled speech reports %d ms, %d bytes played, want %d ms, %d bytes", status.DurationMs, status.BytesWritten, tc.wantMs, tc.wantMs*48*2)
					}
					return
				}
				if status.Status == StatusFailed || time.Now().After(deadline) {
					t.Fatalf("status after Cancel of the voice = %s (%s), want completed", status.Status, status.Error)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestAStoppedOrchestratorReportsNoPlayedBeforeTheTakeBack fails if a speech
// cancelled in its write, whose orchestrator stops while the take-back is
// still to come, completes with what it read as played before that
// take-back: 20 ms and 1920 bytes, where the room heard none (Sol's review
// of #153 at 80418b0). It must fail, or complete with what played. The
// control does not stop, and completes with 0 ms once the take-back comes.
func TestAStoppedOrchestratorReportsNoPlayedBeforeTheTakeBack(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopped while it waits %v", stop), func(t *testing.T) {
			pcm := make([]byte, 2*48000*4/10) // 400 ms at 48 kHz, more than the input queues
			for i := 0; i < len(pcm); i += 2 {
				pcm[i], pcm[i+1] = 0xE8, 0x03 // 1000
			}
			stream := &heldDropStream{entered: make(chan struct{}), release: make(chan struct{}), back: 960}
			release := sync.OnceFunc(func() { close(stream.release) })
			t.Cleanup(release)
			out := newOutputOn(t, stream)
			ended := make(chan audio.SpeechEvent, 2)
			defer out.OnSpeech(func(e audio.SpeechEvent) {
				if e.Input == audio.InputVoice && !e.Started {
					ended <- e
				}
			})()
			provider := &mockProvider{audio: pcm, format: audio.AudioFormat{SampleRate: 48000, Channels: 1, Encoding: audio.EncodingPCM16}}
			o := NewOrchestrator(provider, out, OrchestratorOpts{MaxRequestsPerMin: 100})
			defer o.Stop()
			id, err := o.Speak("a long answer", "")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-stream.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("the mixer never reached its second write")
			}
			out.Cancel(audio.InputVoice)
			time.Sleep(100 * time.Millisecond) // the cancelled write reaches its Drain
			if stop {
				o.Stop()
			}
			time.Sleep(100 * time.Millisecond)
			release()
			var end audio.SpeechEvent
			select {
			case end = <-ended:
			case <-time.After(3 * time.Second):
				t.Fatal("the cancelled speech never ended")
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				status, _ := o.Status(id)
				switch {
				case status.Status == StatusCompleted:
					if status.DurationMs != end.Played.Milliseconds() {
						t.Fatalf("the speech completed with %d ms, %d bytes played, but the room heard %v", status.DurationMs, status.BytesWritten, end.Played)
					}
					return
				case status.Status == StatusFailed && stop:
					return
				case status.Status == StatusFailed || time.Now().After(deadline):
					t.Fatalf("status %s (%s), want completed", status.Status, status.Error)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
