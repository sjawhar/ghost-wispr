package tts

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

var mono24k = audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}

// frame20ms is the most a Write takes at 24 kHz mono: 480 samples.
const frame20ms = 960

// fakeEngine streams whatever its stream function emits, at 24 kHz mono.
type fakeEngine struct {
	stream func(ctx context.Context, text string, emit func(pcm []byte) error) error
}

func (fakeEngine) Name() string              { return "fake" }
func (fakeEngine) Format() audio.AudioFormat { return mono24k }
func (e fakeEngine) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	return e.stream(ctx, text, emit)
}

// speaks is an engine that emits each text's audio in one chunk.
func speaks(pcm map[string][]byte) fakeEngine {
	return fakeEngine{stream: func(_ context.Context, text string, emit func([]byte) error) error {
		return emit(pcm[text])
	}}
}

// fakeConnector is an engine that holds a connection per Session.
type fakeConnector struct {
	fakeEngine
	connects atomic.Int32
	closes   atomic.Int32
}

func (c *fakeConnector) Connect(context.Context) (Conn, error) {
	c.connects.Add(1)
	return fakeConn{c}, nil
}

type fakeConn struct{ c *fakeConnector }

func (f fakeConn) Stream(ctx context.Context, text string, emit func(pcm []byte) error) error {
	return f.c.stream(ctx, text, emit)
}

func (f fakeConn) Close() error {
	f.c.closes.Add(1)
	return nil
}

// fakeOutput is the speaker output's voice input as a test sees it: it keeps
// every frame each Playback was given.
type fakeOutput struct {
	mu        sync.Mutex
	playbacks []*fakePlayback
	drainGate chan struct{} // when set, Drain waits for it to close
	first     chan struct{} // closed at the first Write
	firstOnce sync.Once
}

func newFakeOutput() *fakeOutput { return &fakeOutput{first: make(chan struct{})} }

func (o *fakeOutput) Begin() voice.Playback {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := &fakePlayback{out: o, cancelled: make(chan struct{}), drainGate: o.drainGate}
	o.playbacks = append(o.playbacks, p)
	return p
}

func (o *fakeOutput) begun() []*fakePlayback {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.playbacks)
}

// waitFirstWrite waits for the first frame any Playback was given.
func (o *fakeOutput) waitFirstWrite(t *testing.T) {
	t.Helper()
	select {
	case <-o.first:
	case <-time.After(2 * time.Second):
		t.Fatal("no frame written in 2 s")
	}
}

type fakePlayback struct {
	out        *fakeOutput
	mu         sync.Mutex
	frames     [][]byte
	formats    []audio.AudioFormat
	drains     int
	drainGate  chan struct{}
	cancelled  chan struct{}
	cancelOnce sync.Once
}

func (p *fakePlayback) Write(ctx context.Context, frame []byte, format audio.AudioFormat) error {
	select {
	case <-p.cancelled:
		return audio.ErrCancelled
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.frames = append(p.frames, bytes.Clone(frame))
	p.formats = append(p.formats, format)
	p.mu.Unlock()
	p.out.firstOnce.Do(func() { close(p.out.first) })
	return nil
}

func (p *fakePlayback) Drain(ctx context.Context) error {
	p.mu.Lock()
	p.drains++
	p.mu.Unlock()
	if p.drainGate != nil {
		select {
		case <-p.drainGate:
		case <-p.cancelled:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-p.cancelled:
		return audio.ErrCancelled
	default:
		return nil
	}
}

// cancel is the Supervisor cancelling the voice input under this playback.
func (p *fakePlayback) cancel() { p.cancelOnce.Do(func() { close(p.cancelled) }) }

func (p *fakePlayback) written() (frames [][]byte, formats []audio.AudioFormat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.frames), slices.Clone(p.formats)
}

func (p *fakePlayback) audio() []byte {
	frames, _ := p.written()
	return bytes.Join(frames, nil)
}

// waitPlayback waits until the Output began want playbacks and the last one
// got n bytes, since a Say returns once its first frame is queued, before
// its audio is all written.
func (o *fakeOutput) waitPlayback(t *testing.T, want, n int) []*fakePlayback {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pbs := o.begun()
		if len(pbs) >= want && len(pbs[want-1].audio()) >= n {
			return pbs
		}
		if time.Now().After(deadline) {
			return pbs
		}
		time.Sleep(time.Millisecond)
	}
}

