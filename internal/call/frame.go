package call

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// Streams of the link's binary frames (C2).
const (
	StreamCallMix     uint8 = 1 // bridge to Pi, 24 kHz
	StreamParticipant uint8 = 2 // bridge to Pi, 16 kHz
	StreamOffice      uint8 = 3 // Pi to bridge, 16 kHz
	StreamWisprVoice  uint8 = 4 // Pi to bridge, 24 kHz
	StreamWisprChime  uint8 = 5 // Pi to bridge, 24 kHz
)

// HeaderSize is the length of a frame's header: byte 0 the stream, 1 the
// participant index, 2-3 the cancel generation, 4-7 the sequence and 8-15 the
// sender's clock, each little-endian. The PCM16 LE mono samples follow it.
const HeaderSize = 16

// Frame is one 20 ms audio frame of the link.
type Frame struct {
	Stream     uint8
	Index      uint8  // participant index; 0 unless StreamParticipant
	Generation uint16 // cancel generation; streams 4 and 5, else 0
	Seq        uint32 // per stream
	ClockUS    int64  // sender's clock, Unix microseconds
	PCM        []int16
}

// ErrBadFrame is wrapped by every refusal of ParseFrame. The receiver drops
// and counts such a frame.
var ErrBadFrame = errors.New("bad frame")

// FrameSamples gives the samples in one 20 ms frame of the stream: 480 at
// 24 kHz, 320 at 16 kHz, and 0 for an unknown stream.
func FrameSamples(stream uint8) int {
	switch stream {
	case StreamCallMix, StreamWisprVoice, StreamWisprChime:
		return 480
	case StreamParticipant, StreamOffice:
		return 320
	}
	return 0
}

// AppendFrame appends f's header and samples to dst and returns the extended
// slice. It allocates nothing when dst has the capacity. It checks nothing:
// the sender gives each stream its own 20 ms of samples.
func AppendFrame(dst []byte, f Frame) []byte {
	start := len(dst)
	dst = slices.Grow(dst, HeaderSize+2*len(f.PCM))[:start+HeaderSize+2*len(f.PCM)]
	b := dst[start:]
	b[0] = f.Stream
	b[1] = f.Index
	binary.LittleEndian.PutUint16(b[2:4], f.Generation)
	binary.LittleEndian.PutUint32(b[4:8], f.Seq)
	binary.LittleEndian.PutUint64(b[8:16], uint64(f.ClockUS))
	for i, s := range f.PCM {
		binary.LittleEndian.PutUint16(b[HeaderSize+2*i:], uint16(s))
	}
	return dst
}

// ParseFrame reads one frame from b into f, and writes the samples into
// f.PCM's array when it has the capacity. It refuses, with an error that
// wraps ErrBadFrame, a frame shorter than the header, an odd length, an
// unknown stream, and a PCM length other than that stream's 20 ms.
func ParseFrame(b []byte, f *Frame) error {
	if len(b) < HeaderSize {
		return fmt.Errorf("%w: %d bytes, shorter than the %d-byte header", ErrBadFrame, len(b), HeaderSize)
	}
	if len(b)%2 != 0 {
		return fmt.Errorf("%w: an odd length, %d bytes", ErrBadFrame, len(b))
	}
	n := FrameSamples(b[0])
	if n == 0 {
		return fmt.Errorf("%w: unknown stream %d", ErrBadFrame, b[0])
	}
	if got := (len(b) - HeaderSize) / 2; got != n {
		return fmt.Errorf("%w: stream %d frame of %d samples, not %d", ErrBadFrame, b[0], got, n)
	}
	f.Stream = b[0]
	f.Index = b[1]
	f.Generation = binary.LittleEndian.Uint16(b[2:4])
	f.Seq = binary.LittleEndian.Uint32(b[4:8])
	f.ClockUS = int64(binary.LittleEndian.Uint64(b[8:16]))
	f.PCM = slices.Grow(f.PCM[:0], n)[:n]
	for i := range f.PCM {
		f.PCM[i] = int16(binary.LittleEndian.Uint16(b[HeaderSize+2*i:]))
	}
	return nil
}
