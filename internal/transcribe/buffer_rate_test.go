package transcribe

import (
	"context"
	"testing"
)

// TestTheReconnectBufferHoldsSixtySecondsAtTheAudiosRate fails if the buffer
// that holds live audio while Deepgram reconnects does not hold 60 s at the
// rate of the audio written: it was 60 s at 16000 Hz (1,920,000 bytes), so 20
// s at 48000 Hz. A buffer size set in bytes is kept as it is.
func TestTheReconnectBufferHoldsSixtySecondsAtTheAudiosRate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config ResilientConfig
		want   int
	}{
		{name: "48000 Hz", config: ResilientConfig{SampleRate: 48000}, want: 60 * 48000 * 2},
		{name: "16000 Hz", config: ResilientConfig{SampleRate: 16000}, want: 60 * 16000 * 2},
		{name: "rate unknown: 16000 Hz", config: ResilientConfig{}, want: 60 * 16000 * 2},
		{name: "a size set in bytes", config: ResilientConfig{SampleRate: 48000, BufferSize: 1000}, want: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rc := NewResilientClient(ctx, nil, tc.config, nil, nil)
			if got := len(rc.Buffer.buf); got != tc.want {
				t.Fatalf("buffer of %d bytes (%.1f s at the audio's rate), want %d", got, float64(got)/float64(2*max(tc.config.SampleRate, 16000)), tc.want)
			}
		})
	}
}