// ramp is n bytes counting up from start, so the order of audio shows.
func ramp(start, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(start + i)
	}
	return b
}

func openSession(t *testing.T, e Engine, out voice.Output) voice.Session {
	t.Helper()
	s, err := New(e).Open(context.Background(), &voice.Exchange{Out: out})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var kindNames = map[voice.EventKind]string{
	voice.EventSpeechStarted: "SpeechStarted",
	voice.EventSpeechEnded:   "SpeechEnded",
	voice.EventUserTurn:      "UserTurn",
	voice.EventToolCall:      "ToolCall",
	voice.EventErr:           "Err",
	voice.EventAgentText:     "AgentText",
	voice.EventTextAudio:     "TextAudio",
}

func nextEvent(t *testing.T, events <-chan voice.Event) voice.Event {
	t.Helper()
	select {
	case e, ok := <-events:
		if !ok {
			t.Fatal("Events closed early")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event in 2 s")
	}
	return voice.Event{}
}

func expectEvents(t *testing.T, events <-chan voice.Event, want ...voice.EventKind) []voice.Event {
	t.Helper()
	got := make([]voice.Event, 0, len(want))
	for i, kind := range want {
		e := nextEvent(t, events)
		got = append(got, e)
		if e.Kind != kind {
			t.Fatalf("event %d = %s %+v, want %s", i+1, kindNames[e.Kind], e, kindNames[kind])
		}
	}
	return got
}

func expectNoEvent(t *testing.T, events <-chan voice.Event, within time.Duration) {
	t.Helper()
	select {
	case e := <-events:
		t.Fatalf("event %s %+v, want none yet", kindNames[e.Kind], e)
	case <-time.After(within):
	}
}

// TestSayWritesWholeFramesOfAtMost20msInOrder fails if a Say hands the Output
// a frame longer than 20 ms, a frame that splits a sample, audio out of
// order, or less than all of it: the engine's chunks have any length, odd
// ones included.
func TestSayWritesWholeFramesOfAtMost20msInOrder(t *testing.T) {
	var chunks [][]byte
	n := 0
	for _, size := range []int{1, 959, 961, 3, 1500, 7, 1} {
		chunks = append(chunks, ramp(n, size))
		n += size
	}
	want := bytes.Join(chunks, nil)
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(_ context.Context, _ string, emit func([]byte) error) error {
		for _, c := range chunks {
			if err := emit(c); err != nil {
				return err
			}
		}
		return nil
	}}, out)

	if err := s.Say(context.Background(), "Twelve open issues.", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	pbs := out.waitPlayback(t, 1, len(want))
	if len(pbs) != 1 {
		t.Fatalf("Say began %d playbacks, want 1", len(pbs))
	}
	frames, formats := pbs[0].written()
	for i, f := range frames {
		if len(f) > frame20ms || len(f)%2 != 0 {
			t.Errorf("frame %d is %d bytes, want whole 16-bit samples of at most %d", i, len(f), frame20ms)
		}
		if i < len(frames)-1 && len(f) != frame20ms {
			t.Errorf("frame %d of %d is %d bytes, want only the last short of %d", i, len(frames), len(f), frame20ms)
		}
		if formats[i] != mono24k {
			t.Errorf("frame %d format = %+v, want %+v", i, formats[i], mono24k)
		}
	}
	if got := bytes.Join(frames, nil); !bytes.Equal(got, want) {
		t.Fatalf("wrote %d bytes, want the engine's %d in order", len(got), len(want))
	}
}

// TestSayReportsAgentTextThenSpeechStartedThenSpeechEnded fails if a Say's
// events come in another order, or the text is not the words spoken.
func TestSayReportsAgentTextThenSpeechStartedThenSpeechEnded(t *testing.T) {
	out := newFakeOutput()
	s := openSession(t, speaks(map[string][]byte{"Hello there.": ramp(0, 2000)}), out)

	if err := s.Say(context.Background(), "Hello there.", voice.Ack); err != nil {
		t.Fatalf("Say: %v", err)
	}
	got := expectEvents(t, s.Events(), voice.EventAgentText, voice.EventSpeechStarted, voice.EventTextAudio, voice.EventSpeechEnded)
	if got[0].Text != "Hello there." {
		t.Fatalf("AgentText = %q, want the words spoken", got[0].Text)
	}
}

// TestSayReturnsOnceItsFirstFrameIsQueued fails if a Say waits for its audio
// to play out, or SpeechEnded comes before the playback drained.
func TestSayReturnsOnceItsFirstFrameIsQueued(t *testing.T) {
	out := newFakeOutput()
	out.drainGate = make(chan struct{})
	s := openSession(t, speaks(map[string][]byte{"One moment.": ramp(0, 3000)}), out)

	said := make(chan error, 1)
	go func() { said <- s.Say(context.Background(), "One moment.", voice.Ack) }()
	select {
	case err := <-said:
		if err != nil {
			t.Fatalf("Say: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Say still running 2 s after its first frame was queued: it waits for playback")
	}
	expectEvents(t, s.Events(), voice.EventAgentText, voice.EventSpeechStarted, voice.EventTextAudio)
	expectNoEvent(t, s.Events(), 50*time.Millisecond)
	close(out.drainGate)
	expectEvents(t, s.Events(), voice.EventSpeechEnded)
}

// TestSaysQueuedBehindASpeechPlayInOrderInOnePlayback fails if Says called
// while another speaks play out of order, mix, or end the speech between
// them.
func TestSaysQueuedBehindASpeechPlayInOrderInOnePlayback(t *testing.T) {
	audioOf := map[string][]byte{"first": ramp(0, 1000), "second": ramp(50, 1000), "third": ramp(100, 1000)}
	release := make(chan struct{})
	var mu sync.Mutex
	var streamed []string
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(ctx context.Context, text string, emit func([]byte) error) error {
		mu.Lock()
		streamed = append(streamed, text)
		mu.Unlock()
		if err := emit(audioOf[text]); err != nil {
			return err
		}
		if text == "first" {
			<-release
		}
		return nil
	}}, out)

	said := make(chan error, 3)
	for _, text := range []string{"first", "second", "third"} {
		go func() { said <- s.Say(context.Background(), text, voice.Fact) }()
		time.Sleep(50 * time.Millisecond)
	}
	close(release)
	for range 3 {
		if err := <-said; err != nil {
			t.Fatalf("Say: %v", err)
		}
	}
	mu.Lock()
	order := slices.Clone(streamed)
	mu.Unlock()
	if !slices.Equal(order, []string{"first", "second", "third"}) {
		t.Fatalf("streamed %q, want the Says in the order they were called", order)
	}
	want := bytes.Join([][]byte{audioOf["first"], audioOf["second"], audioOf["third"]}, nil)
	pbs := out.waitPlayback(t, 1, len(want))
	if len(pbs) != 1 {
		t.Fatalf("began %d playbacks, want one speech in one", len(pbs))
	}
	if got := pbs[0].audio(); !bytes.Equal(got, want) {
		t.Fatalf("played %d bytes, want the three texts' %d in order", len(got), len(want))
	}
	// The synthesis stage reports each text as it starts on it, the playback
	// stage each text's first frame: their order is each stage's.
	var texts, plays []string
	starts, ends := 0, 0
	for ends == 0 {
		e := nextEvent(t, s.Events())
		switch e.Kind {
		case voice.EventAgentText:
			texts = append(texts, e.Text)
		case voice.EventTextAudio:
			plays = append(plays, e.Text)
		case voice.EventSpeechStarted:
			starts++
		case voice.EventSpeechEnded:
			ends++
		}
	}
	if order := []string{"first", "second", "third"}; !slices.Equal(texts, order) || !slices.Equal(plays, order) || starts != 1 {
		t.Fatalf("AgentText %q, TextAudio %q and %d SpeechStarted, want each text in order in one speech", texts, plays, starts)
	}
}

