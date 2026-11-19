package audio

// Frame is a buffer of PCM16 audio: PCM holds Channels interleaved samples
// per frame at Rate samples a second. Every voice path is mono (Channels 1).
type Frame struct {
	PCM      []int16
	Rate     int
	Channels int
}
