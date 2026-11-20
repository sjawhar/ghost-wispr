package joined

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// supervisorRig is a real voice.Supervisor over a speaker that records the
// sounds it plays and a voice that records what it says, in meeting sess-1
// with the light red.
type supervisorRig struct {
	t       *testing.T
	s       *voice.Supervisor
	speaker *soundSpeaker
	says    chan string

	mu       sync.Mutex
	handDone func(*voice.Hand, voice.HandOutcome)
	start    float64
}

func newSupervisorRig(t *testing.T) *supervisorRig {
	t.Helper()
	r := &supervisorRig{t: t, speaker: &soundSpeaker{}, says: make(chan string, 8)}
	r.s = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:       func() (voice.Voice, error) { return sayingVoice{says: r.says}, nil },
		Concierge:   quietConcierge{},
		Speaker:     r.speaker,
		WakePhrases: []string{"Wispr", "Ghost Wispr"},
		Session:     func() string { return "sess-1" },
		Red:         func() bool { return true },
		SoundGap:    50 * time.Millisecond,
		SilenceWait: 100 * time.Millisecond,
		HandDone: func(h *voice.Hand, o voice.HandOutcome) {
			r.mu.Lock()
			done := r.handDone
			r.mu.Unlock()
			if done != nil {
				done(h, o)
			}
		},
		Logf: t.Logf,
	})
	t.Cleanup(r.s.Close)
	return r
}

// heard feeds one final result of the room's words.
func (r *supervisorRig) heard(text string) {
	r.mu.Lock()
	start := r.start
	r.start += 5
	r.mu.Unlock()
	tr := voice.Transcript{Text: text, IsFinal: true, SpeechFinal: true, GateOpen: true, At: time.Now()}
	for i, w := range strings.Fields(text) {
		ws := start + 0.2*float64(i)
		tr.Words = append(tr.Words, voice.Word{Text: w, Start: ws, End: ws + 0.15})
	}
	r.s.OnTranscript(&tr)
}

func (r *supervisorRig) said(t *testing.T) string {
	t.Helper()
	select {
	case text := <-r.says:
		return text
	case <-time.After(testWait):
		t.Fatal("the voice said nothing")
		return ""
	}
}

func (r *supervisorRig) saidAnything() bool { return len(r.says) > 0 }

// soundSpeaker records the first frame of each sound on the chime input.
type soundSpeaker struct {
	mu     sync.Mutex
	sounds [][]byte
}

func (s *soundSpeaker) Begin(input string) voice.Playback { return &soundPlayback{s: s, input: input} }
func (s *soundSpeaker) Cancel(string)                     {}
func (s *soundSpeaker) OnSpeech(func(audio.SpeechEvent)) (remove func()) {
	return func() {}
}

// played reports whether the named sound began.
func (s *soundSpeaker) played(name string) bool {
	want := map[string][]byte{"hand": voice.HandChime(), "answer": voice.AnswerChime()}[name]
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, first := range s.sounds {
		if len(want) >= len(first) && bytes.Equal(want[:len(first)], first) {
			return true
		}
	}
	return false
}

type soundPlayback struct {
	s     *soundSpeaker
	input string
	wrote bool
}

func (p *soundPlayback) Write(_ context.Context, frame []byte, _ audio.AudioFormat) error {
	if p.input == audio.InputChime && !p.wrote {
		p.s.mu.Lock()
		p.s.sounds = append(p.s.sounds, bytes.Clone(frame))
		p.s.mu.Unlock()
	}
	p.wrote = true
	return nil
}

func (p *soundPlayback) Drain(context.Context) error { return nil }

// sayingVoice opens sessions that record each text they are asked to say.
type sayingVoice struct{ says chan string }

func (v sayingVoice) Open(context.Context, *voice.Exchange) (voice.Session, error) {
	return &sayingSession{says: v.says, events: make(chan voice.Event)}, nil
}

type sayingSession struct {
	says   chan string
	events chan voice.Event
	once   sync.Once
}

func (s *sayingSession) Say(_ context.Context, text string, _ voice.SayMode) error {
	s.says <- text
	return nil
}
func (s *sayingSession) Deliver(context.Context, string, json.RawMessage) error { return nil }
func (s *sayingSession) Interrupt()                                             {}
func (s *sayingSession) Events() <-chan voice.Event                             { return s.events }
func (s *sayingSession) Close() error {
	s.once.Do(func() { close(s.events) })
	return nil
}

// quietConcierge answers nothing: the tests here ask it nothing.
type quietConcierge struct{}

func (quietConcierge) Tools(*voice.Exchange) voice.ToolRunner         { return nil }
func (quietConcierge) Heard(context.Context, *voice.Exchange, string) {}
func (quietConcierge) Answers() <-chan voice.PendingAnswer            { return nil }
func (quietConcierge) Spoken(context.Context, string) error           { return nil }
func (quietConcierge) NeverMind(context.Context, string) error        { return nil }
func (quietConcierge) Answer(context.Context, *voice.Exchange, string, voice.SayFunc) error {
	return nil
}
