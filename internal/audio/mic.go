package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gordonklaus/portaudio"
)

// micReadPoll is how long Stream waits before it looks again when the capture
// stream has no frame for it.
const micReadPoll = 10 * time.Millisecond

// micCloseWait is how long Close waits for a Stream under way to return
// before it closes the capture stream. Stream returns within micReadPoll of
// the Close unless a write of its audio, or a PortAudio call, does not return.
var micCloseWait = 2 * time.Second

// ErrMicStillReading is what Close returns when the Stream under way did not
// return within micCloseWait: the capture stream, and the input device with
// it, stay open. A Close once that Stream has returned closes them.
var ErrMicStillReading = errors.New("microphone still reading: its capture stream stays open")

// errMicClosed is what Stream returns when Close ends it.
var errMicClosed = errors.New("microphone closed")

// inputStream is the part of a PortAudio capture stream a Mic uses.
type inputStream interface {
	Start() error
	Stop() error
	Close() error
	AvailableToRead() (int, error)
	Read() error
}

// openInputStreamFn opens a capture stream that reads into *window: each Read
// reads len(*window) frames into it.
var openInputStreamFn = openInputStream

// Mic is a PortAudio capture stream that delivers buffers of framesPerBuffer
// frames. Stream never waits inside PortAudio for frames to come: it reads
// only the frames the stream already holds, and looks again every micReadPoll.
// So Close, from any goroutine, ends a Stream under way even on a device that
// stopped delivering frames, where a PortAudio read never returns and no
// PortAudio call from another thread can end it: after Pa_AbortStream, ALSA's
// blocking read polls an error, takes it for an overrun and polls again
// (PortAudio 19.6, pa_linux_alsa.c, ReadStream and PaAlsaStream_WaitForFrames),
// and Pa_CloseStream frees the stream the read still uses.
type Mic struct {
	mu       sync.Mutex
	stream   inputStream   // nil while closed
	reading  chan struct{} // closed once the Stream under way returns; nil while none runs
	stopping chan struct{} // closed by Close to end the Stream under way; nil once closed
	// buf is one buffer of framesPerBuffer frames, which Stream fills and
	// writes. window is the part of it the next Read fills: the stream reads
	// through a pointer to window, so each Read reads len(window) frames.
	buf             []int16
	window          []int16
	sampleRate      int
	framesPerBuffer int
	active          atomic.Bool
}

// NewMic opens a PortAudio capture stream with the given sample rate and buffer size (in frames).
func NewMic(sampleRate, framesPerBuffer int) (*Mic, error) {
	m := &Mic{buf: make([]int16, framesPerBuffer), sampleRate: sampleRate, framesPerBuffer: framesPerBuffer}
	stream, err := openInputStreamFn(sampleRate, framesPerBuffer, &m.window)
	if err != nil {
		return nil, err
	}
	m.stream = stream
	return m, nil
}

func (m *Mic) current() inputStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stream
}

func (m *Mic) Start() error {
	if m == nil {
		return fmt.Errorf("microphone stream unavailable")
	}
	stream := m.current()
	if stream == nil {
		m.active.Store(false)
		return fmt.Errorf("microphone stream unavailable")
	}
	err := stream.Start()
	if err == nil {
		m.active.Store(true)
	}
	return err
}

func (m *Mic) Stop() error {
	if m == nil {
		return nil
	}
	m.active.Store(false)
	stream := m.current()
	if stream == nil {
		return nil
	}
	return stream.Stop()
}

// Close stops and closes the capture stream, which releases the input device.
// Start and Stream then fail until Open or Reopen opens a fresh stream. Close
// may be called from any goroutine while Stream runs: it ends that Stream, and
// waits up to micCloseWait for it to return before it closes the stream. If it
// has not returned by then, Close closes nothing and returns
// ErrMicStillReading.
func (m *Mic) Close() error {
	if m == nil {
		return nil
	}
	m.active.Store(false)
	m.mu.Lock()
	for m.reading != nil {
		reading := m.reading
		if m.stopping != nil {
			close(m.stopping)
			m.stopping = nil
		}
		m.mu.Unlock()
		wait := time.NewTimer(micCloseWait)
		select {
		case <-reading:
			wait.Stop()
		case <-wait.C:
			return ErrMicStillReading
		}
		m.mu.Lock()
	}
	stream := m.stream
	m.stream = nil
	m.mu.Unlock()
	if stream == nil {
		return nil
	}
	_ = stream.Stop() // a stream that never started reports it is stopped
	return stream.Close()
}

