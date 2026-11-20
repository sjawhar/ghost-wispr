package audio

// AudioEncoding represents the encoding format of audio data.
type AudioEncoding int

const (
	// EncodingPCM16 is signed 16-bit little-endian PCM.
	EncodingPCM16 AudioEncoding = iota
	// EncodingMP3 is MPEG Layer 3 compressed audio.
	EncodingMP3
)

func (e AudioEncoding) String() string {
	switch e {
	case EncodingPCM16:
		return "pcm16"
	case EncodingMP3:
		return "mp3"
	default:
		return "unknown"
	}
}

// AudioFormat describes audio: a TTS provider's clip (DecodePCM16) or the
// PCM16 frames a Playback writes.
type AudioFormat struct {
	SampleRate int
	Channels   int
	Encoding   AudioEncoding
}