// TestInterruptReturnsAnInFlightSayWithin100msAndTheNextSayPlays fails if an
// Interrupt of a text in flight (its Say returned once its first frame was
// queued) takes more than 100 ms, or the Session takes no Say afterwards in
// a fresh playback.
func TestInterruptReturnsAnInFlightSayWithin100msAndTheNextSayPlays(t *testing.T) {
	after := ramp(7, 1200)
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(ctx context.Context, text string, emit func([]byte) error) error {
		if text == "after" {
			return emit(after)
		}
		if err := emit(ramp(0, frame20ms)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}, out)

	if err := s.Say(context.Background(), "a long answer", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	out.waitFirstWrite(t)
	interrupted := time.Now()
	s.Interrupt()
	if took := time.Since(interrupted); took > 100*time.Millisecond {
		t.Fatalf("Interrupt took %v, want within 100 ms", took)
	}

	if err := s.Say(context.Background(), "after", voice.Fact); err != nil {
		t.Fatalf("Say after Interrupt: %v", err)
	}
	pbs := out.waitPlayback(t, 2, len(after))
	if len(pbs) != 2 {
		t.Fatalf("began %d playbacks, want a fresh one after Interrupt", len(pbs))
	}
	if got := pbs[1].audio(); !bytes.Equal(got, after) {
		t.Fatalf("the fresh playback got %d bytes, want the next Say's %d", len(got), len(after))
	}
}

// TestInterruptDoesNotWaitForTheEngine fails if Interrupt, or the Say it
// stops before its first audio, waits for an engine that is slow to see its
// context end, as a stalled network is.
func TestInterruptDoesNotWaitForTheEngine(t *testing.T) {
	release, called := make(chan struct{}), make(chan struct{})
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(_ context.Context, _ string, _ func([]byte) error) error {
		close(called)
		<-release
		return nil
	}}, out)
	t.Cleanup(func() { close(release) })

	said := make(chan error, 1)
	go func() { said <- s.Say(context.Background(), "stuck", voice.Fact) }()
	<-called
	interrupted := time.Now()
	s.Interrupt()
	if took := time.Since(interrupted); took > 10*time.Millisecond {
		t.Fatalf("Interrupt took %v, want it to return at once", took)
	}
	select {
	case err := <-said:
		if took := time.Since(interrupted); took > 100*time.Millisecond {
			t.Fatalf("Say returned %v after Interrupt, want within 100 ms", took)
		}
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("interrupted Say = %v, want ErrInterrupted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Say still running 1 s after Interrupt while the engine stalls")
	}
}

