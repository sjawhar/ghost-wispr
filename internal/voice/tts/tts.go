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
		events: make(chan voice.Event, eventBuffer),
		worker: make(chan struct{}),
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

// session is one open voice A. Say hands each text to one worker goroutine,
// which streams it, writes its frames and owns the Playback; an Interrupt
// moves the session to a new generation, which ends every Say of the old one
// at once, whatever the engine is doing.
type session struct {
	format audio.AudioFormat
	sample int // bytes in one sample of every channel
	out    voice.Output
	stream func(ctx context.Context, text string, emit func(pcm []byte) error) error
	conn   Conn // nil for an engine that holds no connection

	ctx    context.Context // ends at Close
	cancel context.CancelCauseFunc
	says   chan *sayRequest
	worker chan struct{} // closed when the worker returns
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

type sayRequest struct {
	ctx  context.Context
	text string
	gen  int
	done chan error
}

// Say speaks text after every Say called before it, and returns once its last
// frame is queued to the Output. A text of no words speaks nothing.
//
// Voice A speaks every SayMode in the same voice and style, by design: the
// room hears one consistent voice, and a style prompt per mode would add
// latency before the first audio. The mode is for the caller's log.
func (s *session) Say(ctx context.Context, text string, _ voice.SayMode) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	epoch, gen := s.epoch, s.gen
	s.pending++
	s.mu.Unlock()

	// The request ends with its generation, synchronously, so an Interrupt
	// reaches it at once; the caller's ctx ends it too.
	rctx, cancel := context.WithCancelCause(epoch)
	defer cancel(nil)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	defer stop()

	r := &sayRequest{ctx: rctx, text: text, gen: gen, done: make(chan error, 1)}
	select {
	case s.says <- r:
	case <-rctx.Done():
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
		// The worker finishes the request when the engine lets it go.
		return context.Cause(rctx)
	}
}

// Deliver takes no late tool results: the Concierge drives voice A's turn.
func (s *session) Deliver(context.Context, string, json.RawMessage) error { return nil }

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

func (s *session) run() {
	defer close(s.worker)
	for {
		select {
		case r := <-s.says:
			s.finish(r, s.speak(r))
		case <-s.ctx.Done():
			return
		}
	}
}

// speak streams one Say into the playback.
func (s *session) speak(r *sayRequest) error {
	if r.ctx.Err() != nil {
		return context.Cause(r.ctx)
	}
	s.emit(&voice.Event{Kind: voice.EventAgentText, Text: r.text})
	var writeErr error
	err := s.stream(r.ctx, r.text, func(pcm []byte) error {
		if err := s.write(r, pcm); err != nil {
			writeErr = err
			return err
		}
		return nil
	})
	if err == nil {
		// The last frame of a text may be short; a byte that is not a whole
		// sample is not audio.
		if n := len(s.carry) - len(s.carry)%s.sample; n > 0 {
			err = s.writeFrame(r, s.carry[:n])
			writeErr = err
		}
	}
	s.carry = s.carry[:0]
	if err == nil {
		return nil
	}
	if writeErr != nil {
		// The Output ended the playback: the next Say begins another.
		s.mu.Lock()
		s.pb = nil
		s.mu.Unlock()
	}
	if r.ctx.Err() != nil {
		return context.Cause(r.ctx)
	}
	s.emit(&voice.Event{Kind: voice.EventErr, Err: err})
	return err
}

// finish hands a Say its result and, when no Say follows it, drains its
// playback in the background.
func (s *session) finish(r *sayRequest, err error) {
	r.done <- err
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
func (s *session) write(r *sayRequest, pcm []byte) error {
	size := cap(s.carry)
	for len(pcm) > 0 {
		if len(s.carry) == 0 && len(pcm) >= size {
			if err := s.writeFrame(r, pcm[:size]); err != nil {
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
		if err := s.writeFrame(r, s.carry); err != nil {
			return err
		}
		s.carry = s.carry[:0]
	}
	return nil
}

// writeFrame queues one frame, beginning a playback, and the speech, at the
// first frame.
func (s *session) writeFrame(r *sayRequest, frame []byte) error {
	s.mu.Lock()
	if r.gen != s.gen {
		s.mu.Unlock()
		return ErrInterrupted
	}
	if s.pb == nil {
		s.pb, s.pbGen = s.out.Begin(), r.gen
	}
	if !s.speaking {
		s.speaking = true
		s.emitLocked(&voice.Event{Kind: voice.EventSpeechStarted})
	}
	pb := s.pb
	s.mu.Unlock()
	return pb.Write(r.ctx, frame, s.format)
}
