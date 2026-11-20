package dgagent

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// speechFrame is the most the player writes at once: audio.MaxWriteFrame
// (20 ms) of PCM16 at OutputRate, the Output's limit.
const speechFrame = OutputRate * 2 * int(audio.MaxWriteFrame/time.Millisecond) / 1000

// speechFormat is the voice's speech as the session declares it to
// Deepgram and writes it to the Output.
var speechFormat = audio.AudioFormat{SampleRate: OutputRate, Channels: 1, Encoding: audio.EncodingPCM16}

// itemKind is what a speechItem carries; the zero item is none of these.
type itemKind int

const (
	itemAudio     itemKind = iota + 1
	itemEnd                // Deepgram's AgentAudioDone
	itemIdle               // no audio for the session's speech gap
	itemInterrupt          // Interrupt was called
	itemClosed             // the session closed
	itemSaid               // a Say's words were all echoed; their audio follows
)

type speechItem struct {
	kind       itemKind
	pcm        []byte
	say        *sayWait // itemSaid's
	audioFirst bool     // itemSaid's audio was queued before its text
}

type utteranceDecision uint8

const (
	undecided utteranceDecision = iota
	allowed
	dropped
)

// utterance is one Deepgram reply. It owns every decision that can release
// its audio, including the Say that waits for the reply's accepted bytes.
// A completed audio-first utterance stays in awaitingText until its text
// arrives; it never shares state with a later reply.
type utterance struct {
	decision utteranceDecision
	text     bool
	done     bool
	queued   bool
	ended    bool
	audio    [][]byte
	waiter   *sayWait
}

// speechQueue carries the agent's speech from the reader, which must never
// wait on the speaker, to the player.
type speechQueue struct {
	mu           sync.Mutex
	items        []speechItem
	ready        chan struct{} // signalled when items or interrupted change
	current      *utterance    // the reply receiving audio now
	awaitingText []*utterance  // completed audio-first replies, in wire order
	awaitingEnd  []*utterance  // authorized replies superseded before their done
	interrupted  bool          // the player has not seen the last Interrupt
	stop         context.CancelFunc
}

func newSpeechQueue() speechQueue {
	return speechQueue{ready: make(chan struct{}, 1)}
}

func (q *speechQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// audio records one binary message on its own reply. Audio is buffered until
// that reply's text authorizes it, never until some later reply's text.
func (q *speechQueue) audio(pcm []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		q.current = &utterance{}
	}
	q.queueAudioLocked(q.current, pcm)
}

func (q *speechQueue) queueAudioLocked(u *utterance, pcm []byte) {
	switch u.decision {
	case undecided:
		u.audio = append(u.audio, pcm)
	case allowed:
		u.queued = true
		q.items = append(q.items, speechItem{kind: itemAudio, pcm: pcm})
		q.signal()
	}
}

// textCame returns the reply its text belongs to. An audio-first reply that
// already ended wins over a newer reply that has begun: Deepgram's wire order
// does not identify replies, so that completed reply is the oldest text due.
func (q *speechQueue) textCame() *utterance {
	q.mu.Lock()
	defer q.mu.Unlock()
	var u *utterance
	if len(q.awaitingText) > 0 {
		u = q.awaitingText[0]
		q.awaitingText = q.awaitingText[1:]
	} else {
		if q.current == nil {
			q.current = &utterance{}
		}
		u = q.current
	}
	u.text = true
	return u
}

// injection starts a new reply when Deepgram begins an authorized injection
// after an unfinished dropped model reply, or after an earlier completed Say
// whose audio stopped without AgentAudioDone. A partial multi-sentence Say
// has no waiter yet, so it stays on its current reply.
func (q *speechQueue) injection(u *utterance) *utterance {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current != u || u.decision == undecided || (u.decision == allowed && u.waiter == nil) {
		return u
	}
	if u.decision == allowed {
		q.awaitingEnd = append(q.awaitingEnd, u)
	}
	q.current = &utterance{text: true}
	return q.current
}

// foreign starts a new dropped reply when Deepgram resumes its own speech
// after an authorized injection. The authorized reply still gets the shared
// AgentAudioDone, while every following model byte stays with the new object.
func (q *speechQueue) foreign(u *utterance) *utterance {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == u && u.decision == allowed {
		q.awaitingEnd = append(q.awaitingEnd, u)
		q.current = &utterance{text: true}
		return q.current
	}
	return u
}

// done finishes the reply currently receiving audio. An undecided reply with
// audio remains only in awaitingText, so its later text cannot affect audio
// of the next reply. A model reply may follow an allowed injection before
// Deepgram sends its one terminal event, so that terminal finishes both.
func (q *speechQueue) done() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, previous := range q.awaitingEnd {
		previous.done = true
		q.endLocked(previous)
	}
	q.awaitingEnd = nil
	u := q.current
	if u == nil {
		q.signal()
		return
	}
	u.done = true
	q.current = nil
	if !u.text {
		q.awaitingText = append(q.awaitingText, u)
		q.signal()
		return
	}
	q.endLocked(u)
	q.signal()
}

