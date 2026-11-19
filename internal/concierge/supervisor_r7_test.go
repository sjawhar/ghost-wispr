package concierge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The Supervisor's waits in these tests, short so that each runs in seconds.
const (
	r7SoundGap     = 500 * time.Millisecond
	r7SilenceWait  = 400 * time.Millisecond
	r7FollowUpWait = 150 * time.Millisecond
	r7TurnWait     = 300 * time.Millisecond
	r7ReplayWindow = 300 * time.Millisecond
	// r7Deadline bounds each wait for something that must happen.
	r7Deadline = 5 * time.Second
)

const (
	// r7Question calls Wispr and says "ask", so the Concierge asks the holder.
	r7Question = "Wispr, ask the agent on ABC-12 whether the tap is merged"
	// r7Answer is the holder's written answer. The model gives it back
	// unchanged for the ear, and the voice speaks it with the question's key
	// in front: r7Spoken.
	r7Answer = "Yes, PR 80 merged the tap."
	r7Spoken = "ABC-12: " + r7Answer
	// r7RoomWords are words of a person in the room, not Wispr's.
	r7RoomWords = "Let us look at the budget next"
	// r7GoAhead is the invitation.
	r7GoAhead = "Wispr, go ahead"
)

// TestAnswerSoundAndInvitation (c-ask-holder, R7 with R10): a real Supervisor
// over a real Concierge. A holder's answer plays the answer sound once, at a
// gap of SoundGap with no transcript word. With a room word since t0 it waits
// for "Wispr, go ahead"; with none and the light red it is spoken at once; with
// the light not red nothing is spoken.
func TestAnswerSoundAndInvitation(t *testing.T) {
	t.Run("a word since t0", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.waitExchangeEnded()
		// The room talks from before the answer lands until 3 gaps after, a
		// word every fifth of the gap. The answer sound waits for a gap of
		// SoundGap after the newest word before it.
		stop := r.talk(r7SoundGap / 5)
		r.reply()
		time.Sleep(3 * r7SoundGap)
		words := stop()
		r7Until(t, "the answer sound", func() bool { return len(r.speaker.sounds(voice.AnswerChime())) > 0 })
		chime := r.speaker.sounds(voice.AnswerChime())[0]
		newest := words[0]
		for _, at := range words {
			if at.Before(chime) {
				newest = at
			}
		}
		if gap := chime.Sub(newest); gap < r7SoundGap {
			t.Fatalf("the answer sound began %v after the newest room word before it, want a gap of at least %v", gap, r7SoundGap)
		}
		r7Never(t, "the answer was spoken with no invitation after a room word since t0", 3*r7SilenceWait, func() bool { return len(r.voice.heldAnswers()) > 0 })
		if opened, _ := r.voice.counts(); opened != 1 {
			t.Fatalf("%d voices opened before the invitation, want only the question's", opened)
		}
		r.hear(r7GoAhead)
		r7Until(t, "the answer spoken after the invitation", func() bool { return len(r.voice.heldAnswers()) > 0 })
		r.expectSpoken()
		if n := len(r.speaker.sounds(voice.AnswerChime())); n != 1 {
			t.Fatalf("the answer sound played %d times, want once", n)
		}
	})

	t.Run("no word since t0, light red", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.waitExchangeEnded()
		r.reply()
		r7Until(t, "the answer spoken with no invitation", func() bool { return len(r.voice.heldAnswers()) > 0 })
		r.expectSpoken()
		chimes := r.speaker.sounds(voice.AnswerChime())
		if len(chimes) != 1 || !chimes[0].Before(r.voice.heldAnswers()[0].at) {
			t.Fatalf("answer sounds at %v, the answer spoken at %v; want one sound before it", chimes, r.voice.heldAnswers()[0].at)
		}
	})

	t.Run("the light not red", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.waitExchangeEnded()
		r.setRed(false)
		r.reply()
		r7Never(t, "the answer was spoken, or its sound played, with the light not red", 3*(r7SoundGap+r7SilenceWait), func() bool {
			return len(r.voice.heldAnswers()) > 0 || len(r.speaker.sounds(voice.AnswerChime())) > 0
		})
		if opened, _ := r.voice.counts(); opened != 1 {
			t.Fatalf("%d voices opened, want only the question's", opened)
		}
	})
}

