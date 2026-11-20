package tts_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// tailEngine emits first after latency, then the rest after gap: a stream
// whose next chunk arrives after the audio already queued has played.
type tailEngine struct {
	latency, first, gap, rest time.Duration
}

func (tailEngine) Name() string { return "tail" }
func (tailEngine) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
}
func (e tailEngine) Stream(ctx context.Context, _ string, emit func([]byte) error) error {
	wait := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := wait(e.latency); err != nil {
		return err
	}
	if err := emit(make([]byte, int(e.first/time.Millisecond)*48)); err != nil {
		return err
	}
	if err := wait(e.gap); err != nil {
		return err
	}
	return emit(make([]byte, int(e.rest/time.Millisecond)*48))
}

// tailSpeaker plays at real time with a 200 ms queue. Drain returns when its
// input is idle, as audio.Output's does, rechecking as later audio is written.
type tailSpeaker struct {
	mu       sync.Mutex
	cut      map[string]chan struct{}
	playEnd  time.Time
	voiceMs  atomic.Int64
	drainErr error
}

func newTailSpeaker() *tailSpeaker {
	return &tailSpeaker{cut: map[string]chan struct{}{audio.InputVoice: make(chan struct{}), audio.InputChime: make(chan struct{})}}
}

func (s *tailSpeaker) Begin(input string) voice.Playback {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &tailPlayback{s: s, input: input, cut: s.cut[input]}
}

func (s *tailSpeaker) Cancel(input string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.cut[input])
	s.cut[input] = make(chan struct{})
}

// playedBy reports whether every frame written to the voice input has played
// by at.
func (s *tailSpeaker) playedBy(at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !at.Before(s.playEnd)
}

func (s *tailSpeaker) OnSpeech(func(audio.SpeechEvent)) func() { return func() {} }

type tailPlayback struct {
	s     *tailSpeaker
	input string
	cut   chan struct{}
}

