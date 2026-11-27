package call

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"testing"
)

// pcmOf returns n samples that run through the int16 extremes, so a codec that
// swaps the bytes of a sample, or reads it unsigned, changes one of them.
func pcmOf(n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = []int16{math.MinInt16, math.MaxInt16, -1, 0, 1, -2}[i%6]
	}
	return pcm
}

// TestFrameHeaderRoundTrips holds C2's header: byte 0 the stream, 1 the
// participant index, 2-3 the cancel generation, 4-7 the sequence and 8-15 the
// sender's clock, each little-endian, then the PCM16 LE samples; and that every
// field at its extreme comes back from ParseFrame unchanged. It fails if a field
// moves, a byte order flips, a sign is lost or a stream's sample count is wrong.
func TestFrameHeaderRoundTrips(t *testing.T) {
	t.Run("the bytes of C2's header", func(t *testing.T) {
		pcm := make([]int16, 320)
		pcm[0], pcm[1] = -2, 0x0102
		got := AppendFrame(nil, Frame{
			Stream: StreamParticipant, Index: 0xAB, Generation: 0x1234,
			Seq: 0x89ABCDEF, ClockUS: 0x0102030405060708, PCM: pcm,
		})
		want := []byte{
			0x02,       // stream
			0xAB,       // participant index
			0x34, 0x12, // generation
			0xEF, 0xCD, 0xAB, 0x89, // sequence
			0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // clock
			0xFE, 0xFF, // sample 0: -2
			0x02, 0x01, // sample 1: 0x0102
		}
		if len(got) != 16+640 {
			t.Fatalf("a 16 kHz frame is %d bytes; want 656 (the header and 640 PCM bytes)", len(got))
		}
		if !bytes.Equal(got[:len(want)], want) {
			t.Errorf("the frame starts % X; want % X", got[:len(want)], want)
		}
		if !bytes.Equal(got[len(want):], make([]byte, len(got)-len(want))) {
			t.Error("the silent samples after the first two are not zero bytes")
		}
	})

	for _, tc := range []struct {
		name  string
		frame Frame
	}{
		{"call mix, lowest", Frame{Stream: StreamCallMix, Seq: 0, ClockUS: math.MinInt64, PCM: pcmOf(480)}},
		{"call mix, highest", Frame{Stream: StreamCallMix, Seq: math.MaxUint32, ClockUS: math.MaxInt64, PCM: pcmOf(480)}},
		{"participant, lowest", Frame{Stream: StreamParticipant, Index: 0, Seq: 0, ClockUS: math.MinInt64, PCM: pcmOf(320)}},
		{"participant, highest", Frame{Stream: StreamParticipant, Index: math.MaxUint8, Seq: math.MaxUint32, ClockUS: math.MaxInt64, PCM: pcmOf(320)}},
		{"office, highest", Frame{Stream: StreamOffice, Seq: math.MaxUint32, ClockUS: math.MaxInt64, PCM: pcmOf(320)}},
		{"Wispr voice, lowest", Frame{Stream: StreamWisprVoice, Generation: 0, Seq: 0, ClockUS: math.MinInt64, PCM: pcmOf(480)}},
		{"Wispr voice, highest", Frame{Stream: StreamWisprVoice, Generation: math.MaxUint16, Seq: math.MaxUint32, ClockUS: math.MaxInt64, PCM: pcmOf(480)}},
		{"Wispr chime, highest", Frame{Stream: StreamWisprChime, Generation: math.MaxUint16, Seq: math.MaxUint32, ClockUS: math.MaxInt64, PCM: pcmOf(480)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := AppendFrame(nil, tc.frame)
			var got Frame
			if err := ParseFrame(b, &got); err != nil {
				t.Fatalf("ParseFrame refused the %d bytes AppendFrame made: %v", len(b), err)
			}
			if got.Stream != tc.frame.Stream || got.Index != tc.frame.Index || got.Generation != tc.frame.Generation ||
				got.Seq != tc.frame.Seq || got.ClockUS != tc.frame.ClockUS {
				t.Errorf("header came back as stream %d index %d generation %d seq %d clock %d; want %d %d %d %d %d",
					got.Stream, got.Index, got.Generation, got.Seq, got.ClockUS,
					tc.frame.Stream, tc.frame.Index, tc.frame.Generation, tc.frame.Seq, tc.frame.ClockUS)
			}
			if !slices.Equal(got.PCM, tc.frame.PCM) {
				t.Errorf("the %d samples did not come back unchanged", len(tc.frame.PCM))
			}
		})
	}
}

