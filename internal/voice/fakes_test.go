package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

const testWait = 2 * time.Second

var errFakeCancelled = errors.New("fake playback cancelled")

// fakeSpeaker is the speaker output: it records what each input plays and
// each Cancel, and reports speech on each input as audio.Output does.
type fakeSpeaker struct {
	mu        sync.Mutex
	writes    map[string]int       // frames written per input
	cancels   map[string]time.Time // last Cancel per input
	speaking  map[string]bool      // an input's speech is under way
	gen       map[string]int       // bumps on each Cancel, ending its playbacks
	finished  map[string]chan struct{}
	listeners map[int]func(audio.SpeechEvent)
	nextID    int
	began     chan string              // each Begin's input
	hold      map[string]chan struct{} // a Begin on this input waits until it closes
	events    chan audio.SpeechEvent
	// cancelDelay holds each Cancel of an input back, as a late cancel does.
	cancelDelay map[string]time.Duration
	writePace   map[string]time.Duration // each Write on an input takes this long
	chimes      []*fakePlayback          // each playback begun on the chime input
}

func newFakeSpeaker() *fakeSpeaker {
	s := &fakeSpeaker{
		writes:    map[string]int{},
		cancels:   map[string]time.Time{},
		speaking:  map[string]bool{},
		gen:       map[string]int{},
		finished:  map[string]chan struct{}{},
		listeners: map[int]func(audio.SpeechEvent){},
		began:     make(chan string, 64),
		events:    make(chan audio.SpeechEvent, 256),
	}
	// Speech events reach listeners in order on a goroutine of the speaker's,
	// as audio.Output sends them: never inside Cancel or Write.
	go func() {
		for e := range s.events {
			s.mu.Lock()
			fns := make([]func(audio.SpeechEvent), 0, len(s.listeners))
			for _, fn := range s.listeners {
				fns = append(fns, fn)
			}
			s.mu.Unlock()
			for _, fn := range fns {
				fn(e)
			}
		}
	}()
	return s
}

func (s *fakeSpeaker) Begin(input string) Playback {
	s.mu.Lock()
	hold := s.hold[input]
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished[input] == nil {
		s.finished[input] = make(chan struct{})
	}
	select {
	case s.began <- input:
	default:
	}
	pb := &fakePlayback{s: s, input: input, gen: s.gen[input]}
	if input == audio.InputChime {
		s.chimes = append(s.chimes, pb)
	}
	return pb
}

// chimePlaybacks returns the playbacks begun on the chime input that wrote a
// frame, with their sound names and frame counts.
func (s *fakeSpeaker) chimePlaybacks() (names []string, frames []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pb := range s.chimes {
		if pb.first != nil {
			names = append(names, soundName(pb.first))
			frames = append(frames, pb.n)
		}
	}
	return names, frames
}

// soundsPlayed names each sound begun on the chime input that wrote a frame,
// in order, by its first frame.
func (s *fakeSpeaker) soundsPlayed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, pb := range s.chimes {
		if pb.first != nil {
			names = append(names, soundName(pb.first))
		}
	}
	return names
}

func soundName(first []byte) string {
	for name, pcm := range map[string][]byte{"wake": WakeChime(), "answer": AnswerChime(), "start": StartChime(), "error": ErrorChime()} {
		if len(pcm) >= len(first) && bytes.Equal(pcm[:len(first)], first) {
			return name
		}
	}
	return "unknown"
}

// holdBegins makes every Begin on input wait until release is called.
func (s *fakeSpeaker) holdBegins(input string) (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	if s.hold == nil {
		s.hold = map[string]chan struct{}{}
	}
	s.hold[input] = ch
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.hold, input)
		s.mu.Unlock()
		close(ch)
	}
}

func (s *fakeSpeaker) Cancel(input string) {
	s.mu.Lock()
	delay := s.cancelDelay[input]
	s.mu.Unlock()
	time.Sleep(delay)
	s.mu.Lock()
	s.cancels[input] = time.Now()
	s.gen[input]++
	if ch := s.finished[input]; ch != nil {
		close(ch)
		s.finished[input] = nil
	}
	was := s.speaking[input]
	s.speaking[input] = false
	s.mu.Unlock()
	if was {
		s.emit(audio.SpeechEvent{Input: input, Started: false, At: time.Now(), Cancelled: true})
	}
}

func (s *fakeSpeaker) OnSpeech(fn func(audio.SpeechEvent)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners, id)
	}
}

func (s *fakeSpeaker) emit(e audio.SpeechEvent) {
	s.events <- e
}

// paceWrites makes every Write on input take d, as the device plays it.
func (s *fakeSpeaker) paceWrites(input string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writePace == nil {
		s.writePace = map[string]time.Duration{}
	}
	s.writePace[input] = d
}