// TestAnswerWhilePausedStaysSilent (c-ask-holder, R7 with R10). A pause ends
// the meeting's session, and an answer of an ended session is never spoken:
// not by itself and not on "Wispr, go ahead", and no answer sound plays after
// the resume; its written answer stays in the ledger, for the session's page.
// During a light hold the session goes on: an answer stays silent until the
// light is red again and a person invites it, whether it arrived during the
// hold or before it.
func TestAnswerWhilePausedStaysSilent(t *testing.T) {
	t.Run("pause, the answer before it", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.waitExchangeEnded()
		// A room word since t0 holds the answer, and the talk keeps its sound
		// back until the pause.
		stop := r.talk(r7SoundGap / 5)
		r.reply()
		stop()
		r.closeGate()
		r.endSession()
		r.expectEndedSilent(storage.VoiceTaskDone)
	})

	t.Run("pause, the answer after it", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.waitExchangeEnded()
		r.closeGate()
		r.endSession()
		r.deliver()
		r7Until(t, "the ledger keeps the written answer", func() bool {
			task, err := r.store.VoiceTask("evt-1")
			return err == nil && task.AnswerWritten == r7Answer
		})
		r.expectEndedSilent(storage.VoiceTaskCancelled)
	})

	t.Run("light hold, the answer during it", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.closeGate()
		r.waitExchangeEnded()
		r.reply()
		r7Never(t, "an answer sound played during the light hold", 2*(r7SoundGap+r7SilenceWait), func() bool { return len(r.speaker.sounds(voice.AnswerChime())) > 0 })
		r.openGate()
		r.expectSilentUntilInvited()
	})

	t.Run("light hold, the answer before it", func(t *testing.T) {
		r := newR7Rig(t)
		r.askHolder()
		r.waitExchangeEnded()
		// No room word since t0: the answer would be spoken by itself once the
		// room is silent for SilenceWait after its sound. The hold comes
		// first.
		r.reply()
		r.closeGate()
		if held := r.voice.heldAnswers(); len(held) != 0 {
			t.Fatalf("the answer was spoken before the hold: %+v; the case needs it held", held)
		}
		r.openGate()
		r.expectSilentUntilInvited()
	})
}

// r7Rig is a real Supervisor over a real Concierge: the Concierge on the fake
// model, Dispatch and Envoy hub and a real store, with ABC-12 held by the
// live agent session agent-7; the Supervisor on a fake voice and speaker.
type r7Rig struct {
	t       *testing.T
	c       *Concierge
	agents  *fakeAgents
	store   *storage.SQLiteStore
	s       *voice.Supervisor
	voice   *r7Voice
	speaker *r7Speaker
	ready   chan struct{} // a voice_answer_ready event

	mu       sync.Mutex
	red      bool
	session  string // the recording session now, "" between sessions
	closings int
	start    float64 // the next transcript's socket time
}

func newR7Rig(t *testing.T) *r7Rig {
	t.Helper()
	logs := &lockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", logs.String())
		}
	})
	// The Concierge runs on the wall clock, as the Supervisor does: it
	// compares the question's time with the room's words.
	now := time.Now()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "unit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateSession("meeting-1", now.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	records := &fakeRecords{
		issues: map[string]*dispatch.Issue{"ABC-12": {
			Key: "ABC-12", Title: "Realtime voice", Status: "in_progress", UpdatedAt: now.Add(-2 * time.Hour),
			Claim: &dispatch.Claim{Actor: actor("session", "agent-7", "Worker"), At: now.Add(-time.Hour)},
		}},
		events: map[string][]dispatch.Event{"ABC-12": {{
			Type: "comment.created", CreatedAt: now.Add(-5 * time.Minute), Actor: actor("user", "lead@example.com", ""),
			Payload: json.RawMessage(`{"body":"PR 80 is in review."}`),
		}}},
		docs:     map[string]string{},
		projects: []string{"ABC"},
	}
	model := &scriptedModel{turns: [][]*genai.GenerateContentResponse{
		{chunk(text("PR 80 is in review."))}, // the fast answer's facts
		{chunk(text(r7Answer))},              // the holder's answer, for the ear
	}}
	r := &r7Rig{
		t:       t,
		agents:  &fakeAgents{id: "unit-session", states: map[string]envoy.SessionInfo{"agent-7": {SessionID: "agent-7", State: envoy.StateLive}}},
		store:   store,
		voice:   &r7Voice{},
		speaker: &r7Speaker{},
		ready:   make(chan struct{}, 8),
		red:     true,
		session: "meeting-1",
	}
	r.c, err = New(&Config{
		Model:     "test-model",
		Generator: model,
		Records:   records,
		Store:     store,
		PublicURL: "https://unit.example",
		Now:       time.Now,
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c.SetAgents(r.agents)
	r7Until(t, "the project list was read", func() bool {
		r.c.mu.Lock()
		defer r.c.mu.Unlock()
		return r.c.projects != nil
	})
	r.s = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:       func() (voice.Voice, error) { return r.voice, nil },
		Concierge:   r.c,
		Speaker:     r.speaker,
		WakePhrases: []string{"Wispr", "Ghost Wispr"},
		Session:     r.sessionID,
		Red:         r.isRed,
		Enabled:     func() bool { return true },
		Notify: func(kind, _ string) {
			if kind == voice.EventAnswerReady {
				select {
				case r.ready <- struct{}{}:
				default:
				}
			}
		},
		Tap:          voice.NewTap(),
		TurnWait:     r7TurnWait,
		SilenceWait:  r7SilenceWait,
		SoundGap:     r7SoundGap,
		FollowUpWait: r7FollowUpWait,
		ReplayWindow: r7ReplayWindow,
		Logf: func(format string, args ...any) {
			_, _ = fmt.Fprintln(logs, "supervisor:", fmt.Sprintf(format, args...))
		},
	})
	t.Cleanup(r.s.Close)
	return r
}

