package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gordonklaus/portaudio"
)

// micReadPoll is how long Stream waits before it looks again when the capture
// stream has no frame for it.
const micReadPoll = 10 * time.Millisecond

// micCloseWait is how long Close waits for a PortAudio call Stream is making
// to return before it gives up on closing the capture stream. Stream's calls
// do not wait for frames (it reads only what the stream holds), so they
// return at once unless PortAudio itself hangs.
var micCloseWait = 2 * time.Second

// ErrMicStillReading is what Close returns when a PortAudio call of the
// Stream under way did not return within micCloseWait: the capture stream, and
// the input device with it, stay open, since closing it under the call would
// free memory the call still uses. A Close once the call has returned closes
// them.
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

// openInputStreamFn opens a capture stream on device, the mic_device setting
// (openInputStream), that reads into *window: each Read reads len(*window)
// frames into it.
var openInputStreamFn = openInputStream

// Mic is a PortAudio capture stream that delivers buffers of framesPerBuffer
// frames. Stream never waits inside PortAudio for frames to come: it reads
// only the frames the stream already holds, and looks again every micReadPoll.
// So Close, from any goroutine, ends a Stream under way even on a device that
// stopped delivering frames, where a PortAudio read never returns and no
// PortAudio call from another thread can end it: after Pa_AbortStream, ALSA's
// blocking read polls an error, takes it for an overrun and polls again
// (PortAudio 19.6, pa_linux_alsa.c, ReadStream and PaAlsaStream_WaitForFrames),
// and Pa_CloseStream frees the stream the read still uses. Close closes the
// stream between two of Stream's PortAudio calls, also while Stream waits on
// a write of its audio, so a write that does not return keeps no device open.
type Mic struct {
	mu       sync.Mutex
	stream   inputStream   // nil while closed
	reading  chan struct{} // closed once the Stream under way returns; nil while none runs
	stopping chan struct{} // closed by Close to end the Stream under way; nil once closed
	// pa is held, as a one-slot channel, through each PortAudio call on the
	// stream: by Stream around each AvailableToRead and Read, and by Close
	// around Stop and Close. Stream takes it only while Close has not begun.
	pa chan struct{}
	// buf is one buffer of framesPerBuffer frames, which Stream fills and
	// writes. window is the part of it the next Read fills: the stream reads
	// through a pointer to window, so each Read reads len(window) frames.
	buf             []int16
	window          []int16
	device          string // the mic_device setting: the sound card it opens, or "" for the default input
	sampleRate      int
	framesPerBuffer int
	held            atomic.Bool // a capture stream is open: from the open until Close closes it
}

// NewMic opens a PortAudio capture stream on device, the mic_device setting
// ("" for the default input), with the given sample rate and buffer size (in
// frames).
func NewMic(device string, sampleRate, framesPerBuffer int) (*Mic, error) {
	m := &Mic{buf: make([]int16, framesPerBuffer), device: device, sampleRate: sampleRate, framesPerBuffer: framesPerBuffer}
	stream, err := openInputStreamFn(device, sampleRate, framesPerBuffer, &m.window)
	if err != nil {
		return nil, err
	}
	m.stream = stream
	m.held.Store(true)
	return m, nil
}

func (m *Mic) current() inputStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stream
}

// paLock returns the one-slot channel held through each PortAudio call.
func (m *Mic) paLock() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pa == nil {
		m.pa = make(chan struct{}, 1)
	}
	return m.pa
}

func (m *Mic) Start() error {
	if m == nil {
		return fmt.Errorf("microphone stream unavailable")
	}
	stream := m.current()
	if stream == nil {
		return fmt.Errorf("microphone stream unavailable")
	}
	return stream.Start()
}

// Stop stops the capture stream and keeps it open. Shutdown calls it once no
// Stream runs.
func (m *Mic) Stop() error {
	if m == nil {
		return nil
	}
	stream := m.current()
	if stream == nil {
		return nil
	}
	return stream.Stop()
}

