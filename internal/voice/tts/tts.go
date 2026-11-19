// Package tts is voice A: Wispr speaks the Concierge's words through a
// streaming text-to-speech engine (Gemini TTS on Vertex AI, or Deepgram
// Aura-2) into the speaker output's voice input. The Concierge drives the
// turn and voice A only speaks, so a Session ignores the exchange's room
// audio and tools.
package tts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Engine synthesises speech as a stream.
type Engine interface {
	// Name names the engine in errors.
	Name() string
	// Format is the audio Stream emits: PCM16 mono at the engine's rate.
	Format() audio.AudioFormat
	// Stream synthesises text and calls emit with each PCM chunk as it
	// arrives, so the first audio plays before the whole text is
	// synthesised. A chunk may hold any number of bytes. Stream returns once
	// the text is synthesised, emit fails, or ctx ends.
	Stream(ctx context.Context, text string, emit func(pcm []byte) error) error
}

// Connector is an Engine that speaks over a connection it keeps open, as
// Aura-2's WebSocket is. A Session connects when it opens, so its first Say
// does not wait for the dial, streams every Say over that connection, and
// closes it on Close.
type Connector interface {
	Engine
	Connect(ctx context.Context) (Conn, error)
}

// Conn is one Session's connection to a Connector's engine.
type Conn interface {
	// Stream is Engine.Stream over this connection. A Session never runs
	// two at once, but may Close the Conn while one runs, which ends it.
	Stream(ctx context.Context, text string, emit func(pcm []byte) error) error
	Close() error
}

var (
	// ErrInterrupted is what a Say returns when Interrupt stopped it, in
	// flight or queued.
	ErrInterrupted = errors.New("tts: speech interrupted")
	// ErrClosed is what a Say returns once its Session is closed.
	ErrClosed = errors.New("tts: session closed")
)

// eventBuffer is how many events a Session holds for a slow reader before it
// drops them.
const eventBuffer = 64

// New returns voice A speaking through e.
func New(e Engine) voice.Voice { return voiceA{engine: e} }

type voiceA struct{ engine Engine }

// Open opens a Session that speaks into x.Out. When the engine is a
// Connector, it connects first.
func (v voiceA) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	if x == nil || x.Out == nil {
		return nil, errors.New("tts: the exchange has no output")
	}
	format := v.engine.Format()
	sample := 2 * format.Channels
	if format.Encoding != audio.EncodingPCM16 || format.SampleRate <= 0 || format.Channels <= 0 {
		return nil, fmt.Errorf("tts: %s speaks %s at %d Hz with %d channels, want PCM16", v.engine.Name(), format.Encoding, format.SampleRate, format.Channels)
	}
	frame := format.SampleRate * int(audio.MaxWriteFrame/time.Millisecond) / 1000 * sample
	s := &session{
		format: format,
		sample: sample,
		out:    x.Out,
		stream: v.engine.Stream,
		says:   make(chan *sayRequest),
		plays:  make(chan *sayRequest, 1),
		events: make(chan voice.Event, eventBuffer),
		worker: make(chan struct{}),
		player: make(chan struct{}),
		carry:  make([]byte, 0, frame),
	}
	if c, ok := v.engine.(Connector); ok {
		conn, err := c.Connect(ctx)
		if err != nil {
			return nil, fmt.Errorf("tts: connect %s: %w", v.engine.Name(), err)
		}
		s.conn, s.stream = conn, conn.Stream
	}
	s.ctx, s.cancel = context.WithCancelCause(context.Background())
	s.epoch, s.interrupt = context.WithCancelCause(s.ctx)
	go s.run()
	return s, nil
}

// session is one open voice A. Say hands each text to the synthesis stage
// (run), which streams it into the Say's buffer, and the playback stage
// (play) writes each buffer in order and owns the Playback, so a text is
// synthesised while the one before it plays. An Interrupt moves the session
// to a new generation, which ends every Say of the old one at once, its
// audio synthesised ahead included, whatever the engine is doing.
type session struct {
	format audio.AudioFormat
	sample int // bytes in one sample of every channel
	out    voice.Output
	stream func(ctx context.Context, text string, emit func(pcm []byte) error) error
	conn   Conn // nil for an engine that holds no connection

	ctx    context.Context // ends at Close
	cancel context.CancelCauseFunc
	says   chan *sayRequest
	plays  chan *sayRequest // from run to play: one Say may wait for play
	worker chan struct{}    // closed when run returns
	player chan struct{}    // closed when play returns
	events chan voice.Event

	mu           sync.Mutex
	closed       bool
	eventsClosed bool
	dropped      int64
	gen          int             // counts Interrupts; a Say belongs to the generation it was called in
	epoch        context.Context // ends at the next Interrupt
	interrupt    context.CancelCauseFunc
	pending      int            // Says called and not yet finished
	speaking     bool           // a SpeechStarted awaits its SpeechEnded
	draining     int            // drains of this generation's playbacks in progress
	pb           voice.Playback // the worker's playback; written by the worker under mu
	pbGen        int            // the generation pb belongs to

	carry []byte // worker only: the start of a frame not yet written
}