// allow authorizes exactly u. When w waits for audio, its marker stays with
// this utterance; the player ends it only after Output accepts that audio.
func (q *speechQueue) allow(u *utterance, w *sayWait) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if u.decision == dropped {
		return
	}
	u.decision = allowed
	if w != nil && u.waiter == nil {
		u.waiter = w
		q.items = append(q.items, speechItem{kind: itemSaid, say: w, audioFirst: u.queued})
	}
	if !u.queued && len(u.audio) > 0 {
		for _, pcm := range u.audio {
			q.items = append(q.items, speechItem{kind: itemAudio, pcm: pcm})
		}
		u.audio = nil
		u.queued = true
	}
	q.endLocked(u)
	if len(q.items) > 0 {
		q.signal()
	}
}

func (q *speechQueue) endLocked(u *utterance) {
	if u.decision != allowed || !u.done || u.ended {
		return
	}
	u.ended = true
	q.items = append(q.items, speechItem{kind: itemEnd})
}

// reject drops precisely the reply whose text Deepgram just sent.
func (q *speechQueue) reject(u *utterance) {
	q.mu.Lock()
	defer q.mu.Unlock()
	u.decision, u.audio = dropped, nil
}

// silenced says whether Deepgram is actively sending a reply that cannot
// play. A finished rejected reply cannot make an injection cut a later one.
func (q *speechQueue) silenced() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current != nil && q.current.decision == dropped
}

// userSpoke ends the reply Deepgram stops for the room. Its late bytes and
// text cannot be claimed later; the next vendor event creates a new reply.
func (q *speechQueue) userSpoke() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rejectAllLocked()
}

func (q *speechQueue) rejectAllLocked() {
	if q.current != nil {
		q.current.decision, q.current.audio = dropped, nil
		q.current = nil
	}
	for _, u := range q.awaitingText {
		u.decision, u.audio = dropped, nil
	}
	q.awaitingText = nil
	q.awaitingEnd = nil
}

// interrupt drops queued speech and keeps the interrupted reply's object
// only to consume any tail Deepgram sends before its AgentAudioDone.
func (q *speechQueue) interrupt() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.items {
		if it.say != nil {
			it.say.finish(ErrInterrupted)
		}
	}
	if q.current != nil && q.current.waiter != nil {
		q.current.waiter.finish(ErrInterrupted)
	}
	for _, u := range q.awaitingEnd {
		if u.waiter != nil {
			u.waiter.finish(ErrInterrupted)
		}
	}
	clear(q.items)
	q.items = q.items[:0]
	for _, u := range q.awaitingText {
		u.decision, u.audio = dropped, nil
	}
	q.awaitingText = nil
	q.awaitingEnd = nil
	if q.current == nil {
		q.current = &utterance{decision: dropped}
	} else {
		q.current.decision, q.current.audio = dropped, nil
	}
	q.interrupted = true
	if q.stop != nil {
		q.stop()
	}
	q.signal()
}

// close clears every unfinished reply and ends a Say the player still held.
func (q *speechQueue) close(reason error) {
	q.finish(reason, false)
}

func (q *speechQueue) finish(reason error, interrupt bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.items {
		if it.say != nil {
			it.say.finish(reason)
		}
	}
	if q.current != nil && q.current.waiter != nil {
		q.current.waiter.finish(reason)
	}
	for _, u := range q.awaitingText {
		if u.waiter != nil {
			u.waiter.finish(reason)
		}
	}
	clear(q.items)
	q.items = q.items[:0]
	q.rejectAllLocked()
	if interrupt {
		q.interrupted = true
	}
	if q.stop != nil {
		q.stop()
	}
	q.signal()
}

// next waits for the player's next item. With wait above zero it returns
// itemIdle when nothing comes for that long. Once done is closed it returns
// itemClosed, whatever is queued, so Close begins no playback.
func (q *speechQueue) next(done <-chan struct{}, wait time.Duration) speechItem {
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		select {
		case <-done:
			return speechItem{kind: itemClosed}
		default:
		}
		q.mu.Lock()
		if q.interrupted {
			q.interrupted = false
			q.mu.Unlock()
			return speechItem{kind: itemInterrupt}
		}
		if len(q.items) > 0 {
			it := q.items[0]
			q.items[0] = speechItem{}
			q.items = q.items[1:]
			q.mu.Unlock()
			return it
		}
		q.mu.Unlock()
		select {
		case <-done:
			return speechItem{kind: itemClosed}
		case <-q.ready:
		case <-timeout:
			return speechItem{kind: itemIdle}
		}
	}
}