// TestInterruptCancelsQueuedSays fails if a Say queued behind the one
// Interrupt stopped still speaks.
func TestInterruptCancelsQueuedSays(t *testing.T) {
	var mu sync.Mutex
	var streamed []string
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(ctx context.Context, text string, emit func([]byte) error) error {
		mu.Lock()
		streamed = append(streamed, text)
		mu.Unlock()
		if err := emit(ramp(0, frame20ms)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}, out)

	// The Say in flight returned once its first frame was queued; the one
	// queued behind it waits for the synthesis stage, and Interrupt ends it.
	first, queued := make(chan error, 1), make(chan error, 1)
	go func() { first <- s.Say(context.Background(), "first", voice.Fact) }()
	out.waitFirstWrite(t)
	if err := <-first; err != nil {
		t.Fatalf("the Say in flight: %v", err)
	}
	go func() { queued <- s.Say(context.Background(), "queued", voice.Fact) }()
	time.Sleep(50 * time.Millisecond)
	s.Interrupt()
	select {
	case err := <-queued:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("queued Say = %v, want ErrInterrupted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued Say still running 1 s after Interrupt")
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(streamed, []string{"first"}) {
		t.Fatalf("streamed %q, want only the Say in flight", streamed)
	}
}

// TestInterruptEndsTheSpeech fails if Interrupt leaves the speech without its
// SpeechEnded, or the next Say does not start a new one.
func TestInterruptEndsTheSpeech(t *testing.T) {
	out := newFakeOutput()
	out.drainGate = make(chan struct{})
	s := openSession(t, speaks(map[string][]byte{"one": ramp(0, 2000), "two": ramp(9, 2000)}), out)

	if err := s.Say(context.Background(), "one", voice.Fact); err != nil {
		t.Fatalf("Say: %v", err)
	}
	expectEvents(t, s.Events(), voice.EventAgentText, voice.EventSpeechStarted, voice.EventTextAudio)
	s.Interrupt()
	expectEvents(t, s.Events(), voice.EventSpeechEnded)
	if err := s.Say(context.Background(), "two", voice.Fact); err != nil {
		t.Fatalf("Say after Interrupt: %v", err)
	}
	expectEvents(t, s.Events(), voice.EventAgentText, voice.EventSpeechStarted, voice.EventTextAudio)
	close(out.drainGate)
	expectEvents(t, s.Events(), voice.EventSpeechEnded)
	expectNoEvent(t, s.Events(), 50*time.Millisecond)
}

// TestEngineErrorSurfacesAsSayErrorAndEventErr fails if a synthesis failure
// is not the Say's error or is not reported on Events.
func TestEngineErrorSurfacesAsSayErrorAndEventErr(t *testing.T) {
	failed := errors.New("synthesis failed")
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(context.Context, string, func([]byte) error) error {
		return failed
	}}, out)

	if err := s.Say(context.Background(), "Twelve open issues.", voice.Fact); !errors.Is(err, failed) {
		t.Fatalf("Say = %v, want the engine's error", err)
	}
	got := expectEvents(t, s.Events(), voice.EventAgentText, voice.EventErr)
	if !errors.Is(got[1].Err, failed) {
		t.Fatalf("EventErr = %v, want the engine's error", got[1].Err)
	}
}

