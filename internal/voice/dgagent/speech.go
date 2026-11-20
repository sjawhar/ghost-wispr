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
//
// From an injection's first echoed words until its reply ends, each frame
// of its audio waits holdTail before it plays: no message names the
// sentence a binary frame belongs to, so a frame may be the injection's or
// the start of the model's own words after it, whose text Deepgram sends a
// little after their first audio. When model text comes, the frames still
// waiting are dropped. The whole injection waits the same time, so its
// audio keeps its spacing. Model audio that leads its own text by more than
// holdTail plays: that is the gate's one bound, which the probe's frame log
// measures.
type utterance struct {
	decision utteranceDecision
	text     bool
	done     bool
	queued   bool
	ended    bool
	holding  bool     // an injection, its reply not ended: its audio waits
	held     [][]byte // its frames that wait, oldest first
	heldBase int      // the number of frames ever taken from held
	audio    [][]byte // audio before its text, waiting for its decision
	waiter   *sayWait
	cut      bool // the tail of a reply a user start stopped, until the next text or AgentAudioDone
}

// speechQueue carries the agent's speech from the reader, which must never
// wait on the speaker, to the player.
type speechQueue struct {
	mu           sync.Mutex
	items        []speechItem
	ready        chan struct{}                   // signalled when items or interrupted change
	holdTail     time.Duration                   // how long each frame of an injection waits
	after        func(d time.Duration, f func()) // runs f once d has passed
	holder       *utterance                      // the injection whose frames may still wait
	current      *utterance                      // the reply receiving audio now
	awaitingText []*utterance                    // completed audio-first replies, in wire order
	awaitingEnd  []*utterance                    // authorized replies superseded before their done
	interrupted  bool                            // the player has not seen the last Interrupt
	stop         context.CancelFunc
}

func newSpeechQueue(holdTail time.Duration, after func(time.Duration, func())) speechQueue {
	return speechQueue{ready: make(chan struct{}, 1), holdTail: holdTail, after: after}
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
	switch {
	case u.decision == undecided:
		u.audio = append(u.audio, pcm)
	case u.decision == allowed && u.holding:
		q.holdLocked(u, pcm)
	case u.decision == allowed:
		u.queued = true
		q.items = append(q.items, speechItem{kind: itemAudio, pcm: pcm})
		q.signal()
	}
}

// holdLocked makes pcm wait holdTail before it plays. q.mu is held.
func (q *speechQueue) holdLocked(u *utterance, pcm []byte) {
	u.held = append(u.held, pcm)
	frame := u.heldBase + len(u.held) // this frame's number, from 1
	q.after(q.holdTail, func() { q.releaseHeld(u, frame) })
}

// releaseHeld plays u's held frames up to frame, once frame has waited
// holdTail with no model text after it, and ends u if Deepgram already did.
func (q *speechQueue) releaseHeld(u *utterance, frame int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if u.decision != allowed {
		return
	}
	if q.queueHeldLocked(u, frame-u.heldBase) {
		q.endLocked(u)
		q.signal()
	}
}

// queueHeldLocked plays u's oldest n held frames, or all it holds when n is
// more, and says whether it played any. q.mu is held.
func (q *speechQueue) queueHeldLocked(u *utterance, n int) bool {
	n = min(n, len(u.held))
	if n <= 0 {
		return false
	}
	for _, pcm := range u.held[:n] {
		q.items = append(q.items, speechItem{kind: itemAudio, pcm: pcm})
	}
	u.queued = true
	u.held = u.held[n:]
	u.heldBase += n
	return true
}

// releaseHolderLocked plays all the holder still holds, and ends it if its
// reply has ended: no model text came after it. q.mu is held.
func (q *speechQueue) releaseHolderLocked() {
	h := q.holder
	if h == nil {
		return
	}
	q.holder = nil
	if h.decision != allowed {
		return
	}
	q.queueHeldLocked(h, len(h.held))
	h.holding = false
	q.endLocked(h)
	q.signal()
}

// dropHeldLocked drops what u still holds and says whether it held any.
func dropHeldLocked(u *utterance) bool {
	n := len(u.held)
	u.heldBase += n
	u.held = nil
	return n > 0
}

