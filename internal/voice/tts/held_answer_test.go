package tts_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// burstEngine emits a text's whole audio, length long, in one chunk once
// latency passed; with gap, it emits it in two halves gap apart.
type burstEngine struct {
	latency, length, gap time.Duration
}

func (burstEngine) Name() string { return "burst" }
func (burstEngine) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
}
func (e burstEngine) Stream(ctx context.Context, _ string, emit func([]byte) error) error {
	select {
	case <-time.After(e.latency):
	case <-ctx.Done():
		return ctx.Err()
	}
	pcm := make([]byte, int(e.length/time.Millisecond)*48)
	if e.gap == 0 {
		return emit(pcm)
	}
	if err := emit(pcm[:len(pcm)/2]); err != nil {
		return err
	}
	select {
	case <-time.After(e.gap):
	case <-ctx.Done():
		return ctx.Err()
	}
	return emit(pcm[len(pcm)/2:])
}

// realtimeSpeaker plays at real time with a 200 ms queue, as audio.Output
// does: Write blocks while 200 ms waits, Drain waits for what was written,
// and Cancel ends the playbacks begun before it. It counts voice frames.
type realtimeSpeaker struct {
	mu           sync.Mutex
	gen          map[string]int
	cut          map[string]chan struct{}
	playEnd      time.Time
	voiceFrames  atomic.Int64
	chimeSamples atomic.Int64
	holdChime    chan struct{}
}

func newRealtimeSpeaker() *realtimeSpeaker {
	return &realtimeSpeaker{gen: map[string]int{}, cut: map[string]chan struct{}{audio.InputVoice: make(chan struct{}), audio.InputChime: make(chan struct{})}}
}

func (s *realtimeSpeaker) Begin(input string, _ any) voice.Playback {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &rtPlayback{s: s, input: input, cut: s.cut[input]}
}

func (s *realtimeSpeaker) Cancel(input string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.cut[input])
	s.cut[input] = make(chan struct{})
}

func (s *realtimeSpeaker) OnSpeech(func(audio.SpeechEvent)) func() { return func() {} }
func (s *realtimeSpeaker) FlushSpeech()                            {}

type rtPlayback struct {
	s     *realtimeSpeaker
	input string
	cut   chan struct{}
	end   time.Time
}

func (p *rtPlayback) Write(ctx context.Context, frame []byte, f audio.AudioFormat) error {
	select {
	case <-p.cut:
		return audio.ErrCancelled
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.input != audio.InputVoice {
		if p.s.holdChime != nil {
			select {
			case <-p.s.holdChime:
			case <-p.cut:
				return audio.ErrCancelled
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		p.s.chimeSamples.Add(int64(len(frame) / 2))
		return nil // the chimes do not pace
	}
	d := time.Duration(len(frame)/2) * time.Second / time.Duration(f.SampleRate)
	p.s.mu.Lock()
	start := p.s.playEnd
	if now := time.Now(); start.Before(now) {
		start = now
	}
	p.s.playEnd = start.Add(d)
	p.end = p.s.playEnd
	wait := time.Until(p.s.playEnd.Add(-200 * time.Millisecond))
	p.s.mu.Unlock()
	p.s.voiceFrames.Add(1)
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-p.cut:
			return audio.ErrCancelled
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *rtPlayback) Drain(ctx context.Context) error {
	p.s.mu.Lock()
	end := p.end
	p.s.mu.Unlock()
	select {
	case <-time.After(time.Until(end)):
		return nil
	case <-p.cut:
		return audio.ErrCancelled
	case <-ctx.Done():
		return ctx.Err()
	}
}

// heldOnly is a Concierge that hands the Supervisor one held answer and
// records Spoken.
type heldOnly struct {
	answers chan voice.PendingAnswer
	spoken  chan string
}

func (c *heldOnly) Tools(*voice.Exchange) voice.ToolRunner                               { return nil }
func (c *heldOnly) Heard(context.Context, *voice.Exchange, string)                       {}
func (c *heldOnly) Answers() <-chan voice.PendingAnswer                                  { return c.answers }
func (c *heldOnly) QuestionsAsked() uint64                                               { return 0 }
func (c *heldOnly) NeverMind(context.Context, string, uint64) (bool, error)              { return false, nil }
func (c *heldOnly) Spoken(_ context.Context, id string) error                            { c.spoken <- id; return nil }
func (c *heldOnly) Answer(context.Context, *voice.Exchange, string, voice.SayFunc) error { return nil }

// TestAHeldAnswerSpokenByVoiceAPlays: the real Supervisor speaks a held
// answer that needs no invitation (a free floor) through the real voice A.
// Each round counts it lost when the Supervisor records it spoken and not one
// frame of it reached the speaker. Control: an engine that emits the audio in
// two halves 30 ms apart. Treatment: one that emits it all at once.
func TestAHeldAnswerSpokenByVoiceAPlays(t *testing.T) {
	const rounds = 20
	for _, tc := range []struct {
		name   string
		engine burstEngine
	}{
		{"control: audio in two halves", burstEngine{latency: 20 * time.Millisecond, length: 300 * time.Millisecond, gap: 30 * time.Millisecond}},
		{"treatment: audio all at once", burstEngine{latency: 20 * time.Millisecond, length: 300 * time.Millisecond}},
		{"treatment: two halves 2 ms apart", burstEngine{latency: 20 * time.Millisecond, length: 300 * time.Millisecond, gap: 2 * time.Millisecond}},
		{"treatment: two halves 10 ms apart", burstEngine{latency: 20 * time.Millisecond, length: 300 * time.Millisecond, gap: 10 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lost, spokenRounds := 0, 0
			for round := range rounds {
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
					time.Sleep(50 * time.Millisecond) // any frame still on its way
					if speaker.voiceFrames.Load() == 0 {
						lost++
					}
				case <-time.After(5 * time.Second):
					t.Errorf("round %d: the held answer was not recorded spoken in 5 s", round)
				}
				s.Close()
			}
			t.Logf("%d of %d rounds recorded spoken; %d of them with no frame of the answer at the speaker", spokenRounds, rounds, lost)
			if lost > 0 {
				t.Errorf("a held answer recorded spoken was never heard in %d of %d rounds", lost, rounds)
			}
		})
	}
}
