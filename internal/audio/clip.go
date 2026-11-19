package audio

import (
	"bytes"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
)

// DecodePCM16 returns data, audio in format, as PCM16 little-endian, with its
// format: MP3 decoded, PCM16 as it is.
func DecodePCM16(data []byte, format AudioFormat) ([]byte, AudioFormat, error) {
	switch format.Encoding {
	case EncodingPCM16:
		return data, format, nil
	case EncodingMP3:
		decoder, err := mp3.NewDecoder(bytes.NewReader(data))
		if err != nil {
			return nil, AudioFormat{}, fmt.Errorf("create mp3 decoder: %w", err)
		}
		pcm, err := io.ReadAll(decoder)
		if err != nil {
			return nil, AudioFormat{}, fmt.Errorf("read decoded mp3 data: %w", err)
		}
		// go-mp3 always decodes to 16-bit stereo.
		return pcm, AudioFormat{SampleRate: decoder.SampleRate(), Channels: 2, Encoding: EncodingPCM16}, nil
	default:
		return nil, AudioFormat{}, fmt.Errorf("unsupported audio encoding: %v", format.Encoding)
	}
}
