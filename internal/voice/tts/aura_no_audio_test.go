package tts

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestAnAuraSayWithNoWholeSampleFails fails if voice A takes a Say as spoken
// when Deepgram Aura answers its Flush with Flushed alone, or with less than
// one whole sample, or if either reaches the speaker: the Supervisor would
// record an answer spoken that played nothing. One whole sample is speech
// (the control).
func TestAnAuraSayWithNoWholeSampleFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		pcm  []byte
		ok   bool
	}{
		{"Flushed with no audio", nil, false},
		{"one byte, no whole sample", []byte{7}, false},
		{"control: one whole sample", []byte{7, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startSpeak(t, speakBack(map[string][]byte{"hello": tc.pcm}))
			out := newFakeOutput()
			s := openSession(t, newTestAura(t, f), out)
			err := s.Say(context.Background(), "hello", voice.Fact)
			played := playedAudio(out)
			if tc.ok {
				if err != nil || !bytes.Equal(played, tc.pcm) {
					t.Fatalf("Say = %v after playing %d bytes; want the whole sample played", err, len(played))
				}
				return
			}
			if !errors.Is(err, errNoAudio) {
				t.Errorf("Say = %v, want %v", err, errNoAudio)
			}
			if len(played) != 0 {
				t.Errorf("played %d bytes, want none", len(played))
			}
		})
	}
}