// Open closes the current capture stream, if any, then opens a fresh one with
// the same parameters and starts it, without re-initializing PortAudio. A
// stream that opens but does not start is closed again, so a failed Open holds
// no input device.
func (m *Mic) Open() error {
	if m == nil {
		return fmt.Errorf("microphone stream unavailable")
	}
	if err := m.Close(); errors.Is(err, ErrMicStillReading) {
		return err
	}
	stream, err := openInputStreamFn(m.sampleRate, m.framesPerBuffer, &m.window)
	if err != nil {
		return err
	}
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		return err
	}
	m.mu.Lock()
	m.stream = stream
	m.mu.Unlock()
	m.active.Store(true)
	return nil
}

// Reopen closes the current PortAudio stream, re-initializes PortAudio and
// opens a fresh stream with the same parameters (Open). The re-initialization
// picks up new device nodes after a USB reconnect.
func (m *Mic) Reopen() error {
	if err := m.Close(); errors.Is(err, ErrMicStillReading) {
		return err
	}
	if err := ReinitPortAudio(); err != nil {
		return fmt.Errorf("reinitialize portaudio: %w", err)
	}
	return m.Open()
}

// Stream reads from the mic and writes each buffer of framesPerBuffer frames,
// as PCM16-LE, to w until an error, or until Close ends it. It reads only the
// frames the capture stream already holds: while it holds none, it waits
// micReadPoll and looks again, so a device that delivers no frame keeps no
// PortAudio call waiting, and a Close ends the Stream within micReadPoll.
func (m *Mic) Stream(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("microphone stream unavailable")
	}
	m.mu.Lock()
	stream := m.stream
	if stream == nil || m.reading != nil {
		m.mu.Unlock()
		if stream == nil {
			m.active.Store(false)
			return fmt.Errorf("microphone stream unavailable")
		}
		return fmt.Errorf("microphone already streaming")
	}
	reading, stopping := make(chan struct{}), make(chan struct{})
	m.reading, m.stopping = reading, stopping
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.reading, m.stopping = nil, nil
		m.mu.Unlock()
		close(reading)
	}()

	out := make([]byte, 2*len(m.buf))
	poll := time.NewTimer(micReadPoll)
	defer poll.Stop()
	filled := 0
	for {
		select {
		case <-stopping:
			return errMicClosed
		default:
		}
		available, err := stream.AvailableToRead()
		if err != nil {
			m.active.Store(false)
			return err
		}
		if available == 0 {
			poll.Reset(micReadPoll)
			select {
			case <-stopping:
				return errMicClosed
			case <-poll.C:
			}
			continue
		}
		n := min(available, len(m.buf)-filled)
		m.window = m.buf[filled : filled+n]
		if err := stream.Read(); err != nil {
			m.active.Store(false)
			return err
		}
		filled += n
		if filled < len(m.buf) {
			continue
		}
		filled = 0
		for i, sample := range m.buf {
			binary.LittleEndian.PutUint16(out[2*i:], uint16(sample))
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
	}
}

// IsOpen returns true if the mic stream is currently active. It reads only the
// atomic flag, so HTTP handlers may call it while the mic loop reopens the
// stream; the flag is set only while a started stream exists.
func (m *Mic) IsOpen() bool {
	return m != nil && m.active.Load()
}

func (m *Mic) SampleRate() int {
	if m == nil {
		return 0
	}
	return m.sampleRate
}

func openInputStream(sampleRate, framesPerBuffer int, window *[]int16) (inputStream, error) {
	stream, err := portaudio.OpenDefaultStream(1, 0, float64(sampleRate), framesPerBuffer, window)
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

		candidate, openErr := portaudio.OpenStream(params, window)
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