// delayCancels makes every Cancel of input wait d before it acts, as a cancel
// sent late from another goroutine does.
func (s *fakeSpeaker) delayCancels(input string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelDelay == nil {
		s.cancelDelay = map[string]time.Duration{}
	}
	s.cancelDelay[input] = d
}

// finish plays out what input holds: its playbacks drain, and its speech
// ends.
func (s *fakeSpeaker) finish(input string) {
	s.mu.Lock()
	if ch := s.finished[input]; ch != nil {
		close(ch)
		s.finished[input] = nil
	}
	was := s.speaking[input]
	s.speaking[input] = false
	s.mu.Unlock()
	if was {
		s.emit(audio.SpeechEvent{Input: input, Started: false, At: time.Now()})
	}
}

func (s *fakeSpeaker) cancelledAt(input string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.cancels[input]
	return at, ok
}

func (s *fakeSpeaker) written(input string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[input]
}

type fakePlayback struct {
	s     *fakeSpeaker
	input string
	gen   int
	first []byte // its first frame written
	n     int    // frames written
}

func (p *fakePlayback) Write(ctx context.Context, frame []byte, format audio.AudioFormat) error {
	s := p.s
	s.mu.Lock()
	pace := s.writePace[p.input]
	s.mu.Unlock()
	time.Sleep(pace)
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.gen[p.input] != p.gen {
		s.mu.Unlock()
		return errFakeCancelled
	}
	s.writes[p.input]++
	p.n++
	if p.first == nil {
		p.first = append([]byte(nil), frame...)
	}
	if s.finished[p.input] == nil {
		s.finished[p.input] = make(chan struct{})
	}
	started := !s.speaking[p.input]
	s.speaking[p.input] = true
	s.mu.Unlock()
	if started {
		s.emit(audio.SpeechEvent{Input: p.input, Started: true, At: time.Now()})
	}
	return nil
}

func (p *fakePlayback) Drain(ctx context.Context) error {
	s := p.s
	s.mu.Lock()
	if s.gen[p.input] != p.gen {
		s.mu.Unlock()
		return errFakeCancelled
	}
	ch := s.finished[p.input]
	s.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen[p.input] != p.gen {
		return errFakeCancelled
	}
	return nil
}

// fakeVoice opens fakeSessions that play each Say on the exchange's Out.
type fakeVoice struct {
	mu       sync.Mutex
	sessions []*fakeSession
	opened   chan *fakeSession
	openErr  error
	sayErr   error // every Say of a session opened while set fails with it
}

func newFakeVoice() *fakeVoice { return &fakeVoice{opened: make(chan *fakeSession, 16)} }

func (v *fakeVoice) failOpens(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.openErr = err
}

func (v *fakeVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	v.mu.Lock()
	err := v.openErr
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	s := &fakeSession{x: *x, events: make(chan Event, 64), said: make(chan string, 16), sayErr: v.sayErr}
	v.sessions = append(v.sessions, s)
	v.mu.Unlock()
	v.opened <- s
	return s, nil
}

func (v *fakeVoice) failSays(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sayErr = err
}

type fakeSession struct {
	x      Exchange
	events chan Event
	said   chan string

	mu          sync.Mutex
	interrupted time.Time
	closedAt    time.Time
	closed      bool
	eventsDone  bool
	says        []string
	modes       []SayMode
	sayErr      error
}

func (s *fakeSession) Say(ctx context.Context, text string, mode SayMode) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.sayErr != nil {
		s.mu.Unlock()
		return s.sayErr
	}
	s.says = append(s.says, text)
	s.modes = append(s.modes, mode)
	s.mu.Unlock()
	s.send(EventAgentText, text)
	pb := s.x.Out.Begin()
	if err := pb.Write(ctx, make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
		return err
	}
	s.send(EventSpeechStarted, "")
	s.said <- text
	go func() {
		_ = pb.Drain(context.Background())
		s.send(EventSpeechEnded, "")
	}()
	return nil
}

func (s *fakeSession) send(kind EventKind, text string) {
	e := Event{Kind: kind, Text: text}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventsDone {
		return
	}
	select {
	case s.events <- e:
	default:
	}
}

func (s *fakeSession) Deliver(context.Context, string, json.RawMessage) error { return nil }

func (s *fakeSession) Interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interrupted.IsZero() {
		s.interrupted = time.Now()
	}
}

func (s *fakeSession) Events() <-chan Event { return s.events }

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.closedAt = time.Now()
		s.endEventsLocked()
	}
	return nil
}

func (s *fakeSession) endEventsLocked() {
	if !s.eventsDone {
		s.eventsDone = true
		close(s.events)
	}
}

