package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/gordonklaus/portaudio"
	"github.com/hajimehoshi/go-mp3"
)

// outputStream abstracts a PortAudio output stream for testability.
type outputStream interface {
	Start() error
	Stop() error
	Write() error
	Close() error
}

// Speaker plays PCM audio through a PortAudio output device.
// It is completely isolated from the Mic — no shared streams or buffers.
type Speaker struct {
	mu         sync.Mutex
	deviceName string
	playing    atomic.Bool
	closed     atomic.Bool

	cancelMu sync.Mutex
	cancel   context.CancelFunc

	// openStream is the factory for creating output streams.
	// Override in tests to avoid PortAudio hardware dependency.
	openStream func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error)
}

// NewSpeaker creates a new Speaker. deviceName is optional — if empty,
// the system default output device is used.
func NewSpeaker(deviceName string) *Speaker {
	s := &Speaker{
		deviceName: deviceName,
	}
	s.openStream = s.defaultOpenStream
	return s
}

// Play sends audio bytes to the speaker. If format.Encoding is EncodingMP3,
// the audio is decoded to PCM16 before playback. This method blocks until
// playback completes or Stop() is called.
func (s *Speaker) Play(audioData []byte, format AudioFormat) (result *PlaybackResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("speaker panic recovered", "panic", r)
			err = fmt.Errorf("speaker panic: %v", r)
			s.playing.Store(false)
		}
	}()

	if s.closed.Load() {
		return nil, fmt.Errorf("speaker is closed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.playing.Load() {
		return nil, fmt.Errorf("playback already in progress")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.cancelMu.Lock()
	s.cancel = cancel
	s.cancelMu.Unlock()

	s.playing.Store(true)
	defer s.playing.Store(false)

	// Decode MP3 to PCM if needed.
	var pcmData []byte
	sampleRate := format.SampleRate
	channels := format.Channels

	switch format.Encoding {
	case EncodingMP3:
		var decErr error
		pcmData, sampleRate, channels, decErr = decodeMP3(audioData)
		if decErr != nil {
			return nil, fmt.Errorf("decode mp3: %w", decErr)
		}
	case EncodingPCM16:
		pcmData = audioData
	default:
		return nil, fmt.Errorf("unsupported audio encoding: %v", format.Encoding)
	}

	if channels <= 0 {
		channels = 1
	}
	if sampleRate <= 0 {
		sampleRate = 16000
	}

	// Trim to even byte count for int16 alignment.
	if len(pcmData)%2 != 0 {
		pcmData = pcmData[:len(pcmData)-1]
	}

	// Convert PCM bytes to int16 samples.
	samples := make([]int16, len(pcmData)/2)
	if len(samples) > 0 {
		if err := binary.Read(bytes.NewReader(pcmData), binary.LittleEndian, samples); err != nil {
			return nil, fmt.Errorf("decode pcm samples: %w", err)
		}
	}

	if len(samples) == 0 {
		return &PlaybackResult{}, nil
	}

	// Open PortAudio output stream with sample rate fallback.
	framesPerBuffer := sampleRate / 4 // 250ms buffer
	outputBuf := make([]int16, framesPerBuffer*channels)

	stream, actualRate, err := s.openStreamWithFallback(sampleRate, channels, framesPerBuffer, outputBuf)
	if err != nil {
		return nil, fmt.Errorf("open output stream: %w", err)
	}
	defer stream.Close()

	// Resample PCM if the actual stream rate differs from the source rate.
	if actualRate != sampleRate {
		samples = resamplePCM(samples, channels, sampleRate, actualRate)
		sampleRate = actualRate
		framesPerBuffer = sampleRate / 4
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return nil, fmt.Errorf("start output stream: %w", err)
	}
	defer stream.Stop()

	// Write samples in chunks.
	var bytesWritten int64
	for offset := 0; offset < len(samples); offset += len(outputBuf) {
		select {
		case <-ctx.Done():
			return &PlaybackResult{
				BytesWritten: bytesWritten,
				DurationMs:   samplesToMs(bytesWritten/2, channels, sampleRate),
			}, nil
		default:
		}

		remaining := len(samples) - offset
		chunkSize := len(outputBuf)
		if remaining < chunkSize {
			chunkSize = remaining
		}
		copy(outputBuf[:chunkSize], samples[offset:offset+chunkSize])
		// Zero-fill the remainder for the last chunk.
		for i := chunkSize; i < len(outputBuf); i++ {
			outputBuf[i] = 0
		}

		if err := stream.Write(); err != nil {
			return nil, fmt.Errorf("write to output stream: %w", err)
		}
		bytesWritten += int64(chunkSize) * 2 // int16 = 2 bytes
	}

	return &PlaybackResult{
		BytesWritten: bytesWritten,
		DurationMs:   samplesToMs(int64(len(samples)), channels, sampleRate),
	}, nil
}

// Stop interrupts any in-progress playback.
func (s *Speaker) Stop() error {
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()

	if cancel != nil {
		cancel()
	}
	return nil
}

// IsPlaying returns true if audio is currently being played.
func (s *Speaker) IsPlaying() bool {
	return s.playing.Load()
}

// Close releases speaker resources. After Close, Play returns an error.
func (s *Speaker) Close() error {
	s.closed.Store(true)
	return s.Stop()
}

// OutputStream is the exported version of outputStream for cross-package testing.
type OutputStream interface {
	Start() error
	Stop() error
	Write() error
	Close() error
}

// SetOpenStreamForTest overrides the stream factory for testing.
// It allows callers outside the audio package to inject a mock stream.
func (s *Speaker) SetOpenStreamForTest(fn func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (OutputStream, error)) {
	s.openStream = func(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error) {
		return fn(sampleRate, channels, framesPerBuffer, buf, deviceName)
	}
}

func samplesToMs(totalSamples int64, channels, sampleRate int) int64 {
	if channels <= 0 || sampleRate <= 0 {
		return 0
	}
	frames := totalSamples / int64(channels)
	return frames * 1000 / int64(sampleRate)
}

func (s *Speaker) defaultOpenStream(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (outputStream, error) {
	return openOutputStream(sampleRate, channels, framesPerBuffer, buf, deviceName)
}

// openStreamWithFallback tries the source sample rate first, then falls back to
// common rates the hardware might support (same approach as mic.go).
func (s *Speaker) openStreamWithFallback(srcRate, channels, framesPerBuffer int, buf []int16, rates ...int) (outputStream, int, error) {
	// Try source rate first.
	stream, err := s.openStream(srcRate, channels, framesPerBuffer, buf, s.deviceName)
	if err == nil {
		return stream, srcRate, nil
	}
	slog.Info("speaker: source rate failed, trying fallbacks", "source_rate", srcRate, "error", err)

	// Fallback rates to try.
	fallbackRates := []int{32000, 48000, 44100, 24000, 22050, 16000}
	for _, rate := range fallbackRates {
		if rate == srcRate {
			continue
		}
		fb := rate / 4
		fbBuf := make([]int16, fb*channels)
		stream, tryErr := s.openStream(rate, channels, fb, fbBuf, s.deviceName)
		if tryErr == nil {
			slog.Info("speaker: opened at fallback rate", "rate", rate)
			return stream, rate, nil
		}
	}
	return nil, 0, fmt.Errorf("no supported output sample rate (tried %d and fallbacks): %w", srcRate, err)
}

// resamplePCM does simple linear interpolation resampling of int16 PCM.
func resamplePCM(samples []int16, channels, srcRate, dstRate int) []int16 {
	if srcRate == dstRate || len(samples) == 0 {
		return samples
	}
	srcFrames := len(samples) / channels
	dstFrames := int(float64(srcFrames) * float64(dstRate) / float64(srcRate))
	out := make([]int16, dstFrames*channels)
	ratio := float64(srcRate) / float64(dstRate)

	for i := 0; i < dstFrames; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)
		for ch := 0; ch < channels; ch++ {
			s0 := int(samples[idx*channels+ch])
			s1 := s0
			if idx+1 < srcFrames {
				s1 = int(samples[(idx+1)*channels+ch])
			}
			out[i*channels+ch] = int16(float64(s0) + frac*float64(s1-s0))
		}
	}
	return out
}