// frameBytes is a frame of the given stream byte with pcmBytes bytes after the
// header, built by hand so the refusals below do not lean on AppendFrame.
func frameBytes(stream byte, pcmBytes int) []byte {
	b := make([]byte, HeaderSize+pcmBytes)
	b[0] = stream
	return b
}

// TestParseFrameRefusesAShortOrOddFrame holds ParseFrame's refusals: a frame
// shorter than the header, an odd length, an unknown stream, and a PCM length
// other than the stream's 20 ms (960 bytes at 24 kHz, 640 at 16 kHz). It fails
// if any of those checks is dropped; a 977-byte frame, for one, has 480 whole
// samples and passes a check of the sample count alone.
func TestParseFrameRefusesAShortOrOddFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"one byte short of the header", frameBytes(StreamCallMix, 0)[:15]},
		{"the header alone", frameBytes(StreamCallMix, 0)},
		{"odd: a 24 kHz frame and one byte", frameBytes(StreamCallMix, 961)},
		{"odd: a 16 kHz frame and one byte", frameBytes(StreamOffice, 641)},
		{"stream 0", frameBytes(0, 960)},
		{"stream 6", frameBytes(6, 960)},
		{"stream 255", frameBytes(255, 640)},
		{"24 kHz stream with 16 kHz's length", frameBytes(StreamCallMix, 640)},
		{"Wispr voice with 16 kHz's length", frameBytes(StreamWisprVoice, 640)},
		{"Wispr chime one sample short", frameBytes(StreamWisprChime, 958)},
		{"participant with 24 kHz's length", frameBytes(StreamParticipant, 960)},
		{"office with 24 kHz's length", frameBytes(StreamOffice, 960)},
		{"office one sample long", frameBytes(StreamOffice, 642)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f Frame
			if err := ParseFrame(tc.b, &f); !errors.Is(err, ErrBadFrame) {
				t.Errorf("ParseFrame of %d bytes = %v; want an error that wraps ErrBadFrame", len(tc.b), err)
			}
		})
	}

	// The control: each stream's own 20 ms is taken.
	for _, tc := range []struct {
		stream   byte
		pcmBytes int
	}{{StreamCallMix, 960}, {StreamParticipant, 640}, {StreamOffice, 640}, {StreamWisprVoice, 960}, {StreamWisprChime, 960}} {
		var f Frame
		if err := ParseFrame(frameBytes(tc.stream, tc.pcmBytes), &f); err != nil {
			t.Errorf("ParseFrame refused stream %d with its own %d PCM bytes: %v", tc.stream, tc.pcmBytes, err)
		}
	}
}

// TestAppendFrameAllocatesNothingIntoACapacity holds that a sender that keeps
// one buffer of a frame's size writes every frame into it with no allocation.
// It fails if AppendFrame builds the frame in a buffer of its own and copies
// it, or grows dst when dst already has the room.
func TestAppendFrameAllocatesNothingIntoACapacity(t *testing.T) {
	f := Frame{Stream: StreamCallMix, Seq: 7, ClockUS: 1_790_000_000_000_000, PCM: pcmOf(480)}
	buf := make([]byte, 0, HeaderSize+960)
	allocs := testing.AllocsPerRun(100, func() {
		buf = AppendFrame(buf[:0], f)
	})
	if allocs != 0 {
		t.Errorf("AppendFrame into a buffer with the room allocated %v times a frame; want 0", allocs)
	}
	if len(buf) != HeaderSize+960 {
		t.Fatalf("the frame is %d bytes; want 976", len(buf))
	}
	if cap(buf) != HeaderSize+960 {
		t.Errorf("the buffer's capacity moved to %d; want the 976 it had", cap(buf))
	}
}

// TestParseFrameReusesThePCMCapacity holds that a receiver that parses every
// frame into one Frame keeps one sample buffer: ParseFrame writes the samples
// into f.PCM's array when it has the room. It fails if ParseFrame makes a new
// slice for each frame.
func TestParseFrameReusesThePCMCapacity(t *testing.T) {
	b := AppendFrame(nil, Frame{Stream: StreamWisprVoice, Generation: 3, Seq: 9, PCM: pcmOf(480)})
	f := Frame{PCM: make([]int16, 0, 480)}
	first := &f.PCM[:1][0]
	allocs := testing.AllocsPerRun(100, func() {
		if err := ParseFrame(b, &f); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("ParseFrame into a Frame whose PCM has the room allocated %v times a frame; want 0", allocs)
	}
	if len(f.PCM) != 480 || &f.PCM[0] != first {
		t.Errorf("f.PCM is %d samples in a new array; want 480 in the array it had", len(f.PCM))
	}
}
