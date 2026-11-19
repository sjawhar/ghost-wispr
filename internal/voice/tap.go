package voice

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TapRate is the rate of every frame the tap delivers: the Pi's capture rate.
const TapRate = 16000

// tapRing is how much audio the tap keeps for the start of an exchange: the
// wake phrase is spoken before its transcript arrives.
const tapRing = 2 * time.Second

// tapQueue is how many frames an exchange may leave unread before the tap
// drops: 10 s of 250 ms microphone buffers.
const tapQueue = 40

// Tap is one more writer behind the capture gate: room audio for the voice of
// an open exchange, 16 kHz mono PCM16, one frame per microphone buffer. It
// never blocks and never fails the gate's write: when the exchange reads too
// slowly it drops and counts, as transcribe.AsyncSender does. Behind the gate
// it passes nothing while the gate is closed; Reset empties its ring on every
// closing, so no audio from before a closing reaches an exchange after it.
type Tap struct {
	mu   sync.Mutex
	rate int // the microphone's rate
	// ring holds the last tapRing of audio at TapRate; n samples of it are
	// valid, the newest ending at pos.
	ring []int16
	pos  int
	n    int
	// phase carries a resampler's position across writes at another rate.
	phase float64
	last  int16
	out   chan audio.Frame // nil while no exchange has the tap open

	dropped atomic.Uint64
}

// NewTap returns a tap for a microphone at TapRate until SetRate says
// otherwise.
func NewTap() *Tap {
	return &Tap{rate: TapRate, ring: make([]int16, int(tapRing/time.Millisecond)*TapRate/1000)}
}

// SetRate sets the rate the microphone writes at; the tap resamples it to
// TapRate.
func (t *Tap) SetRate(rate int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rate > 0 && rate != t.rate {
		t.rate, t.phase = rate, 0
		t.clearLocked()
	}
}

// Write takes one microphone buffer of PCM16 LE mono. It always reports the
// whole buffer written.
func (t *Tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := len(p) / 2
	if samples == 0 {
		return len(p), nil
	}
	var frame []int16
	if t.out != nil {
		frame = make([]int16, 0, samples*TapRate/t.rate+1)
	}
	if t.rate == TapRate {
		for i := range samples {
			s := int16(binary.LittleEndian.Uint16(p[2*i:]))
			t.pushLocked(s)
			if frame != nil {
				frame = append(frame, s)
			}
		}
	} else {
		// Linear interpolation from the microphone's rate to TapRate.
		step := float64(t.rate) / TapRate
		for i := range samples {
			cur := int16(binary.LittleEndian.Uint16(p[2*i:]))
			for t.phase < 1 {
				s := int16(float64(t.last) + (float64(cur)-float64(t.last))*t.phase)
				t.pushLocked(s)
				if frame != nil {
					frame = append(frame, s)
				}
				t.phase += step
			}
			t.phase--
			t.last = cur
		}
	}
	if frame != nil {
		t.sendLocked(audio.Frame{PCM: frame, Rate: TapRate, Channels: 1})
	}
	return len(p), nil
}

func (t *Tap) pushLocked(s int16) {
	t.ring[t.pos] = s
	t.pos = (t.pos + 1) % len(t.ring)
	if t.n < len(t.ring) {
		t.n++
	}
}

func (t *Tap) sendLocked(f audio.Frame) {
	select {
	case t.out <- f:
	default:
		t.dropped.Add(1)
	}
}

// Open starts delivering room audio to a new exchange: first the last replay
// of the ring (at most tapRing), then each buffer as it is written. A frame
// stream already open is closed first.
func (t *Tap) Open(replay time.Duration) <-chan audio.Frame {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeOutLocked()
	out := make(chan audio.Frame, tapQueue) // the replay is at most 8 frames of it
	t.out = out
	want := min(t.n, int(min(replay, tapRing)/time.Millisecond)*TapRate/1000)
	if want > 0 {
		pcm := make([]int16, want)
		start := (t.pos - want + len(t.ring)) % len(t.ring)
		for i := range want {
			pcm[i] = t.ring[(start+i)%len(t.ring)]
		}
		// In buffers of at most 250 ms, as the microphone delivers.
		const chunk = TapRate / 4
		for off := 0; off < len(pcm); off += chunk {
			t.sendLocked(audio.Frame{PCM: pcm[off:min(off+chunk, len(pcm))], Rate: TapRate, Channels: 1})
		}
	}
	return out
}

// CloseOut ends the open exchange's stream, dropping what it has not read.
func (t *Tap) CloseOut() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeOutLocked()
}

func (t *Tap) closeOutLocked() {
	if t.out == nil {
		return
	}
	for len(t.out) > 0 {
		<-t.out
	}
	close(t.out)
	t.out = nil
}

// Reset empties the ring, at every gate closing: audio from before it is never
// replayed.
func (t *Tap) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearLocked()
}

func (t *Tap) clearLocked() {
	clear(t.ring)
	t.pos, t.n, t.last = 0, 0, 0
}

// Dropped counts the frames dropped because an exchange read too slowly.
func (t *Tap) Dropped() uint64 { return t.dropped.Load() }
