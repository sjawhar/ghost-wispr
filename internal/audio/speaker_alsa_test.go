package audio

import "testing"

// TestSpeakerPlaysWithoutPortAudio fails if the speaker needs PortAudio to
// play. PortAudio's initialization opens every capture PCM, the Jabra's
// microphone included, so it runs only inside a microphone start, under a red
// light: a unit restarted paused has none, and the speaker must still speak
// (A8). It plays through ALSA's null PCM here, as it plays through the
// Jabra's playback PCM on the unit, with PortAudio not initialized.
func TestSpeakerPlaysWithoutPortAudio(t *testing.T) {
	_ = TerminatePortAudio() // whatever an earlier test initialized
	speaker := NewSpeaker("null")
	samples := make([]int16, 1600) // 100 ms at 16 kHz
	pcm := make([]byte, 2*len(samples))
	result, err := speaker.Play(pcm, AudioFormat{SampleRate: 16000, Channels: 1, Encoding: EncodingPCM16})
	if err != nil {
		t.Fatalf("Play with PortAudio not initialized = %v, want it played through ALSA", err)
	}
	if result.BytesWritten != int64(len(pcm)) {
		t.Fatalf("played %d bytes, want %d", result.BytesWritten, len(pcm))
	}
}