// Close stops and closes the capture stream, which releases the input device.
// Start and Stream then fail until Open or Reopen opens a fresh stream. Close
// may be called from any goroutine while Stream runs: it ends that Stream and
// closes the stream as soon as no PortAudio call of Stream's runs, without
// waiting for Stream to return, which a write of its audio can keep it from.
// If a PortAudio call has not returned within micCloseWait, Close closes
// nothing and returns ErrMicStillReading.
func (m *Mic) Close() error {
	if m == nil {
		return nil
	}
	pa := m.paLock()
	m.mu.Lock()
	if m.stopping != nil {
		close(m.stopping)
		m.stopping = nil
	}
	m.mu.Unlock()
	wait := time.NewTimer(micCloseWait)
	defer wait.Stop()
	select {
	case pa <- struct{}{}:
	case <-wait.C:
		return ErrMicStillReading
	}
	defer func() { <-pa }()
	m.mu.Lock()
	// A Stream that began while Close waited for pa registered a stopping
	// channel of its own, and holds the stream taken below: end it too, or it
	// would go on calling the closed stream.
	if m.stopping != nil {
		close(m.stopping)
		m.stopping = nil
	}
	stream := m.stream
	m.stream = nil
	m.mu.Unlock()
	if stream == nil {
		m.held.Store(false)
		return nil
	}
	// IsOpen stays true until PortAudio has stopped and closed the stream:
	// the light, which follows it, must not go off while the device is open.
	defer m.held.Store(false)
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
	stream, err := openInputStreamFn(m.device, m.sampleRate, m.framesPerBuffer, &m.window)
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
	m.held.Store(true)
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
// PortAudio call waiting. Each PortAudio call holds pa and runs only while
// Close has not begun, so once Close has closed the stream, Stream makes no
// call on it: it returns errMicClosed at its next step, a write of its audio
// included once that returns.
func (m *Mic) Stream(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("microphone stream unavailable")
	}
	pa := m.paLock()
	m.mu.Lock()
	stream := m.stream
	if stream == nil || m.reading != nil {
		m.mu.Unlock()
		if stream == nil {
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

	// read makes one PortAudio step under pa: it reports how many frames it
	// read into the buffer from filled on, and false once Close has begun.
	read := func(filled int) (int, bool, error) {
		select {
		case pa <- struct{}{}:
		case <-stopping:
			return 0, false, nil
		}
		defer func() { <-pa }()
		select {
		case <-stopping:
			return 0, false, nil
		default:
		}
		available, err := stream.AvailableToRead()
		if err != nil || available == 0 {
			return 0, true, err
		}
		n := min(available, len(m.buf)-filled)
		m.window = m.buf[filled : filled+n]
		return n, true, stream.Read()
	}

	out := make([]byte, 2*len(m.buf))
	poll := time.NewTimer(micReadPoll)
	defer poll.Stop()
	filled := 0
	for {
		n, open, err := read(filled)
		switch {
		case !open:
			return errMicClosed
		case err != nil:
			return err
		case n == 0:
			poll.Reset(micReadPoll)
			select {
			case <-stopping:
				return errMicClosed
			case <-poll.C:
			}
			continue
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

// IsOpen reports whether the mic holds an open capture stream, and with it the
// input device: from the open until Close closes it, also after a read error.
// It reads only an atomic flag, so HTTP handlers and the light controller may
// call it while the mic loop reopens the stream.
func (m *Mic) IsOpen() bool {
	return m != nil && m.held.Load()
}

func (m *Mic) SampleRate() int {
	if m == nil {
		return 0
	}
	return m.sampleRate
}

// Each PortAudio call openInputStream makes; tests replace them. A real open
// of an input is a microphone, so a test binary refuses it (realDevice).
var (
	paInputDevices = portaudio.Devices
	paOpenInput    = func(params portaudio.StreamParameters, window *[]int16) (inputStream, error) {
		if err := realDevice("PortAudio input " + inputName(params)); err != nil {
			return nil, err
		}
		return portaudio.OpenStream(params, window)
	}
	paOpenDefaultInput = func(sampleRate, framesPerBuffer int, window *[]int16) (inputStream, error) {
		if err := realDevice("PortAudio's default input"); err != nil {
			return nil, err
		}
		return portaudio.OpenDefaultStream(1, 0, float64(sampleRate), framesPerBuffer, window)
	}
)

// inputName is the name of the input device params open.
func inputName(params portaudio.StreamParameters) string {
	if params.Input.Device == nil {
		return "(none)"
	}
	return params.Input.Device.Name
}

// openInputStream opens a capture stream. With device set, the mic_device
// setting, it opens the input of the sound card device names, and no other
// (openNamedInput). Empty, it opens PortAudio's default input, else the
// first input device that opens.
func openInputStream(device string, sampleRate, framesPerBuffer int, window *[]int16) (inputStream, error) {
	if device != "" {
		return openNamedInput(device, sampleRate, framesPerBuffer, window)
	}
	stream, err := paOpenDefaultInput(sampleRate, framesPerBuffer, window)
	if err == nil {
		return stream, nil
	}

	devices, devErr := paInputDevices()
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

		candidate, openErr := paOpenInput(params, window)
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

// openNamedInput opens the first capture PCM of the sound card device names
// (findCard), through the plug device PortAudio lists for it, "...
// (plughw:N,M)" (UsePlugCapture), at sampleRate, which ALSA's plug layer
// converts from the rate the card takes. It fails when PortAudio lists no plug
// device for the card, as when it lists only the raw "(hw:N,M)", which opens
// only at the card's own rate (32000 Hz on the Jabra Speak2 75), where every
// 16000 Hz assumption downstream would be wrong. It falls back to no other
// input, which could record from a card nobody chose, such as an ALSA
// loopback, with the light red.
func openNamedInput(device string, sampleRate, framesPerBuffer int, window *[]int16) (inputStream, error) {
	card, _, dev, cards, found := findCard(device, "c")
	if !found {
		return nil, fmt.Errorf("mic_device %q: no sound card with a capture device matches it (cards: %s)", device, strings.Join(cards, ", "))
	}
	devices, err := paInputDevices()
	if err != nil {
		return nil, fmt.Errorf("mic_device %q: list PortAudio devices: %w", device, err)
	}
	info := plugInputFor(devices, card, dev)
	if info == nil {
		return nil, fmt.Errorf("mic_device %q: PortAudio lists no plug input (plughw:%s,%s) for card %s, device %s", device, card, dev, card, dev)
	}
	params := portaudio.HighLatencyParameters(info, nil)
	params.Input.Channels = 1
	params.SampleRate = float64(sampleRate)
	params.FramesPerBuffer = framesPerBuffer
	// Stream polls the stream every micReadPoll and takes a buffer of
	// framesPerBuffer frames at a time: let ALSA hold two of them.
	params.Input.Latency = max(params.Input.Latency, 2*time.Duration(framesPerBuffer)*time.Second/time.Duration(sampleRate))
	return paOpenInput(params, window)
}

// plugInputFor returns the PortAudio input device for ALSA card card, device
// dev, through ALSA's plug layer, or nil when PortAudio lists none.
func plugInputFor(devices []*portaudio.DeviceInfo, card, dev string) *portaudio.DeviceInfo {
	for _, info := range devices {
		if info != nil && info.MaxInputChannels > 0 && strings.HasSuffix(info.Name, "(plughw:"+card+","+dev+")") {
			return info
		}
	}
	return nil
}