func (r *r7Rig) isRed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.red
}

func (r *r7Rig) setRed(red bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.red = red
}

func (r *r7Rig) sessionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

func (r *r7Rig) setSession(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = id
}

// hear gives the Supervisor one final Deepgram result of text, its words
// 0.2 s apart, and returns when it was heard.
func (r *r7Rig) hear(text string) time.Time {
	r.mu.Lock()
	closings, start := r.closings, r.start
	r.start += 5
	r.mu.Unlock()
	at := time.Now()
	tr := voice.Transcript{Text: text, IsFinal: true, SpeechFinal: true, Closings: closings, GateOpen: true, At: at}
	for i, w := range strings.Fields(text) {
		ws := start + 0.2*float64(i)
		tr.Words = append(tr.Words, voice.Word{Text: w, Start: ws, End: ws + 0.15})
	}
	r.s.OnTranscript(&tr)
	return at
}

// talk has the room say r7RoomWords now and then every interval, until stop
// is called, or the test ends; stop returns when each was heard, oldest
// first. Nothing else may give the Supervisor a transcript meanwhile: one
// reader calls OnTranscript.
func (r *r7Rig) talk(interval time.Duration) (stop func() []time.Time) {
	done := make(chan struct{})
	heard := make(chan []time.Time, 1)
	go func() {
		var at []time.Time
		for {
			at = append(at, r.hear(r7RoomWords))
			select {
			case <-done:
				heard <- at
				return
			case <-time.After(interval):
			}
		}
	}()
	var once sync.Once
	end := func() { once.Do(func() { close(done) }) }
	r.t.Cleanup(end)
	return func() []time.Time {
		end()
		return <-heard
	}
}

// closeGate closes the capture gate and turns the light off, as a pause and a
// light hold both do.
func (r *r7Rig) closeGate() {
	r.mu.Lock()
	r.red = false
	r.closings++
	closings := r.closings
	r.mu.Unlock()
	r.s.GateClosed(closings)
}

// endSession ends meeting-1 as a pause's settling does (session.Manager's
// end): the store marks it ended, no session records, and then the
// session-ended hook tells the Concierge and the Supervisor, in that order
// (wireVoiceSessionHooks, cmd/ghost-wispr/voice.go).
func (r *r7Rig) endSession() {
	r.t.Helper()
	if err := r.store.EndSession("meeting-1", time.Now().UTC(), ""); err != nil {
		r.t.Fatal(err)
	}
	r.setSession("")
	if err := r.c.SessionEnded("meeting-1"); err != nil {
		r.t.Fatal(err)
	}
	r.s.SessionEnded("meeting-1")
}

// startSession starts the next recording session, id, as a resume does.
func (r *r7Rig) startSession(id string) {
	r.t.Helper()
	if err := r.store.CreateSession(id, time.Now().UTC()); err != nil {
		r.t.Fatal(err)
	}
	r.setSession(id)
}

// openGate turns the light red and plays the start sound, as a resume and the
// end of a light hold both do, and returns when.
func (r *r7Rig) openGate() time.Time {
	r.t.Helper()
	starts := len(r.speaker.sounds(voice.StartChime()))
	at := time.Now()
	r.setRed(true)
	r.s.Started()
	r7Until(r.t, "the start sound", func() bool { return len(r.speaker.sounds(voice.StartChime())) == starts+1 })
	return at
}

