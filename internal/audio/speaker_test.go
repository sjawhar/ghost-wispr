package audio

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mockOutputStream implements outputStream for testing without PortAudio hardware.
type mockOutputStream struct {
	mu         sync.Mutex
	started    bool
	stopped    bool
	closed     bool
	writeCount int
	writeErr   error
	writtenBuf [][]int16 // snapshots of buffer at each Write()
	buf        []int16   // reference to the shared buffer
}

func (m *mockOutputStream) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	return nil
}

func (m *mockOutputStream) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	return nil
}

func (m *mockOutputStream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockOutputStream) Write() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeCount++
	if m.buf != nil {
		snapshot := make([]int16, len(m.buf))
		copy(snapshot, m.buf)
		m.writtenBuf = append(m.writtenBuf, snapshot)
	}
	return m.writeErr
}

func newMockStreamFactory(mock *mockOutputStream) func(int, int, int, []int16, string) (outputStream, error) {
	return func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error) {
		mock.buf = buf
		return mock, nil
	}
}

func newFailingStreamFactory(err error) func(int, int, int, []int16, string) (outputStream, error) {
	return func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error) {
		return nil, err
	}
}

// gatedOutputStream wraps a mockOutputStream, blocking on Write() until a gate channel is closed.
// This allows tests to synchronize with the write loop for reliable Stop() testing.
type gatedOutputStream struct {
	mock         *mockOutputStream
	writeStarted chan struct{}
	writeGate    chan struct{}
	firstWrite   *bool
}

func (g *gatedOutputStream) Start() error { return g.mock.Start() }
func (g *gatedOutputStream) Stop() error  { return g.mock.Stop() }
func (g *gatedOutputStream) Close() error { return g.mock.Close() }

func (g *gatedOutputStream) Write() error {
	if *g.firstWrite {
		*g.firstWrite = false
		close(g.writeStarted)
		<-g.writeGate
	}
	return g.mock.Write()
}

// makePCM16Bytes creates PCM16 little-endian bytes from int16 samples.
func makePCM16Bytes(samples []int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf
}

