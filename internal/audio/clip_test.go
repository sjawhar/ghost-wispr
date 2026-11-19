package audio

import (
	"bytes"
	"testing"
)

func TestAudioEncodingString(t *testing.T) {
	tests := []struct {
		enc  AudioEncoding
		want string
	}{
		{EncodingPCM16, "pcm16"},
		{EncodingMP3, "mp3"},
		{AudioEncoding(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.enc.String(); got != tt.want {
			t.Errorf("AudioEncoding(%d).String() = %q, want %q", tt.enc, got, tt.want)
		}
	}
}

// TestDecodePCM16 fails if PCM16 does not pass through as it is, or if
// audio that cannot be decoded, an MP3 cut short or an unknown encoding, is
// not refused.
func TestDecodePCM16(t *testing.T) {
	pcm := []byte{1, 0, 2, 0}
	format := AudioFormat{SampleRate: 24000, Channels: 1}
	got, gotFormat, err := DecodePCM16(pcm, format)
	if err != nil || !bytes.Equal(got, pcm) || gotFormat != format {
		t.Fatalf("PCM16 = %v, %+v, %v; want it as it is", got, gotFormat, err)
	}
	if _, _, err := DecodePCM16([]byte{0xFF, 0xFB, 0x90, 0x00}, AudioFormat{SampleRate: 44100, Channels: 2, Encoding: EncodingMP3}); err == nil {
		t.Error("an MP3 cut short decoded")
	}
	if _, _, err := DecodePCM16([]byte{1, 2}, AudioFormat{SampleRate: 16000, Channels: 1, Encoding: AudioEncoding(99)}); err == nil {
		t.Error("an unknown encoding decoded")
	}
}
