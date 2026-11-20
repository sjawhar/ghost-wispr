package local

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// ReadWAV reads a stored recording for a measurement or a self-test: a RIFF
// WAVE file of 16 kHz mono 16-bit PCM, the listener's own format. It never
// reads the microphone.
func ReadWAV(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pcm, err := decodeWAV(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pcm, nil
}

func decodeWAV(data []byte) ([]int16, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF WAVE file")
	}
	formatSeen := false
	for chunk := data[12:]; len(chunk) >= 8; {
		id, size := string(chunk[:4]), int(binary.LittleEndian.Uint32(chunk[4:8]))
		body := chunk[8:]
		if size > len(body) {
			size = len(body)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("short fmt chunk")
			}
			format, channels := binary.LittleEndian.Uint16(body[0:2]), binary.LittleEndian.Uint16(body[2:4])
			rate, bits := binary.LittleEndian.Uint32(body[4:8]), binary.LittleEndian.Uint16(body[14:16])
			if format != 1 || channels != 1 || rate != SampleRate || bits != 16 {
				return nil, fmt.Errorf("format %d, %d channels, %d Hz, %d bits; want PCM, 1 channel, %d Hz, 16 bits", format, channels, rate, bits, SampleRate)
			}
			formatSeen = true
		case "data":
			if !formatSeen {
				return nil, errors.New("data chunk before the fmt chunk")
			}
			pcm := make([]int16, size/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
			}
			return pcm, nil
		}
		next := size + size%2
		if next > len(body) {
			break
		}
		chunk = body[next:]
	}
	return nil, errors.New("no data chunk")
}