func TestAudioEncodingString(t *testing.T) {
	tests := []struct {
		enc  AudioEncoding
		want string
	}{
		{EncodingPCM16, "pcm16"},
		{EncodingMP3, "mp3"},
		{AudioEncoding(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.enc.String(); got != tt.want {
			t.Errorf("AudioEncoding(%d).String() = %q, want %q", tt.enc, got, tt.want)
		}
	}
}

func TestSpeakerPlayPCM16(t *testing.T) {
	mock := &mockOutputStream{}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	// 100 mono samples at 1000 Hz → 100ms of audio
	samples := make([]int16, 100)
	for i := range samples {
		samples[i] = int16(i * 100)
	}
	pcmBytes := makePCM16Bytes(samples)

	result, err := speaker.Play(pcmBytes, AudioFormat{
		SampleRate: 1000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}

	if result.BytesWritten != int64(len(pcmBytes)) {
		t.Errorf("BytesWritten = %d, want %d", result.BytesWritten, len(pcmBytes))
	}
	if result.DurationMs != 100 {
		t.Errorf("DurationMs = %d, want 100", result.DurationMs)
	}

	if !mock.started {
		t.Error("stream was not started")
	}
	if !mock.stopped {
		t.Error("stream was not stopped")
	}
	if !mock.closed {
		t.Error("stream was not closed")
	}
	if mock.writeCount == 0 {
		t.Error("stream.Write() was never called")
	}
}

func TestSpeakerPlayStereo(t *testing.T) {
	mock := &mockOutputStream{}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	// 200 stereo samples (100 frames) at 1000 Hz → 100ms
	samples := make([]int16, 200)
	for i := range samples {
		samples[i] = int16(i)
	}
	pcmBytes := makePCM16Bytes(samples)

	result, err := speaker.Play(pcmBytes, AudioFormat{
		SampleRate: 1000,
		Channels:   2,
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}

	if result.DurationMs != 100 {
		t.Errorf("DurationMs = %d, want 100", result.DurationMs)
	}
}

func TestSpeakerPlayEmptyAudio(t *testing.T) {
	mock := &mockOutputStream{}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	result, err := speaker.Play([]byte{}, AudioFormat{
		SampleRate: 16000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}
	if result.BytesWritten != 0 {
		t.Errorf("BytesWritten = %d, want 0", result.BytesWritten)
	}
	if result.DurationMs != 0 {
		t.Errorf("DurationMs = %d, want 0", result.DurationMs)
	}
	// Stream should not have been opened for empty audio.
	if mock.started {
		t.Error("stream should not be started for empty audio")
	}
}

func TestSpeakerPlayOddByteCount(t *testing.T) {
	mock := &mockOutputStream{}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	// 5 bytes → trimmed to 4 bytes → 2 samples
	result, err := speaker.Play([]byte{0x01, 0x00, 0x02, 0x00, 0xFF}, AudioFormat{
		SampleRate: 1000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}
	if result.BytesWritten != 4 {
		t.Errorf("BytesWritten = %d, want 4", result.BytesWritten)
	}
}

func TestSpeakerPlayUnsupportedEncoding(t *testing.T) {
	speaker := NewSpeaker("")
	_, err := speaker.Play([]byte{1, 2}, AudioFormat{
		SampleRate: 16000,
		Channels:   1,
		Encoding:   AudioEncoding(99),
	})
	if err == nil {
		t.Fatal("expected error for unsupported encoding")
	}
}

func TestSpeakerPlayStreamOpenError(t *testing.T) {
	speaker := NewSpeaker("")
	speaker.openStream = newFailingStreamFactory(fmt.Errorf("no output device"))

	samples := makePCM16Bytes([]int16{100, 200})
	_, err := speaker.Play(samples, AudioFormat{
		SampleRate: 16000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err == nil {
		t.Fatal("expected error when stream open fails")
	}
}

func TestSpeakerPlayStreamWriteError(t *testing.T) {
	mock := &mockOutputStream{writeErr: fmt.Errorf("device disconnected")}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	samples := makePCM16Bytes([]int16{100, 200, 300, 400})
	_, err := speaker.Play(samples, AudioFormat{
		SampleRate: 16000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err == nil {
		t.Fatal("expected error when stream write fails")
	}
}

func TestSpeakerStop(t *testing.T) {
	speaker := NewSpeaker("")

	// Create a large audio buffer so Play takes many Write() calls.
	largeSamples := make([]int16, 100000)
	for i := range largeSamples {
		largeSamples[i] = int16(i % 32000)
	}
	pcmBytes := makePCM16Bytes(largeSamples)

	// Use a blocking mock: first Write() signals readiness, then blocks until released.
	writeStarted := make(chan struct{})
	writeGate := make(chan struct{})
	firstWrite := true
	blockingMock := &mockOutputStream{}
	speaker.openStream = func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error) {
		blockingMock.buf = buf
		blockingMock.writeErr = nil
		return &gatedOutputStream{
			mock:         blockingMock,
			writeStarted: writeStarted,
			writeGate:    writeGate,
			firstWrite:   &firstWrite,
		}, nil
	}

	var wg sync.WaitGroup
	var playResult *PlaybackResult
	var playErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		playResult, playErr = speaker.Play(pcmBytes, AudioFormat{
			SampleRate: 16000,
			Channels:   1,
			Encoding:   EncodingPCM16,
		})
	}()

	// Wait for the first Write() to start, proving Play is in the write loop.
	<-writeStarted
	// Now stop playback.
	if err := speaker.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	// Release the blocked Write() so Play can check ctx.Done().
	close(writeGate)

	wg.Wait()

	if playErr != nil {
		t.Fatalf("Play returned error after Stop: %v", playErr)
	}
	if playResult == nil {
		t.Fatal("Play returned nil result after Stop")
	}
	// Should have written less than the full audio.
	if playResult.BytesWritten >= int64(len(pcmBytes)) {
		t.Errorf("expected partial playback, got BytesWritten=%d (full=%d)", playResult.BytesWritten, len(pcmBytes))
	}
}

func TestSpeakerIsPlaying(t *testing.T) {
	speaker := NewSpeaker("")

	if speaker.IsPlaying() {
		t.Error("expected IsPlaying=false before Play")
	}

	mock := &mockOutputStream{}
	speaker.openStream = newMockStreamFactory(mock)

	samples := makePCM16Bytes([]int16{1, 2, 3, 4})
	_, err := speaker.Play(samples, AudioFormat{
		SampleRate: 16000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}

	if speaker.IsPlaying() {
		t.Error("expected IsPlaying=false after Play completes")
	}
}

func TestSpeakerClose(t *testing.T) {
	speaker := NewSpeaker("")

	if err := speaker.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	_, err := speaker.Play([]byte{1, 2}, AudioFormat{
		SampleRate: 16000,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestSpeakerCloseStopsPlayback(t *testing.T) {
	speaker := NewSpeaker("")

	largeSamples := make([]int16, 100000)
	pcmBytes := makePCM16Bytes(largeSamples)

	mock := &mockOutputStream{}
	speaker.openStream = newMockStreamFactory(mock)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = speaker.Play(pcmBytes, AudioFormat{
			SampleRate: 16000,
			Channels:   1,
			Encoding:   EncodingPCM16,
		})
	}()

	time.Sleep(10 * time.Millisecond)
	if err := speaker.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	wg.Wait()
	// Play should have returned (either with result or error).
}

func TestSpeakerPlayDefaultsForInvalidFormat(t *testing.T) {
	mock := &mockOutputStream{}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	samples := makePCM16Bytes([]int16{100, 200})
	result, err := speaker.Play(samples, AudioFormat{
		SampleRate: 0, // should default to 16000
		Channels:   0, // should default to 1
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestSpeakerPlayInvalidMP3(t *testing.T) {
	speaker := NewSpeaker("")
	_, err := speaker.Play([]byte{0xFF, 0xFB, 0x90, 0x00}, AudioFormat{
		SampleRate: 44100,
		Channels:   2,
		Encoding:   EncodingMP3,
	})
	// go-mp3 should fail on truncated/invalid MP3 data.
	if err == nil {
		t.Fatal("expected error for invalid MP3 data")
	}
}

func TestSpeakerStopWhenNotPlaying(t *testing.T) {
	speaker := NewSpeaker("")
	// Stop when not playing should be a no-op.
	if err := speaker.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

func TestSpeakerMultipleStopCalls(t *testing.T) {
	speaker := NewSpeaker("")
	// Multiple Stop calls should not panic.
	_ = speaker.Stop()
	_ = speaker.Stop()
	_ = speaker.Stop()
}

func TestSamplesToMs(t *testing.T) {
	tests := []struct {
		name         string
		totalSamples int64
		channels     int
		sampleRate   int
		want         int64
	}{
		{"mono 1s", 16000, 1, 16000, 1000},
		{"stereo 1s", 88200, 2, 44100, 1000},
		{"mono 500ms", 8000, 1, 16000, 500},
		{"zero channels", 100, 0, 16000, 0},
		{"zero sample rate", 100, 1, 0, 0},
		{"zero samples", 0, 1, 16000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := samplesToMs(tt.totalSamples, tt.channels, tt.sampleRate)
			if got != tt.want {
				t.Errorf("samplesToMs(%d, %d, %d) = %d, want %d",
					tt.totalSamples, tt.channels, tt.sampleRate, got, tt.want)
			}
		})
	}
}

func TestSpeakerWritesCorrectSamples(t *testing.T) {
	mock := &mockOutputStream{}
	speaker := NewSpeaker("")
	speaker.openStream = newMockStreamFactory(mock)

	// 4 mono samples at sampleRate=4 → framesPerBuffer=1, outputBuf size=1
	// This means 4 Write() calls, each with 1 sample.
	samples := []int16{100, 200, 300, 400}
	pcmBytes := makePCM16Bytes(samples)

	_, err := speaker.Play(pcmBytes, AudioFormat{
		SampleRate: 4,
		Channels:   1,
		Encoding:   EncodingPCM16,
	})
	if err != nil {
		t.Fatalf("Play failed: %v", err)
	}

	if mock.writeCount != 4 {
		t.Fatalf("expected 4 Write() calls, got %d", mock.writeCount)
	}

	// Verify each written chunk contains the correct sample.
	for i, buf := range mock.writtenBuf {
		if len(buf) == 0 {
			t.Fatalf("chunk %d is empty", i)
		}
		if buf[0] != samples[i] {
			t.Errorf("chunk %d: got sample %d, want %d", i, buf[0], samples[i])
		}
	}
}