// TestAFailedWriteIsReportedAndTheNextSayBeginsAnotherPlayback fails if a
// write the Output refused, once it ended the playback, is not reported (as
// the Say's error when it came before the Say's synthesis ended, and as
// EventErr), or a Say queued behind it writes to the ended playback instead
// of a fresh one.
func TestAFailedWriteIsReportedAndTheNextSayBeginsAnotherPlayback(t *testing.T) {
	next := ramp(3, 2000)
	queued := make(chan struct{})
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(_ context.Context, text string, emit func([]byte) error) error {
		if text == "next" {
			return emit(next)
		}
		if err := emit(ramp(0, frame20ms)); err != nil {
			return err
		}
		<-queued
		out.begun()[0].cancel()
		return emit(ramp(0, frame20ms))
	}}, out)

	failed, nextSaid := make(chan error, 1), make(chan error, 1)
	go func() { failed <- s.Say(context.Background(), "ended under it", voice.Fact) }()
	out.waitFirstWrite(t)
	go func() { nextSaid <- s.Say(context.Background(), "next", voice.Fact) }()
	time.Sleep(50 * time.Millisecond)
	close(queued)
	// The Say returned at its first frame: the write failure after it is
	// reported as EventErr only.
	if err := <-failed; err != nil {
		t.Fatalf("Say whose playback ended after its first frame = %v, want nil", err)
	}
	if err := <-nextSaid; err != nil {
		t.Fatalf("the Say queued behind it: %v", err)
	}
	pbs := out.waitPlayback(t, 2, len(next))
	if len(pbs) != 2 {
		t.Fatalf("began %d playbacks, want a fresh one after the first ended", len(pbs))
	}
	if got := pbs[1].audio(); !bytes.Equal(got, next) {
		t.Fatalf("the fresh playback got %d bytes, want the queued Say's %d", len(got), len(next))
	}
	for {
		e := nextEvent(t, s.Events())
		if e.Kind == voice.EventErr {
			if !errors.Is(e.Err, audio.ErrCancelled) {
				t.Fatalf("EventErr = %v, want the Output's ErrCancelled", e.Err)
			}
			return
		}
	}
}

// refusingOutput is an Output whose every playback refuses each write, as
// the Supervisor's playback begun after its exchange ended does.
type refusingOutput struct{}

var errRefused = errors.New("the exchange ended")

func (refusingOutput) Begin() voice.Playback                                  { return refusingOutput{} }
func (refusingOutput) Write(context.Context, []byte, audio.AudioFormat) error { return errRefused }
func (refusingOutput) Drain(context.Context) error                            { return nil }