// sayRequest is one Say: its text's audio, as synthesis gives it, waits in
// chunks until play writes it.
type sayRequest struct {
	ctx context.Context // synthesis: ends with the generation (an Interrupt) or the caller's ctx
	// wctx is the context of the request's writes to the playback: a child
	// of the caller's ctx, so a stop (the exchange's ctx ending) refuses the
	// next write synchronously, with no frame of a sentence synthesised
	// ahead reaching the Output; it also ends with the generation.
	wctx   context.Context
	cancel func(error) // ends ctx and wctx, and their ties, once play is done with the Say
	text   string
	gen    int
	done   chan error // the Say's result, once
	ended  sync.Once

	mu       sync.Mutex
	chunks   [][]byte      // synthesised, not yet written
	finished bool          // synthesis ended
	synthErr error         // why synthesis ended, when it failed
	playErr  error         // play's write failure, which stops synthesis
	more     chan struct{} // signalled when chunks grow or synthesis ends
}

// errPlaybackEnded stops a Say's synthesis once play could not write it.
var errPlaybackEnded = errors.New("tts: the playback ended")

// end hands the Say its result, once.
func (r *sayRequest) end(err error) { r.ended.Do(func() { r.done <- err }) }

// release ends the Say's context once play is done with it.
func (r *sayRequest) release() { r.cancel(nil) }

// push keeps a copy of pcm for play, unless play could not write the Say.
func (r *sayRequest) push(pcm []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.playErr != nil {
		return errPlaybackEnded
	}
	r.chunks = append(r.chunks, append([]byte(nil), pcm...))
	r.signal()
	return nil
}

// synthesised marks synthesis ended, with err when it failed.
func (r *sayRequest) synthesised(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished, r.synthErr = true, err
	r.signal()
}

// signal wakes play; r.mu is held.
func (r *sayRequest) signal() {
	select {
	case r.more <- struct{}{}:
	default:
	}
}

// next waits for the Say's next chunk: more is false once synthesis ended
// and every chunk was taken. It returns early when the Say's context ends.
func (r *sayRequest) next() (pcm []byte, more bool, err error) {
	for {
		r.mu.Lock()
		switch {
		case r.ctx.Err() != nil:
			r.mu.Unlock()
			return nil, false, context.Cause(r.ctx)
		case r.wctx.Err() != nil:
			r.mu.Unlock()
			return nil, false, context.Cause(r.wctx)
		case len(r.chunks) > 0:
			pcm, r.chunks = r.chunks[0], r.chunks[1:]
			r.mu.Unlock()
			return pcm, true, nil
		case r.finished:
			err := r.synthErr
			r.mu.Unlock()
			return nil, false, err
		}
		r.mu.Unlock()
		select {
		case <-r.more:
		case <-r.ctx.Done():
			return nil, false, context.Cause(r.ctx)
		case <-r.wctx.Done():
			return nil, false, context.Cause(r.wctx)
		}
	}
}

// failWrite records play's write failure, which stops synthesis; it reports
// whether it was the first failure of the Say.
func (r *sayRequest) failWrite(err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.playErr != nil || r.synthErr != nil {
		return false
	}
	r.playErr = err
	r.chunks = nil
	return true
}

func (r *sayRequest) writeErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.playErr
}

