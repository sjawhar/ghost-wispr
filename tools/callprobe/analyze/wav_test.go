package analyze

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
)

// wavFile is a WAV file of rate, channels and bits per sample, holding pcm
// (interleaved little-endian samples); dataSize is the data chunk's size
// field, which a recording cut off before its end leaves at 0. With
// extensible, the format chunk is WAVE_FORMAT_EXTENSIBLE's.
func wavFile(rate, channels, bits int, pcm []byte, dataSize uint32, extensible bool) []byte {
	le := binary.LittleEndian
	format := []byte{}
	tag := uint16(1)
	if extensible {
		tag = 0xFFFE
	}
	format = le.AppendUint16(format, tag)
	format = le.AppendUint16(format, uint16(channels))
	format = le.AppendUint32(format, uint32(rate))
	format = le.AppendUint32(format, uint32(rate*channels*bits/8))
	format = le.AppendUint16(format, uint16(channels*bits/8))
	format = le.AppendUint16(format, uint16(bits))
	if extensible {
		format = le.AppendUint16(format, 22)
		format = le.AppendUint16(format, uint16(bits))
		format = le.AppendUint32(format, 0)
		// KSDATAFORMAT_SUBTYPE_PCM: the format tag 1, then the GUID's tail.
		format = append(format, 1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71)
	}
	b := []byte("RIFF")
	b = le.AppendUint32(b, uint32(4+8+len(format)+8+8+len(pcm)))
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = le.AppendUint32(b, uint32(len(format)))
	b = append(b, format...)
	b = append(b, "LIST"...) // a chunk the reader skips, as ffmpeg writes one
	b = le.AppendUint32(b, 0)
	b = append(b, "data"...)
	b = le.AppendUint32(b, dataSize)
	return append(b, pcm...)
}

// pcm16 is samples as 16-bit little-endian PCM.
func pcm16(samples ...int16) []byte {
	var b []byte
	for _, s := range samples {
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
	}
	return b
}

// The reader gives one channel, or the mean of all, at full scale ±1, from
// plain and WAVE_FORMAT_EXTENSIBLE 16-bit PCM, reads a data chunk whose size
// was never written to the file's end, and refuses what it cannot read.
func TestReadWAVReadsOneChannelOf16BitPCM(t *testing.T) {
	mono := pcm16(0, 16384, -32768)
	stereo := pcm16(100, 200, 300, 400)
	for _, tc := range []struct {
		name  string
		file  []byte
		opts  ReadOptions
		want  []float32
		start time.Duration
		err   string
	}{
		{"mono", wavFile(8000, 1, 16, mono, uint32(len(mono)), false), ReadOptions{}, []float32{0, 0.5, -1}, 0, ""},
		{"extensible", wavFile(8000, 1, 16, mono, uint32(len(mono)), true), ReadOptions{}, []float32{0, 0.5, -1}, 0, ""},
		{"a size never written", wavFile(8000, 1, 16, mono, 0, false), ReadOptions{}, []float32{0, 0.5, -1}, 0, ""},
		{"the second channel", wavFile(8000, 2, 16, stereo, uint32(len(stereo)), false), ReadOptions{Channel: 2}, []float32{200. / 32768, 400. / 32768}, 0, ""},
		{"the channels' mean", wavFile(8000, 2, 16, stereo, uint32(len(stereo)), false), ReadOptions{}, []float32{150. / 32768, 350. / 32768}, 0, ""},
		{"a window", wavFile(10, 1, 16, pcm16(0, 1, 2, 3, 4, 5, 6, 7, 8, 9), 20, false),
			ReadOptions{Start: 300 * time.Millisecond, Length: 400 * time.Millisecond},
			[]float32{3. / 32768, 4. / 32768, 5. / 32768, 6. / 32768}, 300 * time.Millisecond, ""},
		{"a channel it lacks", wavFile(8000, 2, 16, stereo, uint32(len(stereo)), false), ReadOptions{Channel: 3}, nil, 0, "channel 3"},
		{"24-bit PCM", wavFile(8000, 1, 24, make([]byte, 6), 6, false), ReadOptions{}, nil, 0, "pcm_s16le"},
		{"not a WAV file", []byte("ID3 an MP3 file, not a WAV one"), ReadOptions{}, nil, 0, "WAV"},
		{"a start before the file's", wavFile(10, 1, 16, pcm16(0, 1, 2, 3), 8, false),
			ReadOptions{Start: -200 * time.Millisecond}, nil, 0, "non-negative"},
		{"a negative length", wavFile(10, 1, 16, pcm16(0, 1, 2, 3), 8, false),
			ReadOptions{Length: -100 * time.Millisecond}, nil, 0, "non-negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := readWAV(bytes.NewReader(tc.file), tc.opts)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("readWAV = %v, want an error naming %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(a.Samples, tc.want) || a.Start != tc.start {
				t.Errorf("readWAV = %v from %v, want %v from %v", a.Samples, a.Start, tc.want, tc.start)
			}
		})
	}
}

// A window that the file does not hold is refused, never read as the part
// of it the file holds: c-notice reads 10 minutes of the loopback and wants
// no sound, which a recording of 2 minutes would give. So is a data chunk
// whose size says more than the file holds. The files hold 0.4 s at 10 Hz.
func TestReadWAVRefusesAWindowTheFileDoesNotHold(t *testing.T) {
	four := pcm16(0, 1, 2, 3)
	ms := time.Millisecond
	for _, tc := range []struct {
		name string
		pcm  []byte
		size uint32 // the data chunk's size field: 0 is a size never written
		opts ReadOptions
		want []float32
		err  string
	}{
		{"a window it holds", four, 8, ReadOptions{Start: 100 * ms, Length: 300 * ms}, []float32{1. / 32768, 2. / 32768, 3. / 32768}, ""},
		{"to its end", four, 8, ReadOptions{Start: 200 * ms}, []float32{2. / 32768, 3. / 32768}, ""},
		{"to the end of a size never written", four, 0, ReadOptions{Start: 200 * ms}, []float32{2. / 32768, 3. / 32768}, ""},
		{"a window past its end", four, 8, ReadOptions{Start: 200 * ms, Length: 300 * ms}, nil, "ends after the file's end"},
		{"a window past the end of a size never written", four, 0, ReadOptions{Length: 500 * ms}, nil, "ends after the file's end"},
		{"a start at its end", four, 8, ReadOptions{Start: 400 * ms}, nil, "holds no sample"},
		{"a start after its end", four, 8, ReadOptions{Start: 500 * ms, Length: 100 * ms}, nil, "holds no sample"},
		{"a start after the end of a size never written", four, 0, ReadOptions{Start: 500 * ms}, nil, "holds no sample"},
		{"a data chunk of no sample", nil, 0, ReadOptions{}, nil, "holds no sample"},
		{"a data chunk cut short", four, 20, ReadOptions{}, nil, "cut short"},
		{"a window a data chunk cut short says it holds", four, 20, ReadOptions{Length: 800 * ms}, nil, "cut short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := readWAV(bytes.NewReader(wavFile(10, 1, 16, tc.pcm, tc.size, false)), tc.opts)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("readWAV = %v samples, error %v; want an error naming %q", len(a.Samples), err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(a.Samples, tc.want) {
				t.Errorf("readWAV = %v, want %v", a.Samples, tc.want)
			}
		})
	}
}
