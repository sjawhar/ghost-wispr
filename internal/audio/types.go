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

// AudioFormat describes the format of audio data passed to Speaker.Play.
type AudioFormat struct {
	SampleRate int
	Channels   int
	Encoding   AudioEncoding
}

// PlaybackResult contains information about a completed playback operation.
type PlaybackResult struct {
	BytesWritten int64
	DurationMs   int64
}