// TestNoTextAudioForAFrameTheOutputRefused fails if voice A reports a text's
// first audio (EventTextAudio, which the Supervisor logs as fact_audio for
// R6's timing) for a frame the Output refused: none of the text played.
func TestNoTextAudioForAFrameTheOutputRefused(t *testing.T) {
	s := openSession(t, speaks(map[string][]byte{"the fact": ramp(0, 2*frame20ms)}), refusingOutput{})
	if err := s.Say(context.Background(), "the fact", voice.Fact); !errors.Is(err, errRefused) {
		t.Fatalf("Say = %v, want the Output's refusal", err)
	}
	for {
		switch e := nextEvent(t, s.Events()); e.Kind {
		case voice.EventTextAudio:
			t.Fatalf("EventTextAudio %q for a frame the Output refused", e.Text)
		case voice.EventSpeechEnded:
			return
		}
	}
}

// TestCloseClosesEvents fails if Events stays open after Close, or the
// Session takes a Say once closed.
func TestCloseClosesEvents(t *testing.T) {
	out := newFakeOutput()
	s := openSession(t, speaks(map[string][]byte{"hello": ramp(0, 1000)}), out)
	if err := s.Say(context.Background(), "hello", voice.Ack); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-s.Events():
		case <-deadline:
			t.Fatal("Events still open 2 s after Close")
		}
	}
	if err := s.Say(context.Background(), "hello", voice.Ack); !errors.Is(err, ErrClosed) {
		t.Fatalf("Say after Close = %v, want ErrClosed", err)
	}
}

// TestASessionHoldsOneEngineConnectionFromOpenToClose fails if a Session of
// an engine that keeps a connection does not connect when it opens, so its
// first Say pays the dial, or does not release the connection on Close.
func TestASessionHoldsOneEngineConnectionFromOpenToClose(t *testing.T) {
	engine := &fakeConnector{fakeEngine: speaks(map[string][]byte{"hello": ramp(0, 1000)})}
	s, err := New(engine).Open(context.Background(), &voice.Exchange{Out: newFakeOutput()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n := engine.connects.Load(); n != 1 {
		t.Fatalf("Open made %d connections, want 1 before the first Say", n)
	}
	if err := s.Say(context.Background(), "hello", voice.Ack); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c, n := engine.connects.Load(), engine.closes.Load(); c != 1 || n != 1 {
		t.Fatalf("connected %d times and closed %d, want one connection, closed once", c, n)
	}
}

// TestEventsNeverBlockTheSessionOnASlowReader fails if a Session waits for a
// reader of Events, or does not count the events it dropped.
func TestEventsNeverBlockTheSessionOnASlowReader(t *testing.T) {
	out := newFakeOutput()
	s := openSession(t, speaks(map[string][]byte{"tick": ramp(0, 100)}), out)
	done := make(chan error, 1)
	go func() {
		for range 200 {
			if err := s.Say(context.Background(), "tick", voice.Ack); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Say: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("200 Says did not finish in 5 s with nobody reading Events")
	}
	dropped, ok := s.(interface{ DroppedEvents() int64 })
	if !ok {
		t.Fatal("the Session does not report the events it dropped")
	}
	if dropped.DroppedEvents() == 0 {
		t.Fatal("DroppedEvents = 0 after 200 Says nobody read, want the drops counted")
	}
}

// TestEmptyTextSpeaksNothing fails if a Say of no words reaches the engine or
// reports anything.
func TestEmptyTextSpeaksNothing(t *testing.T) {
	var calls atomic.Int32
	out := newFakeOutput()
	s := openSession(t, fakeEngine{stream: func(context.Context, string, func([]byte) error) error {
		calls.Add(1)
		return errors.New("no text")
	}}, out)
	if err := s.Say(context.Background(), " \n", voice.Ack); err != nil {
		t.Fatalf("Say of no words = %v, want nil", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the engine streamed %d times, want none", n)
	}
	expectNoEvent(t, s.Events(), 50*time.Millisecond)
}

// TestDeliverTakesNoLateResults fails if voice A refuses a late tool result:
// it has none to take, and the Supervisor may still hand one over.
func TestDeliverTakesNoLateResults(t *testing.T) {
	s := openSession(t, speaks(nil), newFakeOutput())
	if err := s.Deliver(context.Background(), "call-1", []byte(`{"say":"done"}`)); err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}
}