// fail reports a failure, as a vendor whose connection dropped does.
func (s *fakeSession) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.eventsDone {
		s.events <- Event{Kind: EventErr, Err: err}
	}
}

// die ends the session's events with no Close, as a voice that ended by
// itself does.
func (s *fakeSession) die() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endEventsLocked()
}

func (s *fakeSession) state() (interrupted, closedAt time.Time, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupted, s.closedAt, s.closed
}

// saysCopy returns the texts the session was asked to say, in order.
func (s *fakeSession) saysCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.says...)
}

// fakeConcierge answers each turn with its lines, or holds the turn until
// its context ends when hold is set.
type fakeConcierge struct {
	lines   []string
	hold    bool
	answers chan PendingAnswer

	mu        sync.Mutex
	turns     []string
	exchanges []Exchange
	cancelled chan time.Time
	spoken    []string
	asked     chan string
	heard     []heardCall
	// answerErr is what each Answer returns once its lines are said.
	answerErr  error
	neverMinds []string
	// acks are said in Ack mode before the lines, which wait for afterAcks
	// when it is set.
	acks      []string
	afterAcks chan struct{}
	// toolsUnreachable makes every tool answer as with Dispatch out of reach.
	toolsUnreachable bool
}

func (c *fakeConcierge) Heard(ctx context.Context, x *Exchange, words string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heard = append(c.heard, heardCall{ctx: ctx, x: *x, words: words})
}

func (c *fakeConcierge) heardCalls() []heardCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]heardCall(nil), c.heard...)
}

func newFakeConcierge(lines ...string) *fakeConcierge {
	return &fakeConcierge{lines: lines, answers: make(chan PendingAnswer, 8), cancelled: make(chan time.Time, 8), asked: make(chan string, 8)}
}

func (c *fakeConcierge) Tools(x *Exchange) ToolRunner {
	return fakeTools{unreachable: c.toolsUnreachable}
}

func (c *fakeConcierge) Answer(ctx context.Context, x *Exchange, turn string, say SayFunc) error {
	c.mu.Lock()
	c.turns = append(c.turns, turn)
	c.exchanges = append(c.exchanges, *x)
	c.mu.Unlock()
	c.asked <- turn
	for _, a := range c.acks {
		if err := say(ctx, a, Ack); err != nil {
			return err
		}
	}
	if c.afterAcks != nil {
		select {
		case <-c.afterAcks:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, l := range c.lines {
		if err := say(ctx, l, Fact); err != nil {
			c.cancelled <- time.Now()
			return err
		}
	}
	c.mu.Lock()
	hold := c.hold
	c.mu.Unlock()
	if hold {
		<-ctx.Done()
		c.cancelled <- time.Now()
		return ctx.Err()
	}
	return c.answerErr
}

// setHold sets hold while answers may run.
func (c *fakeConcierge) setHold(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold = on
}

func (c *fakeConcierge) Answers() <-chan PendingAnswer { return c.answers }

func (c *fakeConcierge) Spoken(ctx context.Context, taskID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spoken = append(c.spoken, taskID)
	return nil
}

func (c *fakeConcierge) NeverMind(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.neverMinds = append(c.neverMinds, sessionID)
	return nil
}

func (c *fakeConcierge) neverMindCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.neverMinds...)
}

func (c *fakeConcierge) spokenIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.spoken...)
}

// fakeTools answers every tool with {}, or, with unreachable set, as the
// Concierge answers when Dispatch is out of reach: its result and an error
// that wraps ErrUnreachable.
type fakeTools struct{ unreachable bool }

func (t fakeTools) Run(context.Context, ToolCall) (json.RawMessage, error) {
	if t.unreachable {
		return json.RawMessage(`{"error":"I cannot reach Dispatch yet."}`), fmt.Errorf("concierge: %w", ErrUnreachable)
	}
	return json.RawMessage(`{}`), nil
}

// events records the /ws notifications.
type events struct {
	mu   sync.Mutex
	list []string
	ch   chan string
}

func newEvents() *events { return &events{ch: make(chan string, 64)} }

