package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/gordonklaus/portaudio"
)

// Mic wraps PortAudio with a configurable buffer size.
type Mic struct {
	stream          *portaudio.Stream
	buf             []int16
	sampleRate      int
	framesPerBuffer int
	active          atomic.Bool
}

// NewMic opens a PortAudio capture stream with the given sample rate and buffer size (in frames).
func NewMic(sampleRate, framesPerBuffer int) (*Mic, error) {
	buf := make([]int16, framesPerBuffer)
	stream, err := openInputStream(sampleRate, framesPerBuffer, buf)
	if err != nil {
		return nil, err
	}
	return &Mic{stream: stream, buf: buf, sampleRate: sampleRate, framesPerBuffer: framesPerBuffer}, nil
}

func (m *Mic) Start() error {
	err := m.stream.Start()
	if err == nil {
		m.active.Store(true)
	}
	return err
}
func (m *Mic) Stop() error {
	m.active.Store(false)
	return m.stream.Stop()
}

// Reopen closes the current PortAudio stream and opens a fresh one with the
// same parameters. This picks up new device nodes after a USB reconnect.
func (m *Mic) Reopen() error {
	_ = m.stream.Stop()
	_ = m.stream.Close()
	stream, err := openInputStream(m.sampleRate, m.framesPerBuffer, m.buf)
	if err != nil {
		return err
	}
	m.stream = stream
	err = m.stream.Start()
	if err == nil {
		m.active.Store(true)
	}
	return err
}

// Stream reads from the mic and writes PCM16-LE to w until an error or stop.
func (m *Mic) Stream(w io.Writer) error {
	var out bytes.Buffer
	out.Grow(len(m.buf) * 2) // pre-allocate: int16 = 2 bytes per sample
	for {
		if err := m.stream.Read(); err != nil {
			m.active.Store(false)
			return err
		}
		out.Reset()
		if err := binary.Write(&out, binary.LittleEndian, m.buf); err != nil {
			return err
		}
		if _, err := w.Write(out.Bytes()); err != nil {
			return err
		}
	}
}

// IsOpen returns true if the mic stream is currently active.
func (m *Mic) IsOpen() bool {
	return m.stream != nil && m.active.Load()
}

func openInputStream(sampleRate, framesPerBuffer int, buf []int16) (*portaudio.Stream, error) {
	stream, err := portaudio.OpenDefaultStream(1, 0, float64(sampleRate), framesPerBuffer, buf)
	if err == nil {
		return stream, nil
	}

	devices, devErr := portaudio.Devices()
	if devErr != nil {
		return nil, err
	}

	var firstErr error
	for _, dev := range devices {
		if dev == nil || dev.MaxInputChannels < 1 {
			continue
		}
		params := portaudio.HighLatencyParameters(dev, nil)
		params.Input.Channels = 1
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
		return nil, fmt.Errorf("default input failed: %w; fallback device open failed: %w", err, firstErr)
	}
	return nil, err
}