// askHolder asks r7Question, and waits until the Concierge asked the holder
// and said so. Its wake sound is seen on the chime input, and not taken for
// the answer sound: the control that the speaker tells the sounds apart.
func (r *r7Rig) askHolder() {
	r.t.Helper()
	r.hear(r7Question)
	r7Until(r.t, "the Concierge said that it asked the holder", func() bool {
		return slices.ContainsFunc(r.voice.said(), func(s r7Said) bool {
			return s.mode == voice.Ack && strings.HasPrefix(s.text, "I asked the agent on ABC-12.")
		})
	})
	r.agents.mu.Lock()
	sent := slices.Clone(r.agents.sent)
	r.agents.mu.Unlock()
	if len(sent) != 1 || sent[0].Target != "agent-7" {
		r.t.Fatalf("sent %+v, want one question to agent-7", sent)
	}
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.State != storage.VoiceTaskWorking {
		r.t.Fatalf("task = %+v, %v; want evt-1 working", task, err)
	}
	r7Until(r.t, "the wake sound", func() bool { return len(r.speaker.sounds(voice.WakeChime())) == 1 })
	if n := len(r.speaker.sounds(voice.AnswerChime())); n != 0 {
		r.t.Fatalf("%d answer sounds before any answer: the speaker cannot tell the sounds apart", n)
	}
}

// waitExchangeEnded waits until the question's exchange closed its voice.
func (r *r7Rig) waitExchangeEnded() {
	r.t.Helper()
	r7Until(r.t, "the question's exchange ended", func() bool {
		opened, closed := r.voice.counts()
		return opened == 1 && closed == 1
	})
}

// deliver delivers the holder's answer to the question, as the unit's Envoy
// session does.
func (r *r7Rig) deliver() {
	r.t.Helper()
	if err := r.c.Deliver(context.Background(), &envoy.Message{EventID: "reply-1", SourceSession: "agent-7", InReplyTo: "evt-1", Text: r7Answer}); err != nil {
		r.t.Fatalf("Deliver: %v", err)
	}
}

// reply delivers the holder's answer, and waits until it reaches the
// Supervisor.
func (r *r7Rig) reply() {
	r.t.Helper()
	r.deliver()
	select {
	case <-r.ready:
	case <-time.After(r7Deadline):
		r.t.Fatal("the holder's answer never reached the Supervisor")
	}
}

// expectSpoken checks that the holder's answer was spoken once, as an answer,
// with its text for the ear, and that the ledger records it spoken.
func (r *r7Rig) expectSpoken() {
	r.t.Helper()
	if held := r.voice.heldAnswers(); len(held) != 1 || held[0].mode != voice.Answer || held[0].text != r7Spoken {
		r.t.Fatalf("the holder's answer was said as %+v, want %q once as %v", held, r7Spoken, voice.Answer)
	}
	r7Until(r.t, "the ledger records the answer spoken", func() bool {
		task, err := r.store.VoiceTask("evt-1")
		return err == nil && task.SpokenAt != nil
	})
}

// expectSilentUntilInvited checks, once the light is red again, that the
// answer is not spoken through silence longer than SilenceWait, and that
// "Wispr, go ahead" then speaks it.
func (r *r7Rig) expectSilentUntilInvited() {
	r.t.Helper()
	r7Never(r.t, "the answer was spoken with no invitation after the light hold", r7SoundGap+3*r7SilenceWait, func() bool { return len(r.voice.heldAnswers()) > 0 })
	r.hear(r7GoAhead)
	r7Until(r.t, "the answer spoken after the invitation", func() bool { return len(r.voice.heldAnswers()) > 0 })
	r.expectSpoken()
}

// expectEndedSilent resumes into a new session, meeting-2, and checks that
// the ended session's answer is never spoken, by itself or on "Wispr, go
// ahead"; that no answer sound plays after the resume; and that the ledger
// keeps its written answer, unspoken, with the task in state.
func (r *r7Rig) expectEndedSilent(state storage.VoiceTaskState) {
	r.t.Helper()
	r.startSession("meeting-2")
	resumed := r.openGate()
	heard := func() bool {
		return len(r.voice.heldAnswers()) > 0 || r.speaker.soundsSince(voice.AnswerChime(), resumed) > 0
	}
	r7Never(r.t, "after the resume, the ended session's answer was spoken by itself, or the answer sound played", r7SoundGap+3*r7SilenceWait, heard)
	r.hear(r7GoAhead)
	r7Never(r.t, "after \"Wispr, go ahead\", the ended session's answer was spoken, or the answer sound played", r7SoundGap+2*r7SilenceWait, heard)
	if task, err := r.store.VoiceTask("evt-1"); err != nil || task.AnswerWritten != r7Answer || task.State != state || task.SpokenAt != nil {
		r.t.Fatalf("task = %+v, %v; want %s with the written answer, unspoken", task, err, state)
	}
}