func openOutputStream(sampleRate, channels, framesPerBuffer int, buf []int16, deviceName string) (*portaudio.Stream, error) {
	// Try default output device first.
	stream, err := portaudio.OpenDefaultStream(0, channels, float64(sampleRate), framesPerBuffer, buf)
	if err == nil {
		return stream, nil
	}

	// Fallback: enumerate devices and try ones with sufficient output channels.
	devices, devErr := portaudio.Devices()
	if devErr != nil {
		return nil, err
	}

	var firstErr error
	for _, dev := range devices {
		if dev == nil || dev.MaxOutputChannels < channels {
			continue
		}
		// If a specific device name was requested, only try matching devices.
		if deviceName != "" && dev.Name != deviceName {
			continue
		}

		params := portaudio.HighLatencyParameters(nil, dev)
		params.Output.Channels = channels
		params.SampleRate = float64(sampleRate)
		params.FramesPerBuffer = framesPerBuffer

		candidate, openErr := portaudio.OpenStream(params, buf)
		if openErr == nil {
			return candidate, nil
		}
		if firstErr == nil {
			firstErr = openErr
		}
	}

	if firstErr != nil {
		return nil, fmt.Errorf("default output failed: %w; fallback device open failed: %w", err, firstErr)
	}
	return nil, err
}

// decodeMP3 decodes MP3 audio data to PCM16 little-endian samples.
// go-mp3 always outputs stereo (2 channels), signed 16-bit, little-endian.
func decodeMP3(data []byte) (pcmData []byte, sampleRate int, channels int, err error) {
	decoder, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("create mp3 decoder: %w", err)
	}

	sampleRate = decoder.SampleRate()
	channels = 2 // go-mp3 always outputs stereo

	pcmData, err = io.ReadAll(decoder)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read decoded mp3 data: %w", err)
	}

	return pcmData, sampleRate, channels, nil
}