// begin starts an utterance: it returns the context its writes run under,
// which Interrupt and Close end, or false when an Interrupt came after the
// audio was taken from the queue, so it is not to be played.
func (q *speechQueue) begin(parent context.Context) (context.Context, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.interrupted {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	q.stop = cancel
	return ctx, true
}

// end releases the utterance's context.
func (q *speechQueue) end() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stop != nil {
		q.stop()
		q.stop = nil
	}
}

// play writes the agent's speech to the Output, one utterance per playback,
// and ends each Say once the utterance that holds its words is queued.
func (s *session) play() {
	defer s.emitters.Done()
	var (
		pb    voice.Playback
		ctx   context.Context
		frame = make([]byte, 0, speechFrame)
		said  *sayWait // a Say whose words' utterance is still being queued
	)
	stopped := func(ended bool) {
		s.speech.end()
		pb, frame = nil, frame[:0]
		s.setSpeaking(false, ended)
		s.emit(&voice.Event{Kind: voice.EventSpeechEnded})
	}
	endSaid := func(err error) {
		if said != nil {
			said.finish(err)
			said = nil
		}
	}
	for {
		var wait time.Duration
		switch {
		case pb != nil:
			wait = s.speechGap
		case said != nil:
			// Its audio has not started: it does by the Say's deadline, or
			// the Say is not spoken.
			wait = max(time.Until(said.deadline), time.Millisecond)
		}
		it := s.speech.next(s.done, wait)
		switch it.kind {
		case itemClosed:
			s.speech.end()
			return
		case itemSaid:
			endSaid(ErrInterrupted) // one Say at a time: an earlier one was cut
			said = it.say
			if pb == nil && it.audioFirst {
				endSaid(nil) // its words' audio came before their echo, and has played
			}
		case itemInterrupt:
			endSaid(ErrInterrupted)
			if pb != nil {
				stopped(false)
			}
		case itemEnd, itemIdle:
			if pb == nil {
				if it.kind == itemIdle {
					endSaid(ErrNotSpoken) // the Say's deadline passed with no audio of it
				}
				continue
			}
			endSaid(nil) // every frame of the utterance is queued
			_ = pb.Drain(ctx)
			// Played to its end: its echo follows. An Interrupt during the
			// drain cut it, and the room is to be heard at once.
			stopped(ctx.Err() == nil)
		case itemAudio:
			if pb == nil {
				var ok bool
				if ctx, ok = s.speech.begin(s.ctx); !ok {
					continue
				}
				pb = s.out.Begin()
				s.setSpeaking(true, false)
				s.emit(&voice.Event{Kind: voice.EventSpeechStarted})
			}
			if err := s.writeSpeech(ctx, pb, &frame, it.pcm); err != nil {
				if s.ctx.Err() != nil {
					s.speech.end()
					return
				}
				if ctx.Err() == nil {
					s.log.Warn("the Output refused the voice's speech", "error", err)
					endSaid(err)
				}
				stopped(false)
			}
		}
	}
}

// writeSpeech writes pcm to pb in frames of at most speechFrame, the whole
// samples at once. A trailing odd byte, half a sample, waits in frame for
// the next message.
func (s *session) writeSpeech(ctx context.Context, pb voice.Playback, frame *[]byte, pcm []byte) error {
	for len(pcm) > 0 {
		n := min(speechFrame-len(*frame), len(pcm))
		*frame = append(*frame, pcm[:n]...)
		pcm = pcm[n:]
		if len(*frame) == speechFrame {
			if err := pb.Write(ctx, *frame, speechFormat); err != nil {
				return err
			}
			*frame = (*frame)[:0]
		}
	}
	whole := len(*frame) &^ 1
	if whole == 0 {
		return nil
	}
	if err := pb.Write(ctx, (*frame)[:whole], speechFormat); err != nil {
		return err
	}
	odd := len(*frame) - whole
	copy(*frame, (*frame)[whole:])
	*frame = (*frame)[:odd]
	return nil
}

// byteRing keeps the last limit bytes written to it.
type byteRing struct {
	buf   []byte
	start int
	n     int
}

func newByteRing(limit int) *byteRing { return &byteRing{buf: make([]byte, limit)} }

func (r *byteRing) write(p []byte) {
	if len(p) >= len(r.buf) {
		copy(r.buf, p[len(p)-len(r.buf):])
		r.start, r.n = 0, len(r.buf)
		return
	}
	size := len(r.buf)
	n := copy(r.buf[(r.start+r.n)%size:], p)
	copy(r.buf, p[n:])
	r.n += len(p)
	if r.n > size {
		r.start = (r.start + r.n - size) % size
		r.n = size
	}
}

// parts returns what the ring holds, oldest first, in at most two slices.
func (r *byteRing) parts() [][]byte {
	if r.n == 0 {
		return nil
	}
	end := r.start + r.n
	if end <= len(r.buf) {
		return [][]byte{r.buf[r.start:end]}
	}
	return [][]byte{r.buf[r.start:], r.buf[:end-len(r.buf)]}
}

func (r *byteRing) reset() { r.start, r.n = 0, 0 }