// Say speaks text after every Say called before it, and returns once the
// whole text is synthesised, or synthesis failed: its audio then plays after
// every Say before it, so the caller's next Say is synthesised while this one
// plays. A text of no words speaks nothing.
//
// Voice A speaks every SayMode in the same voice and style, by design: the
// room hears one consistent voice, and a style prompt per mode would add
// latency before the first audio. The mode is for the caller's log.
func (s *session) Say(ctx context.Context, text string, _ voice.SayMode) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		// A stop came first: nothing of the text is queued, nor sent.
		return context.Cause(ctx)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	epoch, gen := s.epoch, s.gen
	s.pending++
	s.mu.Unlock()

	// The request's synthesis ends with its generation, synchronously, so an
	// Interrupt reaches it, and its audio synthesised ahead, at once. Its
	// writes end with the caller's ctx, the exchange's, synchronously, until
	// play is done with it: a stop cuts the text synthesised ahead though its
	// Say has returned, with no frame queued after it. Each also ends, through
	// an AfterFunc, with the other's parent.
	rctx, cancel := context.WithCancelCause(epoch)
	stopR := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	wctx, cancelW := context.WithCancelCause(ctx)
	stopW := context.AfterFunc(epoch, func() { cancelW(context.Cause(epoch)) })
	release := func(err error) {
		stopR()
		stopW()
		cancel(err)
		cancelW(err)
	}

	r := &sayRequest{ctx: rctx, wctx: wctx, cancel: release, text: text, gen: gen, done: make(chan error, 1), more: make(chan struct{}, 1)}
	select {
	case s.says <- r:
	case <-rctx.Done():
		release(nil)
		s.mu.Lock()
		s.pending--
		s.settleLocked()
		s.mu.Unlock()
		return context.Cause(rctx)
	}
	select {
	case err := <-r.done:
		return err
	case <-rctx.Done():
		// play releases the request once it is done with it, which may come
		// as its result does: the result wins.
		select {
		case err := <-r.done:
			return err
		default:
		}
		// The stages finish the request when the engine lets it go.
		return context.Cause(rctx)
	}
}

// Deliver takes no late tool results: the Concierge drives voice A's turn.
func (s *session) Deliver(context.Context, string, json.RawMessage) error { return nil }

// ReportsTextAudio: voice A reports EventTextAudio at each text's first
// frame, since its Say returns before the text plays.
func (*session) ReportsTextAudio() bool { return true }

var _ voice.TextAudioReporter = (*session)(nil)

// Interrupt stops the Say in flight and every Say queued, and ends the
// speech, without waiting for the engine or the Output. The next Say begins
// a fresh Playback.
func (s *session) Interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.interruptLocked()
	}
}

func (s *session) interruptLocked() {
	s.interrupt(ErrInterrupted)
	s.gen++
	s.epoch, s.interrupt = context.WithCancelCause(s.ctx)
	s.draining = 0
	if s.speaking {
		s.speaking = false
		s.emitLocked(&voice.Event{Kind: voice.EventSpeechEnded})
	}
}

// Events reports what the Session does. It holds eventBuffer events; a reader
// slower than that loses the newest, which DroppedEvents counts.
func (s *session) Events() <-chan voice.Event { return s.events }

// DroppedEvents reports how many events Events had no room for.
func (s *session) DroppedEvents() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close interrupts, releases the engine's connection, and closes Events.
func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.interruptLocked()
	s.closed = true
	s.cancel(ErrClosed)
	s.mu.Unlock()

	var err error
	if s.conn != nil {
		// Closing the connection ends a Stream that waits on it.
		if cerr := s.conn.Close(); cerr != nil {
			err = fmt.Errorf("tts: close the engine connection: %w", cerr)
		}
	}
	<-s.worker
	<-s.player
	s.mu.Lock()
	s.eventsClosed = true
	close(s.events)
	s.mu.Unlock()
	return err
}

func (s *session) emit(e *voice.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitLocked(e)
}

func (s *session) emitLocked(e *voice.Event) {
	if s.eventsClosed {
		return
	}
	select {
	case s.events <- *e:
	default:
		s.dropped++
	}
}

// settleLocked ends the speech once nothing more of it can play: no Say
// pending, no playback open, and no drain in progress.
func (s *session) settleLocked() {
	if s.speaking && s.pending == 0 && s.draining == 0 && (s.pb == nil || s.pbGen != s.gen) {
		s.speaking = false
		s.emitLocked(&voice.Event{Kind: voice.EventSpeechEnded})
	}
}

// run is the synthesis stage: it streams each Say's text, in order, into the
// Say's buffer, while play writes the Say before it. It hands a Say to play
// before it streams it, so a Say's first audio plays as soon as it comes, and
// it streams at most one Say ahead of the one playing.
func (s *session) run() {
	defer close(s.worker)
	go s.play()
	for {
		select {
		case r := <-s.says:
			select {
			case s.plays <- r:
			case <-s.ctx.Done():
				r.end(context.Cause(s.ctx))
				return
			}
			s.synthesise(r)
		case <-s.ctx.Done():
			return
		}
	}
}

// synthesise streams one Say into its buffer. Its Say returns once the whole
// text is synthesised, so the caller says the next one while this one plays.
func (s *session) synthesise(r *sayRequest) {
	if r.ctx.Err() != nil {
		r.end(context.Cause(r.ctx))
		return
	}
	s.emit(&voice.Event{Kind: voice.EventAgentText, Text: r.text})
	err := s.stream(r.ctx, r.text, func(pcm []byte) error {
		return r.push(pcm)
	})
	if err != nil && r.ctx.Err() == nil && !errors.Is(err, errPlaybackEnded) {
		s.emit(&voice.Event{Kind: voice.EventErr, Err: err})
	}
	if err != nil && r.ctx.Err() != nil {
		err = context.Cause(r.ctx)
	}
	if errors.Is(err, errPlaybackEnded) || (err == nil && r.writeErr() != nil) {
		// play could not write the Say: its result is play's failure.
		err = r.writeErr()
	}
	r.end(err)
	r.synthesised(err)
}