func (e *events) notify(kind, sessionID string) {
	e.mu.Lock()
	e.list = append(e.list, kind+":"+sessionID)
	e.mu.Unlock()
	e.ch <- kind + ":" + sessionID
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

// rig is a Supervisor wired to fakes, recording on a gate that is open with
// closings count 0.
type rig struct {
	t         *testing.T
	s         *Supervisor
	speaker   *fakeSpeaker
	voice     *fakeVoice
	concierge *fakeConcierge
	events    *events
	tap       *Tap

	mu       sync.Mutex
	red      bool
	enabled  bool
	voiceErr error // the next exchanges' voice cannot be built
	closings int
	session  string
	start    float64 // the next transcript's socket time
	timings  []string
}

func newRig(t *testing.T, c *fakeConcierge) *rig {
	t.Helper()
	return newRigWith(t, c, nil)
}

// newRigWith is newRig with change applied to the Supervisor's config.
func newRigWith(t *testing.T, c *fakeConcierge, change func(*SupervisorConfig, *rig)) *rig {
	t.Helper()
	r := &rig{t: t, speaker: newFakeSpeaker(), voice: newFakeVoice(), concierge: c, events: newEvents(), tap: NewTap(), red: true, enabled: true, session: "sess-1"}
	cfg := &SupervisorConfig{
		Voice: func() (Voice, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.voiceErr != nil {
				return nil, r.voiceErr
			}
			return r.voice, nil
		},
		Concierge:   c,
		Speaker:     r.speaker,
		WakePhrases: []string{"Wispr", "Ghost Wispr"},
		Session: func() string {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.session
		},
		Red: func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.red
		},
		Enabled: func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.enabled
		},
		Notify:       r.events.notify,
		Tap:          r.tap,
		TurnWait:     300 * time.Millisecond,
		SilenceWait:  200 * time.Millisecond,
		SoundGap:     100 * time.Millisecond,
		FollowUpWait: 200 * time.Millisecond,
		ReplayWindow: 300 * time.Millisecond,
		Timing:       r.timing,
		Logf:         t.Logf,
	}
	if change != nil {
		change(cfg, r)
	}
	r.s = NewSupervisor(cfg)
	t.Cleanup(r.s.Close)
	return r
}

// timing records the Supervisor's timing events.
func (r *rig) timing(event, sessionID string, at time.Time, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timings = append(r.timings, event)
}

func (r *rig) timingEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.timings...)
}

func (r *rig) setEnabled(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = on
}

func (r *rig) setRed(red bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.red = red
}

// closeGate closes the capture gate, as a pause or a light hold does.
func (r *rig) closeGate() {
	r.mu.Lock()
	r.closings++
	n := r.closings
	r.mu.Unlock()
	r.s.GateClosed(n)
}

// heard feeds a transcript of text, words 0.2 s apart, as Deepgram sends it:
// interim results and the final of one segment share their words' times, and
// the next segment starts later.
func (r *rig) heard(text string, final, speechFinal bool) {
	r.t.Helper()
	r.mu.Lock()
	closings := r.closings
	start := r.start
	if final {
		r.start += 5
	}
	r.mu.Unlock()
	tr := transcriptOf(text, start, final, speechFinal, closings)
	r.s.OnTranscript(&tr)
}

func transcriptOf(text string, start float64, final, speechFinal bool, closings int) Transcript {
	t := Transcript{Text: text, IsFinal: final, SpeechFinal: speechFinal, Closings: closings, GateOpen: true, At: time.Now()}
	for i, w := range strings.Fields(text) {
		ws := start + 0.2*float64(i)
		t.Words = append(t.Words, Word{Text: w, Speaker: 0, Start: ws, End: ws + 0.15})
	}
	return t
}

// ask opens an exchange with a question, and waits for the Concierge to get
// the turn.
func (r *rig) ask(question string) *fakeSession {
	r.t.Helper()
	r.heard("Wispr, "+question, true, true)
	s := r.waitSession()
	r.waitTurn()
	return s
}

func (r *rig) waitSession() *fakeSession {
	r.t.Helper()
	select {
	case s := <-r.voice.opened:
		return s
	case <-time.After(testWait):
		r.t.Fatal("no voice session opened")
		return nil
	}
}

func (r *rig) waitTurn() string {
	r.t.Helper()
	select {
	case turn := <-r.concierge.asked:
		return turn
	case <-time.After(testWait):
		r.t.Fatal("the Concierge got no turn")
		return ""
	}
}

// waitSpeaking waits until the Supervisor has heard that the voice input
// speaks: speech events reach it on the speaker's goroutine, after the write.
func (r *rig) waitSpeaking() {
	r.t.Helper()
	eventually(r.t, "the voice input speaking", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.voiceSpeaking
	})
}

func (r *rig) waitEvent(want string) {
	r.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case got := <-r.events.ch:
			if got == want {
				return
			}
		case <-deadline:
			r.t.Fatalf("no event %q; events %v", want, r.events.all())
		}
	}
}

func waitSaid(t *testing.T, s *fakeSession) string {
	t.Helper()
	select {
	case text := <-s.said:
		return text
	case <-time.After(testWait):
		t.Fatal("the voice said nothing")
		return ""
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// never fails if cond becomes true within d.
func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
