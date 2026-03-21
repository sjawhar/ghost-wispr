package audio

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/gordonklaus/portaudio"
)

// Mic wraps PortAudio with a configurable buffer size.
type Mic struct {
	stream          *portaudio.Stream
	buf             []int16
	sampleRate      int
	framesPerBuffer int
}

// NewMic opens a PortAudio capture stream with the given sample rate and buffer size (in frames).
func NewMic(sampleRate, framesPerBuffer int) (*Mic, error) {
	buf := make([]int16, framesPerBuffer)
	stream, err := portaudio.OpenDefaultStream(1, 0, float64(sampleRate), framesPerBuffer, buf)
	if err != nil {
		return nil, err
	}
	return &Mic{stream: stream, buf: buf, sampleRate: sampleRate, framesPerBuffer: framesPerBuffer}, nil
}

func (m *Mic) Start() error { return m.stream.Start() }
func (m *Mic) Stop() error  { return m.stream.Stop() }

// Reopen closes the current PortAudio stream and opens a fresh one with the
// same parameters. This picks up new device nodes after a USB reconnect.
func (m *Mic) Reopen() error {
	_ = m.stream.Stop()
	_ = m.stream.Close()
	stream, err := portaudio.OpenDefaultStream(1, 0, float64(m.sampleRate), m.framesPerBuffer, m.buf)
	if err != nil {
		return err
	}
	m.stream = stream
	return m.stream.Start()
}

// Stream reads from the mic and writes PCM16-LE to w until an error or stop.
func (m *Mic) Stream(w io.Writer) error {
	var out bytes.Buffer
	out.Grow(len(m.buf) * 2) // pre-allocate: int16 = 2 bytes per sample
	for {
		if err := m.stream.Read(); err != nil {
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
