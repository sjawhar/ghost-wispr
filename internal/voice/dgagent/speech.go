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
	audioFirst bool     // itemSaid's: its words' audio came before their echo
}

// speechQueue carries the agent's speech from the reader, which must never
// wait on the speaker, to the player.
type speechQueue struct {
	mu          sync.Mutex
	items       []speechItem
	ready       chan struct{} // signalled when items or interrupted change
	gap         time.Duration // a reply has ended when no more of it came for this long
	open        bool          // audio came since the last AgentAudioDone
	dropping    bool          // the rest of an interrupted reply: not played
	muted       bool          // the model's own words: not played until the reply ends
	gated       bool          // a reply's audio waits for its text to say whether it plays
	decided     bool          // a text of this reply said whether its audio plays
	waiting     [][]byte      // audio of this reply that came before any text of it
	audioDone   bool          // AgentAudioDone came while audio waited for its text
	audioFirst  bool          // audio came since the agent's last text
	lastAudio   time.Time
	interrupted bool // the player has not seen the last Interrupt
	stop        context.CancelFunc
}

func newSpeechQueue(gap time.Duration) speechQueue {
	// Audio is untrusted until its text says it is this session's injection.
	// Deepgram may send the audio first, so gate it from session start.
	return speechQueue{ready: make(chan struct{}, 1), gap: gap, gated: true}
}

func (q *speechQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// audio queues one binary message of agent speech.
func (q *speechQueue) audio(pcm []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	if q.dropping && now.Sub(q.lastAudio) > q.gap {
		q.dropping = false // the interrupted reply stopped; this is the next
	}
	q.lastAudio = now
	if q.dropping || q.muted {
		return
	}
	q.open, q.audioFirst = true, true
	if q.gated && !q.decided {
		// Its text has not come: Deepgram may send audio first (the real
		// probe saw 17 ms), and the model's own words are not to play.
		q.waiting = append(q.waiting, pcm)
		return
	}
	q.items = append(q.items, speechItem{kind: itemAudio, pcm: pcm})
	q.signal()
}

// textCame notes a text of the agent's, and says whether audio came since
// the text before it: Deepgram may send a text's audio first.
func (q *speechQueue) textCame() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	first := q.audioFirst
	q.audioFirst = false
	return first
}

// done marks the end of the reply's audio.
func (q *speechQueue) done() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.gated && !q.decided && len(q.waiting) > 0 {
		// The injection's audio came first. Keep it until its text decides
		// whether it may play; it is authorized only then.
		q.open, q.audioDone = false, true
		return
	}
	q.muted, q.decided, q.waiting, q.audioDone = false, false, nil, false // audio no text claimed
	if q.dropping {
		q.dropping, q.open = false, false
		return
	}
	q.open = false
	q.items = append(q.items, speechItem{kind: itemEnd})
	q.signal()
}

// mute drops the audio that follows, the model's own words, until the reply
// ends, an injection's words come, or the room speaks, and the audio of
// this reply that waited for its text. What is queued still plays, and the
// reply still ends at AgentAudioDone.
func (q *speechQueue) mute() {
	q.mu.Lock()
	q.muted, q.decided, q.waiting, q.audioDone = true, true, nil, false
	q.mu.Unlock()
}

// silenced says whether the reply Deepgram is speaking now is one that does
// not play: the model's own words, a filler, or words the session gave up
// on. An injection may cut it, as nothing of it reaches the room.
func (q *speechQueue) silenced() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.muted
}

// waitingAudio says the current reply has audio that arrived before its
// matching text. It has not reached Output, whether or not AgentAudioDone
// has arrived, so Say must be marked as waiting for the actual write.
func (q *speechQueue) waitingAudio() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting) > 0
}

// unmute plays the audio that follows, and the audio of this reply that
// waited for its text.
func (q *speechQueue) unmute() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.muted, q.decided = false, true
	for _, pcm := range q.waiting {
		q.items = append(q.items, speechItem{kind: itemAudio, pcm: pcm})
	}
	if len(q.waiting) > 0 {
		q.waiting = nil
		q.signal()
	}
	if q.audioDone {
		q.audioDone = false
		q.items = append(q.items, speechItem{kind: itemEnd})
		// The completed, authorized reply is now queued. The next vendor
		// utterance is untrusted again, including when its audio comes first.
		q.decided = false
		q.signal()
	}
}

// gate makes each reply's audio wait for its text to say whether it plays.
// ungate plays audio as it comes again, and drops what waited: the room
// spoke, and Deepgram stops a reply then.
func (q *speechQueue) gate() {
	q.mu.Lock()
	q.gated = true
	q.mu.Unlock()
}

func (q *speechQueue) ungate() {
	q.mu.Lock()
	q.gated, q.muted, q.decided, q.waiting, q.audioDone, q.open = false, false, false, nil, false, false
	q.mu.Unlock()
}

// mark queues w where its words' audio starts: the player ends w once the
// utterance that holds them is queued to the Output, or at once when their
// audio came before their echo and has played. Its words are new speech, so
// the rest of an interrupted reply, which is dropped, has ended.
func (q *speechQueue) mark(w *sayWait, audioFirst bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropping = false
	q.items = append(q.items, speechItem{kind: itemSaid, say: w, audioFirst: audioFirst})
	q.signal()
}

// userSpoke notes Deepgram's UserStartedSpeaking: it stops its reply then
// (/docs/voice-agent-user-started-speaking), so the audio after it, even
// with no AgentAudioDone between, is the next reply's, and waits for its
// text. What is queued plays on: barge-in is the Supervisor's.
func (q *speechQueue) userSpoke() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropping, q.open, q.decided, q.waiting, q.audioDone = false, false, false, nil, false
}

// interrupt drops what is queued and stops the utterance playing; audio
// still coming for the reply that was playing is dropped too, and a Say
// waiting in the queue ends with ErrInterrupted.
func (q *speechQueue) interrupt() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.items {
		if it.say != nil {
			it.say.finish(ErrInterrupted)
		}
	}
	clear(q.items)
	q.items = q.items[:0]
	q.decided, q.waiting = false, nil
	if q.open {
		q.dropping, q.open = true, false
	}
	q.interrupted = true
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
