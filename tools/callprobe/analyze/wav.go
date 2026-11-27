// Package analyze reads the measuring laptop's recordings and finds in them
// what the live checks measure: where each sound starts and ends, when each
// click comes, how much later one file holds a copy of another, and how loud
// that copy is against the rest of the recording. Each analysis that finds
// nothing says so, and none gives a level it cannot tell from the noise.
package analyze

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Audio is one channel of a WAV file at full scale ±1, from Start into the
// file.
type Audio struct {
	Rate    int
	Start   time.Duration
	Samples []float32
}

// At is the time, from the file's start, of sample i.
func (a Audio) At(i int) time.Duration {
	return a.Start + time.Duration(int64(i)*int64(time.Second)/int64(a.Rate))
}

// Duration is how long a's samples last.
func (a Audio) Duration() time.Duration { return a.At(len(a.Samples)) - a.Start }

// samples is the number of whole samples in d at a's rate.
func (a Audio) samples(d time.Duration) int {
	return int(int64(d) * int64(a.Rate) / int64(time.Second))
}

// ReadOptions choose what of a WAV file to read.
type ReadOptions struct {
	Channel int           // from 1; 0 reads the mean of every channel
	Start   time.Duration // from the file's start
	Length  time.Duration // 0 reads to the end
}

// ReadWAVFile reads the file at path (16-bit PCM, the format callprobe record
// has ffmpeg write).
func ReadWAVFile(path string, o ReadOptions) (Audio, error) {
	f, err := os.Open(path)
	if err != nil {
		return Audio{}, err
	}
	defer func() { _ = f.Close() }()
	a, err := readWAV(f, o)
	if err != nil {
		return Audio{}, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// WAVFrames is how many sample frames the data chunk of the WAV file at
// path holds: as many as its size says, or, a size never written, as many
// as reach the file's end; never more than the file holds. A header alone,
// whatever chunks it holds, is 0.
func WAVFrames(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	h, err := readHeader(f)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	at, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	held := end - at
	if !unknownSize(h.size) {
		held = min(held, int64(h.size))
	}
	return held / int64(2*h.channels), nil
}

// unknownSize is the data chunk's size field of a WAV file whose writer
// stopped before it went back to fill it in, as ffmpeg leaves it when it is
// killed, or wrote it to a pipe.
func unknownSize(size uint32) bool { return size == 0 || size == 0xFFFFFFFF }

func readWAV(r io.ReadSeeker, o ReadOptions) (Audio, error) {
	if o.Start < 0 || o.Length < 0 {
		return Audio{}, errors.New("start and for must be non-negative")
	}
	h, err := readHeader(r)
	if err != nil {
		return Audio{}, err
	}
	if o.Channel < 0 || o.Channel > h.channels {
		return Audio{}, fmt.Errorf("channel %d: the file has %d", o.Channel, h.channels)
	}
	return readData(r, h, o)
}

// wavHeader is what a WAV file says before its samples.
type wavHeader struct {
	channels, rate int
	size           uint32 // the data chunk's size field
}

// readHeader reads a 16-bit PCM WAV file up to its data chunk's first
// sample.
func readHeader(r io.ReadSeeker) (wavHeader, error) {
	le := binary.LittleEndian
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return wavHeader{}, errors.New("not a WAV file: no RIFF WAVE header")
	}
	var h wavHeader
	for {
		var chunk [8]byte
		if _, err := io.ReadFull(r, chunk[:]); err != nil {
			return wavHeader{}, fmt.Errorf("no data chunk: %w", err)
		}
		id, size := string(chunk[:4]), le.Uint32(chunk[4:])
		switch id {
		case "fmt ":
			if size < 16 || size > 1024 {
				return wavHeader{}, fmt.Errorf("a format chunk of %d bytes", size)
			}
			body := make([]byte, size+size%2)
			if _, err := io.ReadFull(r, body); err != nil {
				return wavHeader{}, fmt.Errorf("format chunk: %w", err)
			}
			tag, bits := le.Uint16(body), le.Uint16(body[14:])
			if tag == 0xFFFE && size >= 26 { // WAVE_FORMAT_EXTENSIBLE: the subformat's tag
				tag = le.Uint16(body[24:])
			}
			if tag != 1 || bits != 16 {
				return wavHeader{}, fmt.Errorf("%d-bit samples of format %d: callprobe reads 16-bit PCM; convert with ffmpeg -i FILE -c:a pcm_s16le OUT.wav", bits, tag)
			}
			h.channels, h.rate = int(le.Uint16(body[2:])), int(le.Uint32(body[4:]))
			if h.channels < 1 || h.rate < 1 {
				return wavHeader{}, fmt.Errorf("%d channels at %d Hz", h.channels, h.rate)
			}
		case "data":
			if h.channels == 0 {
				return wavHeader{}, errors.New("a data chunk before the format chunk")
			}
			h.size = size
			return h, nil
		default:
			if _, err := r.Seek(int64(size)+int64(size%2), io.SeekCurrent); err != nil {
				return wavHeader{}, fmt.Errorf("chunk %q: %w", id, err)
			}
		}
	}
}

// readData reads o's window of the data chunk h describes, and refuses a
// window the file does not hold: one that holds no sample, one that ends
// after the file's end, and one that a file cut short of the size its data
// chunk says does not hold. A size never written (unknownSize) runs to the
// file's end.
func readData(r io.ReadSeeker, h wavHeader, o ReadOptions) (Audio, error) {
	a := Audio{Rate: h.rate}
	frame := int64(2 * h.channels)
	first := int64(a.samples(o.Start))
	a.Start = a.At(int(first))
	known := !unknownSize(h.size)
	total := int64(h.size) / frame // when known
	want := int64(-1)              // the frames of the window; -1: to the file's end
	if known {
		if first >= total {
			return Audio{}, fmt.Errorf("the window from %v holds no sample: the file ends at %v", a.Start, a.At(int(total-first)))
		}
		want = total - first
	}
	if o.Length > 0 {
		n := int64(a.samples(o.Length))
		if want >= 0 && n > want {
			return Audio{}, fmt.Errorf("the window from %v for %v ends after the file's end at %v", a.Start, o.Length, a.At(int(want)))
		}
		want = n
	}
	if _, err := r.Seek(first*frame, io.SeekCurrent); err != nil {
		return Audio{}, err
	}
	var data io.Reader = r
	if want >= 0 {
		data = io.LimitReader(r, want*frame)
	}
	in := bufio.NewReaderSize(data, 1<<16)
	buf := make([]byte, int(frame)*4096)
	scale := float32(1) / 32768
	for {
		n, err := io.ReadFull(in, buf)
		for f := 0; f+int(frame) <= n; f += int(frame) {
			if o.Channel > 0 {
				a.Samples = append(a.Samples, float32(int16(binary.LittleEndian.Uint16(buf[f+2*(o.Channel-1):])))*scale)
				continue
			}
			var sum float32
			for c := range h.channels {
				sum += float32(int16(binary.LittleEndian.Uint16(buf[f+2*c:])))
			}
			a.Samples = append(a.Samples, sum/float32(h.channels)*scale)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return Audio{}, err
		}
	}
	switch got := int64(len(a.Samples)); {
	case got == 0:
		return Audio{}, fmt.Errorf("the window from %v holds no sample", a.Start)
	case got >= want: // all of the window, or to the end of a size never written
		return a, nil
	case known:
		return Audio{}, fmt.Errorf("the data chunk says the file holds %v, but it ends at %v: it was cut short",
			a.At(int(total-first)), a.At(int(got)))
	default:
		return Audio{}, fmt.Errorf("the window from %v for %v ends after the file's end at %v", a.Start, o.Length, a.At(int(got)))
	}
}