// dropLocked rejects u and everything it buffered or held. q.mu is held.
func dropLocked(u *utterance) {
	dropHeldLocked(u)
	u.decision, u.audio = dropped, nil
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
// whose audio stopped without AgentAudioDone. That earlier Say's held audio
// plays: no model text came after it. A partial multi-sentence Say has no
// waiter yet, so it stays on its current reply.
func (q *speechQueue) injection(u *utterance) *utterance {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current != u || u.decision == undecided || (u.decision == allowed && u.waiter == nil) {
		return u
	}
	if u.decision == allowed {
		if q.holder == u {
			q.releaseHolderLocked()
		}
		q.awaitingEnd = append(q.awaitingEnd, u)
	}
	q.current = &utterance{text: true}
	return q.current
}

// foreign notes model text. The audio still waiting since an injection's
// last text may be these model words, whether or not that injection's
// AgentAudioDone came, so it is dropped; a Say none of whose audio played is
// not spoken. When Deepgram resumes its own speech before the injection's
// AgentAudioDone, a new dropped reply takes the audio that follows, and the
// injection still gets the shared AgentAudioDone to end what of it played.
func (q *speechQueue) foreign(u *utterance) *utterance {
	q.mu.Lock()
	defer q.mu.Unlock()
	if h := q.holder; h != nil {
		q.holder = nil
		if h.decision == allowed {
			if dropHeldLocked(h) && !h.queued && h.waiter != nil {
				h.waiter.finish(ErrNotSpoken)
			}
			h.holding = false
			q.endLocked(h)
			q.signal()
		}
	}
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
	if !u.text && u.decision != dropped {
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
	switch {
	case !u.done && !u.holding:
		// Its reply goes on, so model words can still follow it in the
		// same reply: from here its audio waits, the audio before these
		// words as well, so it keeps its spacing.
		if q.holder != u {
			q.releaseHolderLocked() // an earlier injection: no model text came after it
		}
		u.holding, q.holder = true, u
		for _, pcm := range u.audio {
			q.holdLocked(u, pcm)
		}
		u.audio = nil
	case len(u.audio) > 0:
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

// endLocked ends u's playback once its reply ended and no audio of it still
// waits. q.mu is held.
func (q *speechQueue) endLocked(u *utterance) {
	if u.decision != allowed || !u.done || u.ended || len(u.held) > 0 {
		return
	}
	u.ended = true
	q.items = append(q.items, speechItem{kind: itemEnd})
}

// reject drops precisely the reply whose text Deepgram just sent. A stopped
// reply's tail that model text claims is the model's own reply from here.
func (q *speechQueue) reject(u *utterance) {
	q.mu.Lock()
	defer q.mu.Unlock()
	dropLocked(u)
	u.cut = false
}

// silenced says whether Deepgram is actively sending a reply that cannot
// play. A finished rejected reply cannot make an injection cut a later one.
func (q *speechQueue) silenced() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current != nil && q.current.decision == dropped && !q.current.cut
}

// userSpoke ends the reply Deepgram stops for the room. Audio that still
// comes, until the next text or AgentAudioDone, is taken as that reply's
// tail and dropped: its text came before the user start, so no later text
// may claim it. The next reply's audio that comes before its own text is
// dropped with it; the next text starts that reply. An injection's frames
// that still wait have not played, and may be the model's words whose text
// has not come. Over the voice's speech, which the Supervisor stops when
// someone talks over it, they are dropped, and the Say they belong to is
// interrupted. With no speech to talk over, they keep waiting for model
// text or the hold, as the end of a reply Deepgram stopped.
func (q *speechQueue) userSpoke(overVoice bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if h := q.holder; h != nil && h.decision == allowed {
		if overVoice {
			q.holder = nil
			if dropHeldLocked(h) && h.waiter != nil {
				h.waiter.finish(ErrInterrupted)
			}
			h.holding = false
		} else {
			h.done = true
			if q.current == h {
				q.current = nil
			}
		}
		q.endLocked(h)
		q.signal()
	}
	q.rejectAllLocked()
	q.current = &utterance{decision: dropped, cut: true}
}

// rejectAllLocked drops every reply that has not ended, other than an
// injection whose frames wait. q.mu is held.
func (q *speechQueue) rejectAllLocked() {
	if q.current != nil {
		dropLocked(q.current)
		q.current = nil
	}
	for _, u := range q.awaitingText {
		dropLocked(u)
	}
	q.awaitingText = nil
	q.awaitingEnd = nil
}

// interrupt drops queued speech and keeps the interrupted reply's object
// only to consume any tail Deepgram sends before its AgentAudioDone.
func (q *speechQueue) interrupt() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.endWaitersLocked(ErrInterrupted)
	if h := q.holder; h != nil {
		dropLocked(h)
		q.holder = nil
	}
	for _, u := range q.awaitingText {
		dropLocked(u)
	}
	q.awaitingText = nil
	if q.current == nil {
		q.current = &utterance{decision: dropped}
	} else {
		dropLocked(q.current)
	}
	q.interrupted = true
	if q.stop != nil {
		q.stop()
	}
	q.signal()
}

// close clears every unfinished reply and ends a Say the player still held.
func (q *speechQueue) close(reason error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.endWaitersLocked(reason)
	if h := q.holder; h != nil {
		dropLocked(h)
		q.holder = nil
	}
	q.rejectAllLocked()
	if q.stop != nil {
		q.stop()
	}
	q.signal()
}

// endWaitersLocked ends every Say whose marker is still queued, and drops the
// queued speech. The player ends the Say it already took at the Interrupt
// it is told of, and a Say ends at Close by itself. q.mu is held.
func (q *speechQueue) endWaitersLocked(reason error) {
	for _, it := range q.items {
		if it.say != nil {
			it.say.finish(reason)
		}
	}
	q.awaitingEnd = nil
	clear(q.items)
	q.items = q.items[:0]
}

// holds says whether audio of w's words came and waits behind the hold on
// audio after an injection's last text.
func (q *speechQueue) holds(w *sayWait) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.holder
	return w != nil && h != nil && h.waiter == w && len(h.held) > 0
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
					if s.speech.holds(said) {
						// Its audio came in time and waits for model text:
						// it plays within the hold, or is dropped with it.
						said.deadline = time.Now().Add(s.speech.holdTail)
						continue
					}
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
