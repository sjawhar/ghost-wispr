package tts_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// silentEngine ends its stream with no error and, unless it has audio, with no
// audio: as a Gemini stream whose candidates carry no inline data.
type silentEngine struct{ audio time.Duration }

func (silentEngine) Name() string { return "silent" }
func (silentEngine) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
}
func (e silentEngine) Stream(ctx context.Context, _ string, emit func([]byte) error) error {
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	if e.audio == 0 {
		return nil
	}
	return emit(make([]byte, int(e.audio/time.Millisecond)*48))
}

// TestAHeldAnswerWithNoAudioIsNotRecordedSpoken fails if voice A's engine
// returns no audio and no error, and the real Supervisor records a held answer
// spoken without a frame reaching the speaker. The control sends 300 ms.
func TestAHeldAnswerWithNoAudioIsNotRecordedSpoken(t *testing.T) {
	const rounds = 10
	for _, tc := range []struct {
		name   string
		engine silentEngine
	}{
		{"control: 300 ms of audio", silentEngine{audio: 300 * time.Millisecond}},
		{"treatment: no audio, no error", silentEngine{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lost, spokenRounds := 0, 0
			for range rounds {
				speaker := newRealtimeSpeaker()
				c := &heldOnly{answers: make(chan voice.PendingAnswer, 1), spoken: make(chan string, 1)}
				v := tts.New(tc.engine)
				s := voice.NewSupervisor(&voice.SupervisorConfig{
					Voice:       func() (voice.Voice, error) { return v, nil },
					Concierge:   c,
					Speaker:     speaker,
					WakePhrases: []string{"Wispr"},
					Session:     func() string { return "sess-1" },
					Red:         func() bool { return true },
					SilenceWait: 40 * time.Millisecond,
					SoundGap:    10 * time.Millisecond,
					Logf:        func(string, ...any) {},
				})
				c.answers <- voice.PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now().Add(-time.Second)}
				select {
				case <-c.spoken:
					spokenRounds++
					time.Sleep(50 * time.Millisecond)
					if speaker.voiceFrames.Load() == 0 {
						lost++
					}
				case <-time.After(2 * time.Second):
					// Not recorded spoken: the answer stays for an invitation.
				}
				s.Close()
			}
			if lost > 0 {
				t.Errorf("a held answer with no audio was recorded spoken in %d of %d rounds", lost, rounds)
			}
			if tc.engine.audio > 0 && spokenRounds != rounds {
				t.Errorf("control: %d of %d rounds recorded spoken, want all", spokenRounds, rounds)
			}
		})
	}
}