// play is the playback stage: it writes each Say's audio, in order, as its
// synthesis gives it, and owns the Playback.
func (s *session) play() {
	defer close(s.player)
	for {
		select {
		case r := <-s.plays:
			s.finish(r, s.speak(r))
		case <-s.ctx.Done():
			return
		}
	}
}

// speak writes one Say's audio into the playback.
func (s *session) speak(r *sayRequest) error {
	first := true
	for {
		pcm, more, err := r.next()
		if err != nil || !more {
			if err == nil {
				// The last frame of a text may be short; a byte that is
				// not a whole sample is not audio.
				if n := len(s.carry) - len(s.carry)%s.sample; n > 0 {
					err = s.writeFrame(r, s.carry[:n], &first)
				}
			}
			s.carry = s.carry[:0]
			if err != nil && !errors.Is(err, ErrInterrupted) && r.ctx.Err() == nil && r.wctx.Err() == nil && r.failWrite(err) {
				// The Output ended the playback: the next Say begins another.
				s.mu.Lock()
				s.pb = nil
				s.mu.Unlock()
				s.emit(&voice.Event{Kind: voice.EventErr, Err: err})
			}
			return err
		}
		if err := s.write(r, pcm, &first); err != nil {
			s.carry = s.carry[:0]
			if r.ctx.Err() == nil && r.wctx.Err() == nil && !errors.Is(err, ErrInterrupted) && r.failWrite(err) {
				s.mu.Lock()
				s.pb = nil
				s.mu.Unlock()
				s.emit(&voice.Event{Kind: voice.EventErr, Err: err})
			}
			return err
		}
	}
}

// finish ends a Say's playback stage and, when no Say follows it, drains its
// playback in the background.
func (s *session) finish(r *sayRequest, err error) {
	r.end(err)
	r.release()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	if s.pb != nil && s.pbGen != s.gen {
		s.pb = nil // an Interrupt ended its speech
	}
	if s.pb != nil && s.pending == 0 {
		pb, gen := s.pb, s.pbGen
		s.pb = nil
		s.draining++
		go s.drain(pb, gen)
	}
	s.settleLocked()
}

// drain waits for pb to play, then ends the speech if nothing followed it.
// The Output reports its own failures, so a failed drain only ends it too.
func (s *session) drain(pb voice.Playback, gen int) {
	_ = pb.Drain(s.ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return // an Interrupt already ended this speech
	}
	s.draining--
	s.settleLocked()
}

// write splits pcm into whole frames of at most audio.MaxWriteFrame, carrying
// a partial frame, or an odd byte, to the next chunk.
func (s *session) write(r *sayRequest, pcm []byte, first *bool) error {
	size := cap(s.carry)
	for len(pcm) > 0 {
		if len(s.carry) == 0 && len(pcm) >= size {
			if err := s.writeFrame(r, pcm[:size], first); err != nil {
				return err
			}
			pcm = pcm[size:]
			continue
		}
		n := copy(s.carry[len(s.carry):size], pcm)
		s.carry, pcm = s.carry[:len(s.carry)+n], pcm[n:]
		if len(s.carry) < size {
			return nil
		}
		if err := s.writeFrame(r, s.carry, first); err != nil {
			return err
		}
		s.carry = s.carry[:0]
	}
	return nil
}

// writeFrame queues one frame, beginning a playback, and the speech, at the
// first frame; the first frame of a text reports EventTextAudio.
func (s *session) writeFrame(r *sayRequest, frame []byte, first *bool) error {
	s.mu.Lock()
	if r.gen != s.gen {
		s.mu.Unlock()
		return ErrInterrupted
	}
	if err := r.wctx.Err(); err != nil {
		// A stop ended the request: begin no playback for it.
		s.mu.Unlock()
		return context.Cause(r.wctx)
	}
	if s.pb == nil {
		s.pb, s.pbGen = s.out.Begin(), r.gen
	}
	if !s.speaking {
		s.speaking = true
		s.emitLocked(&voice.Event{Kind: voice.EventSpeechStarted})
	}
	if *first {
		*first = false
		s.emitLocked(&voice.Event{Kind: voice.EventTextAudio, Text: r.text})
	}
	pb := s.pb
	s.mu.Unlock()
	return pb.Write(r.wctx, frame, s.format)
}