// r7Voice opens sessions that record each Say, with its mode and time.
type r7Voice struct {
	mu       sync.Mutex
	sessions []*r7Session
	says     []r7Said
}

type r7Said struct {
	text string
	mode voice.SayMode
	at   time.Time
}

func (v *r7Voice) Open(context.Context, *voice.Exchange) (voice.Session, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := &r7Session{v: v, events: make(chan voice.Event)}
	v.sessions = append(v.sessions, s)
	return s, nil
}

// counts reports how many sessions opened, and how many of them closed.
func (v *r7Voice) counts() (opened, closed int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, s := range v.sessions {
		if s.closed {
			closed++
		}
	}
	return len(v.sessions), closed
}

func (v *r7Voice) said() []r7Said {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.says)
}

// heldAnswers is what was said as a holder's answer, or with its text.
func (v *r7Voice) heldAnswers() []r7Said {
	return slices.DeleteFunc(v.said(), func(s r7Said) bool { return s.mode != voice.Answer && s.text != r7Spoken })
}

type r7Session struct {
	v      *r7Voice
	events chan voice.Event
	once   sync.Once
	closed bool // guarded by v.mu
}

func (s *r7Session) Say(ctx context.Context, text string, mode voice.SayMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.v.mu.Lock()
	defer s.v.mu.Unlock()
	s.v.says = append(s.v.says, r7Said{text: text, mode: mode, at: time.Now()})
	return nil
}

func (*r7Session) Deliver(context.Context, string, json.RawMessage) error { return nil }

func (*r7Session) Interrupt() {}

func (s *r7Session) Events() <-chan voice.Event { return s.events }

func (s *r7Session) Close() error {
	s.once.Do(func() {
		s.v.mu.Lock()
		s.closed = true
		s.v.mu.Unlock()
		close(s.events)
	})
	return nil
}

// r7Speaker is the speaker output. It records each Begin's input and, for
// each playback, its first frame, when that was written, and how many frames
// were. A Cancel cuts the input's playbacks.
type r7Speaker struct {
	mu        sync.Mutex
	playbacks []*r7Playback
}

type r7Playback struct {
	s         *r7Speaker
	input     string
	first     []byte
	firstAt   time.Time
	frames    int
	cancelled bool
}

var errR7Cancelled = errors.New("r7 speaker: the playback was cancelled")

func (s *r7Speaker) Begin(input string) voice.Playback {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &r7Playback{s: s, input: input}
	s.playbacks = append(s.playbacks, p)
	return p
}

func (s *r7Speaker) Cancel(input string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.playbacks {
		if p.input == input {
			p.cancelled = true
		}
	}
}

func (*r7Speaker) OnSpeech(func(audio.SpeechEvent)) func() { return func() {} }

// sounds returns when each sound began on the chime input whose first frame
// opens pcm.
func (s *r7Speaker) sounds(pcm []byte) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var at []time.Time
	for _, p := range s.playbacks {
		if p.input == audio.InputChime && p.frames > 0 && bytes.HasPrefix(pcm, p.first) {
			at = append(at, p.firstAt)
		}
	}
	return at
}

// soundsSince counts the sounds of pcm that began at since or later.
func (s *r7Speaker) soundsSince(pcm []byte, since time.Time) int {
	n := 0
	for _, at := range s.sounds(pcm) {
		if !at.Before(since) {
			n++
		}
	}
	return n
}

func (p *r7Playback) Write(ctx context.Context, frame []byte, _ audio.AudioFormat) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.cancelled {
		return errR7Cancelled
	}
	if p.frames == 0 {
		p.first, p.firstAt = bytes.Clone(frame), time.Now()
	}
	p.frames++
	return nil
}

func (*r7Playback) Drain(ctx context.Context) error { return ctx.Err() }

// r7Until waits up to r7Deadline for cond to hold.
func r7Until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(r7Deadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", r7Deadline, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// r7Never fails with what if cond holds within d.
func r7Never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