func (p *tailPlayback) Write(ctx context.Context, frame []byte, f audio.AudioFormat) error {
	select {
	case <-p.cut:
		return audio.ErrCancelled
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.input != audio.InputVoice {
		return nil
	}
	d := time.Duration(len(frame)/2) * time.Second / time.Duration(f.SampleRate)
	p.s.mu.Lock()
	start := p.s.playEnd
	if now := time.Now(); start.Before(now) {
		start = now
	}
	p.s.playEnd = start.Add(d)
	wait := time.Until(p.s.playEnd.Add(-200 * time.Millisecond))
	p.s.mu.Unlock()
	p.s.voiceMs.Add(int64(d / time.Millisecond))
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

func (p *tailPlayback) Drain(ctx context.Context) error {
	for {
		p.s.mu.Lock()
		left, err := time.Until(p.s.playEnd), p.s.drainErr
		p.s.mu.Unlock()
		if left <= 0 {
			return err
		}
		select {
		case <-time.After(min(left, 5*time.Millisecond)):
		case <-p.cut:
			return audio.ErrCancelled
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TestAHeldAnswerRecordedSpokenPlayedToItsEnd fails if the real Supervisor
// records a held answer spoken before voice A has queued and played its final
// chunk. The control's second chunk reaches voice A before the first plays;
// the treatment's arrives after the queued first chunk has played. Both are
// read the moment Spoken is recorded: a frame still queued then, or a chunk
// not yet queued, counts as cut.
func TestAHeldAnswerRecordedSpokenPlayedToItsEnd(t *testing.T) {
	const rounds = 20
	for _, tc := range []struct {
		name   string
		engine tailEngine
	}{
		{"control: second chunk before the first played", tailEngine{latency: 20 * time.Millisecond, first: 300 * time.Millisecond, gap: 100 * time.Millisecond, rest: 400 * time.Millisecond}},
		{"treatment: second chunk after the first played", tailEngine{latency: 20 * time.Millisecond, first: 100 * time.Millisecond, gap: 300 * time.Millisecond, rest: 400 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole := int64((tc.engine.first + tc.engine.rest) / time.Millisecond)
			cut := 0
			for range rounds {
				speaker := newTailSpeaker()
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
					at := time.Now()
					if played := speaker.voiceMs.Load(); played < whole || !speaker.playedBy(at) {
						cut++
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("the held answer was not recorded spoken in 5 s")
				}
				s.Close()
			}
			if cut > 0 {
				t.Fatalf("a held answer recorded spoken was cut short in %d of %d rounds", cut, rounds)
			}
		})
	}
}

// TestAHeldAnswerWithAFailedDrainIsNotRecordedSpoken fails if voice A reports
// successful completion after its output rejected the final playback.
func TestAHeldAnswerWithAFailedDrainIsNotRecordedSpoken(t *testing.T) {
	speaker := newTailSpeaker()
	speaker.drainErr = errors.New("speaker failed")
	c := &heldOnly{answers: make(chan voice.PendingAnswer, 1), spoken: make(chan string, 1)}
	v := tts.New(tailEngine{first: 100 * time.Millisecond, rest: 100 * time.Millisecond})
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
	t.Cleanup(s.Close)
	c.answers <- voice.PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now().Add(-time.Second)}
	select {
	case <-c.spoken:
		t.Fatal("a held answer with a failed final drain was recorded spoken")
	case <-time.After(time.Second):
	}
}

type solTailEngine struct{ first, gap, rest time.Duration }

func (solTailEngine) Name() string { return "sol_tail" }
func (solTailEngine) Format() audio.AudioFormat {
	return audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}
}
func (e solTailEngine) Stream(ctx context.Context, _ string, emit func([]byte) error) error {
	chunk := func(d time.Duration) []byte {
		pcm := make([]byte, int(d/time.Millisecond)*48)
		for i := range len(pcm) / 2 {
			pcm[2*i], pcm[2*i+1] = 0x34, 0x12
		}
		return pcm
	}
	if err := emit(chunk(e.first)); err != nil {
		return err
	}
	select {
	case <-time.After(e.gap):
	case <-ctx.Done():
		return ctx.Err()
	}
	return emit(chunk(e.rest))
}

type solTailStream struct{ samples atomic.Int64 }

func (s *solTailStream) Write(p []int16) error {
	for _, sample := range p {
		if sample == 0x1234 {
			s.samples.Add(1)
		}
	}
	time.Sleep(10 * time.Millisecond)
	return nil
}
func (*solTailStream) Drop() (int, error) { return 0, nil }
func (*solTailStream) Close() error       { return nil }

// TestSolHeldTailWithRealOutput uses the real Supervisor, voice A and
// audio.Output. A held answer recorded spoken must have played all of its
// marked samples by the moment Spoken is recorded, allowing one resampling
// boundary sample.
func TestSolHeldTailWithRealOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    solTailEngine
	}{
		{"control: continuous stream", solTailEngine{300 * time.Millisecond, 100 * time.Millisecond, 400 * time.Millisecond}},
		{"treatment: gap after first chunk", solTailEngine{100 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &solTailStream{}
			out := audio.NewOutput(audio.OutputConfig{Open: func(string) (audio.PlaybackStream, int, error) {
				return stream, 24000, nil
			}})
			t.Cleanup(func() { _ = out.Close() })
			c := &heldOnly{answers: make(chan voice.PendingAnswer, 1), spoken: make(chan string, 1)}
			v := tts.New(tc.e)
			s := voice.NewSupervisor(&voice.SupervisorConfig{
				Voice:       func() (voice.Voice, error) { return v, nil },
				Concierge:   c,
				Speaker:     voice.SpeakerOf(out),
				Session:     func() string { return "sess-1" },
				Red:         func() bool { return true },
				SilenceWait: 500 * time.Millisecond,
				SoundGap:    10 * time.Millisecond,
				Logf:        t.Logf,
			})
			t.Cleanup(s.Close)
			c.answers <- voice.PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now().Add(-time.Second)}
			select {
			case <-c.spoken:
			case <-time.After(3 * time.Second):
				t.Fatal("the answer was not recorded spoken")
			}
			got := stream.samples.Load()
			want := int64((tc.e.first + tc.e.rest) / time.Millisecond * 24)
			if got < want-1 {
				t.Fatalf("recorded spoken after %d/%d marked samples played", got, want)
			}
			t.Logf("complete stream: %d marked samples, expected at least %d", got, want-1)
		})
	}
}
