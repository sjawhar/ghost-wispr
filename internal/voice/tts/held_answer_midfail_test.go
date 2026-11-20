package tts_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// failingEngine fails its first stream after first of audio (or before audio
// when first is zero). Its next stream sends 300 ms for the invitation retry.
type failingEngine struct {
	first time.Duration
	calls atomic.Int32
}

func (*failingEngine) Name() string { return "failing" }
func (*failingEngine) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
}
func (e *failingEngine) Stream(ctx context.Context, _ string, emit func([]byte) error) error {
	n := e.calls.Add(1)
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	if n > 1 {
		return emit(make([]byte, 300*48))
	}
	if e.first > 0 {
		if err := emit(make([]byte, int(e.first/time.Millisecond)*48)); err != nil {
			return err
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("gemini tts: stream error 500 INTERNAL: the engine failed")
}

func midfailGoAhead(at time.Time) *voice.Transcript {
	text := "Wispr, go ahead"
	t := &voice.Transcript{Text: text, IsFinal: true, SpeechFinal: true, GateOpen: true, At: at}
	for i, w := range strings.Fields(text) {
		start := 100 + 0.2*float64(i)
		t.Words = append(t.Words, voice.Word{Text: w, Start: start, End: start + 0.15})
	}
	return t
}

// TestAHeldAnswerCutByAnEngineFailureStaysForAnInvitation fails if a held
// answer whose engine fails after its first frame is neither spoken nor kept
// for "Wispr, go ahead". R11 keeps an answer cut inside ReplayWindow pending.
func TestAHeldAnswerCutByAnEngineFailureStaysForAnInvitation(t *testing.T) {
	const rounds = 5
	for _, tc := range []struct {
		name  string
		first time.Duration
	}{
		{"control: fails before any audio", 0},
		{"treatment: fails after 100 ms of audio", 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := 0
			for range rounds {
				speaker := newRealtimeSpeaker()
				c := &heldOnly{answers: make(chan voice.PendingAnswer, 1), spoken: make(chan string, 1)}
				engine := &failingEngine{first: tc.first}
				v := tts.New(engine)
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
				deadline := time.Now().Add(3 * time.Second)
				for engine.calls.Load() < 1 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(700 * time.Millisecond)
				s.OnTranscript(midfailGoAhead(time.Now()))
				select {
				case <-c.spoken:
					kept++
				case <-time.After(3 * time.Second):
				}
				t.Logf("streams: %d", engine.calls.Load())
				s.Close()
			}
			if kept != rounds {
				t.Errorf("an answer cut by an engine failure was lost in %d of %d rounds", rounds-kept, rounds)
			}
		})
	}
}
