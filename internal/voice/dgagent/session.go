package dgagent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// holdLimit is how much room audio the session holds while the voice speaks,
// to send on if the Supervisor interrupts the voice: 2 s of PCM16 at InputRate.
const holdLimit = 2 * InputRate * 2

// eventBuffer is how many events wait for the Supervisor before the reader
// waits for it.
const eventBuffer = 64

// session is one Voice Agent conversation. It implements voice.Session.
//
// Deepgram has no setting that turns its own barge-in off: a user who starts
// speaking stops the agent's reply. So while the voice's speech plays, the
// session sends Deepgram silence in place of the room and holds the last 2 s
// of the room; the Supervisor alone decides that someone talked over the
// voice, by one rule for every voice. When it calls Interrupt, the held audio
// goes on to Deepgram, which then hears the words that interrupted.
type session struct {
	log   *slog.Logger
	conn  *websocket.Conn
	out   voice.Output
	tools voice.ToolRunner
	tap   <-chan audio.Frame

	ctx       context.Context // ends at Close
	cancel    context.CancelFunc
	done      chan struct{} // closed at Close
	closeOnce sync.Once
	closed    atomic.Bool // the connection is closed
	writeMu   sync.Mutex

	events   chan voice.Event
	emitters sync.WaitGroup // the goroutines that send events
	workers  sync.WaitGroup // the others

	speech     speechQueue
	speechGap  time.Duration
	keepAlive  time.Duration
	sayLimit   time.Duration
	ackLimit   time.Duration
	retryGrace time.Duration

	holdMu   sync.Mutex    // guards speaking, quietAt and flush, which the pump reads together
	speaking bool          // the voice's speech is playing, so room audio is held
	quietAt  time.Time     // until then the room is silenced for the echo of speech that ended
	echoTail time.Duration // how long that is
	flush    bool          // an Interrupt asks for the held room audio to go on

	sayMu        sync.Mutex // one injection at a time: a refusal names no message
	mu           sync.Mutex
	say          *sayWait // the injection whose words Deepgram has not all echoed
	lastRefusal  refusal  // the refusal that last ended an injection
	pending      map[string]*pendingCall
	lostErr      error         // why the connection ended, once it has
	userTurns    int           // the room's turns Deepgram has heard
	turnWords    string        // what Deepgram heard the room say in its last turn
	turnOpen     bool          // Deepgram heard the room's turn start and has not sent its words
	turnHeard    chan struct{} // closed when the current turn's words come
	answeredTurn int           // the turn an answer call last ran for; -1 for none
	modelQuiet   bool          // the model called answer, and the room has not spoken since
	heldAnswer   held          // an answer Deepgram refused, to hand over when the room is done
	retry        *time.Timer
	asker        *time.Timer // sends the asker's words as text if Deepgram hears no turn
	// abandoned holds the words of injections the session gave up on while
	// Deepgram may still speak them: they are not played when it does.
	abandoned [][]string
}

// held is an answer Deepgram refused to speak because someone was speaking.
type held struct {
	say  string
	turn int       // the room's turn it answers
	at   time.Time // when it was first refused
}

// sayWait is one injection: first waiting for Deepgram to echo its words,
// then, for a Say, for the player to queue their audio to the Output.
type sayWait struct {
	remaining  []string      // the words not yet echoed
	words      int           // how many words it has
	turn       int           // the room's turn when it was handed over
	limit      time.Duration // how long Deepgram may take to start the words
	deadline   time.Time     // a Say whose audio has not started by then is not spoken
	untilAudio bool          // finish once the words' audio is queued, not at the echo
	answer     *held         // the answer it hands over, held again if Deepgram refuses it
	echoed     chan struct{} // closed once every word is echoed
	once       sync.Once
	result     chan error
}

func (w *sayWait) finish(err error) { w.once.Do(func() { w.result <- err }) }

// refusal is one refusal message Deepgram sent: its type and when it came.
type refusal struct {
	kind string
	at   time.Time
}

// pendingCall is a call to the answer function that Deepgram waits on.
type pendingCall struct {
	fn     functionCall
	turn   int                // the room's turn it answers
	cancel context.CancelFunc // stops the Concierge's run
	// voided ends when Deepgram cancels the call or the session ends, and
	// not when Deliver answers it: an answer waiting to be handed over is
	// then not handed over.
	voided  context.Context
	void    context.CancelFunc
	claimed bool // the run or a Deliver is answering it
}

var _ voice.Session = (*session)(nil)

func start(cfg *Config, conn *websocket.Conn, x *voice.Exchange) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		log:        cfg.Logger.With("session_id", x.SessionID),
		conn:       conn,
		out:        x.Out,
		tools:      x.Tools,
		tap:        x.Tap,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		events:     make(chan voice.Event, eventBuffer),
		speech:     newSpeechQueue(cfg.speechGap),
		speechGap:  cfg.speechGap,
		keepAlive:  cfg.keepAlive,
		sayLimit:   cfg.sayLimit,
		ackLimit:   cfg.ackLimit,
		retryGrace: cfg.retryGrace,
		pending:    make(map[string]*pendingCall),
		echoTail:   cfg.echoTail,
		// No answer has run for any turn yet.
		answeredTurn: -1,
	}
	s.emitters.Add(2)
	go s.read()
	go s.play()
	if words := strings.TrimSpace(x.AskerTurn); words != "" {
		s.asker = time.AfterFunc(cfg.askerWait, func() { s.askIfUnheard(words) })
	}
	s.workers.Add(2)
	go s.pump()
	go s.keepAliveLoop()
	go func() {
		s.emitters.Wait()
		close(s.events)
	}()
	return s
}

// Events reports what the voice does. The channel closes after Close.
func (s *session) Events() <-chan voice.Event { return s.events }

func (s *session) emit(e *voice.Event) {
	select {
	case s.events <- *e:
	case <-s.done:
	}
}

// Say has Deepgram speak text word for word, queued behind any speech of
// its own, and returns once the words' audio is queued to the Output; or
// ErrRefused when Deepgram will not speak them now because someone holds
// the floor, ErrInterrupted when Interrupt stopped them, or ErrNotSpoken
// when Deepgram did not start them within the session's limit, which for
// an Ack is short: Deepgram holds a routing line said during an answer call
// behind the model's own reply to the call, and that reply waits for the
// answer the Supervisor's Concierge is still finding. Text with no words
// returns nil at once: Deepgram closes a session whose InjectAgentMessage
// has no message.
func (s *session) Say(ctx context.Context, text string, mode voice.SayMode) error {
	s.log.Debug("voice says", "mode", mode.String())
	limit := s.sayLimit
	if mode == voice.Ack {
		limit = s.ackLimit
	}
	w, err := s.inject(ctx, text, true, nil, limit)
	if w == nil {
		return err
	}
	return s.await(ctx, w)
}

// inject hands text to Deepgram to speak word for word, queued behind any
// speech of its own, and returns the wait for its words, which holds the
// session's one injection until await returns; or nil, with an error if
// the text could not be sent, and with nil if it has no words. With
// untilAudio the wait lasts until the words' audio is queued to the Output,
// else until Deepgram echoes them. An answer Deepgram refuses is held, to
// hand over again when the room is done. When the reply Deepgram is
// speaking does not play, the injection cuts it: Deepgram streams a reply
// in real time, and the end-to-end proof at 6ed0386 saw an answer wait
// behind the model's own muted reply until the exchange ended.
func (s *session) inject(ctx context.Context, text string, untilAudio bool, answer *held, limit time.Duration) (*sayWait, error) {
	text = strings.TrimSpace(text)
	words := wordsOf(text)
	if len(words) == 0 {
		return nil, nil
	}
	s.sayMu.Lock()
	if err := ctx.Err(); err != nil {
		// Its caller gave up while another injection held the session's.
		s.sayMu.Unlock()
		return nil, err
	}
	w := &sayWait{
		remaining: words, words: len(words), limit: limit, deadline: time.Now().Add(limit),
		untilAudio: untilAudio, answer: answer, echoed: make(chan struct{}), result: make(chan error, 1),
	}
	s.mu.Lock()
	if s.lostErr != nil {
		err := s.lostErr
		s.mu.Unlock()
		s.sayMu.Unlock()
		return nil, err
	}
	w.turn = s.userTurns
	s.say = w
	callPending := len(s.pending) > 0
	s.mu.Unlock()
	// A reply that does not play is cut, but never while an answer call is
	// pending: Deepgram then cancels the call (the proof at 66b984b saw
	// FunctionCallCancelled 66 ms after an interrupting routing line).
	behavior := "queue"
	if !callPending && s.speech.silenced() {
		behavior = "interrupt"
		s.log.Debug("the injection cuts a reply that does not play")
	}
	if err := s.writeJSON(injectAgentMessage{Type: "InjectAgentMessage", Message: text, Behavior: behavior}); err != nil {
		s.release(w)
		return nil, err
	}
	return w, nil
}

// await waits for an injection's words, and frees the session's injection.
// Words it gives up on before Deepgram has spoken them all, other than
// words Deepgram refused, may still be spoken: Deepgram has no message to
// take an injection back. They are not played when they come.
func (s *session) await(ctx context.Context, w *sayWait) (err error) {
	defer func() {
		s.mu.Lock()
		if s.say == w {
			s.say = nil
		}
		if err != nil && !errors.Is(err, ErrRefused) && len(w.remaining) > 0 {
			s.abandoned = append(s.abandoned, w.remaining)
			if len(s.abandoned) > maxAbandoned {
				s.abandoned = s.abandoned[1:]
			}
		}
		s.mu.Unlock()
		s.sayMu.Unlock()
	}()
	limit := time.NewTimer(w.limit)
	defer limit.Stop()
	echoed, limitC := w.echoed, limit.C
	for {
		select {
		case err := <-w.result:
			return err
		case <-echoed:
			// Spoken: the limit was for Deepgram to start; the audio's own
			// end, an Interrupt or Close now ends the wait.
			echoed, limitC = nil, nil
		case <-limitC:
			return ErrNotSpoken
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrClosed
		}
	}
}

// maxAbandoned bounds the injections given up on that the session watches
// for: Deepgram speaks its queue in order, so an old one is long spoken.
const maxAbandoned = 4

// abandonedSaid says whether content is words of an injection the session
// gave up on, and consumes them.
func (s *session) abandonedSaid(content string) bool {
	words := wordsOf(content)
	if len(words) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rest := range s.abandoned {
		if len(words) <= len(rest) && slices.Equal(rest[:len(words)], words) {
			if rest = rest[len(words):]; len(rest) > 0 {
				s.abandoned[i] = rest
			} else {
				s.abandoned = slices.Delete(s.abandoned, i, i+1)
			}
			return true
		}
	}
	return false
}

// release frees the session's injection that w holds, when it was never
// sent.
func (s *session) release(w *sayWait) {
	s.mu.Lock()
	if s.say == w {
		s.say = nil
	}
	s.mu.Unlock()
	s.sayMu.Unlock()
}

// endSay ends the injection still waiting for its echo with err.
func (s *session) endSay(err error) {
	s.mu.Lock()
	w := s.say
	s.say = nil
	s.mu.Unlock()
	if w != nil {
		w.finish(err)
	}
}

// refused ends the waiting injection with ErrRefused, unless this message is
// the other one of the pair Deepgram may send for the refusal that ended the
// last one.
func (s *session) refused(kind string) {
	now := time.Now()
	s.mu.Lock()
	last := s.lastRefusal
	if last.kind != "" && last.kind != kind && now.Sub(last.at) < refusalEcho {
		s.lastRefusal = refusal{}
		s.mu.Unlock()
		return
	}
	w := s.say
	s.say = nil
	if w != nil {
		s.lastRefusal = refusal{kind: kind, at: now}
		if w.answer != nil {
			// Held here, before the reader reads on, so the room's turn that
			// follows finds it; handed over again after it, or after
			// retryGrace when no turn follows.
			h := *w.answer
			if h.at.IsZero() {
				h.at = now
			}
			s.heldAnswer = h
			s.armRetryLocked()
		}
	}
	s.mu.Unlock()
	if w != nil {
		w.finish(ErrRefused)
	}
}

// spoken notes words Deepgram reports the agent speaks, and says whether
// they are the waiting injection's. Deepgram echoes an injected text one
// sentence at a time, and may first finish the reply it was speaking, so
// the words are matched in order against what the injection has left, by
// words alone: punctuation and capitals may differ. The reply object holds
// its own authorization and waiter, so an earlier reply cannot complete this
// Say or release its audio.
func (s *session) spoken(content string, u *utterance) bool {
	words := wordsOf(content)
	if len(words) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.say
	if w == nil || len(words) > len(w.remaining) || !slices.Equal(w.remaining[:len(words)], words) {
		return false
	}
	u = s.speech.injection(u)
	w.remaining = w.remaining[len(words):]
	if len(w.remaining) > 0 {
		s.speech.allow(u, nil)
		return true
	}
	s.say = nil
	close(w.echoed)
	s.speech.allow(u, w)
	if !w.untilAudio {
		w.finish(nil)
	}
	return true
}

// agentSaid handles words the agent speaks, decides whether the audio that
// follows them plays, and says whether they do. The model keeps the turn
// and states no fact, so from the moment it calls answer until the room
// speaks again, its own words are not played: the real probe heard phonellm
// say "The issue is held by Lucia." with its call while the Concierge found
// that Ada holds it, and gpt-4o-mini add a fact after the answer. A turn it
// answers in its own words, with no call, goes to the Concierge. Only the
// session's own injections play, and the model's words outside a turn.
//
// Each text's kind is logged at Debug, without its words, so a probe can
// count the model's own words that played.
func (s *session) agentSaid(m *serverMessage) bool {
	u := s.speech.textCame()
	// Words given up on come first: Deepgram speaks its queue in order, and
	// the next injection may begin with the same words, such as an answer
	// after the routing line that was given up on.
	if s.abandonedSaid(m.Content) {
		s.log.Info("words the session gave up on are not played")
		s.speech.reject(s.speech.foreign(u))
		return false
	}
	if s.spoken(m.Content, u) {
		s.log.Debug("agent words", "kind", "injected")
		return true // an injection's words: spoken unmutes them
	}
	if m.Behavior == "filler" {
		// Voice C asks for no Think Live; a filler that comes anyway is
		// worded by Deepgram's model, and is not played.
		s.log.Debug("agent words", "kind", "filler")
		s.speech.reject(s.speech.foreign(u))
		return false
	}
	s.mu.Lock()
	quiet := s.modelQuiet
	turn, words, heard, takeTurn := s.userTurns, s.turnWords, s.turnHeard, false
	if !quiet && s.userTurns > 0 && s.answeredTurn != s.userTurns && s.lostErr == nil {
		// The model answers a turn of the room's in its own words, with
		// no answer call: the real probe heard phonellm say "I don't have
		// that detail in the records." to a follow-up. Its words are not
		// played, and the Concierge answers the turn instead. The turn's
		// words may come after the reply.
		takeTurn = true
		s.answeredTurn = turn
		s.modelQuiet = true
		// This reply is decided below; a later reply starts with its own
		// utterance object and needs no session-wide gate reset.
		if s.heldAnswer.turn != turn {
			s.heldAnswer = held{}
		}
	}
	s.mu.Unlock()
	if takeTurn {
		s.log.Info("the model answered a turn in its own words; the Concierge answers it instead")
		s.speech.reject(s.speech.foreign(u))
		s.answerTurn(turn, words, heard)
		return false
	}
	if !quiet {
		s.log.Debug("agent words", "kind", "model")
	} else {
		s.log.Info("the model's own words while it has an answer are not played")
	}
	// No model words play. An injected answer or allowed routing line is the
	// only Deepgram speech that reaches Output, even before the first user
	// turn or after a session that has delivered only a held answer.
	s.speech.reject(s.speech.foreign(u))
	return false
}

// answerTurn asks the Concierge the room's turn the model answered in its
// own words, and hands the answer over word for word. It reports the call
// here, on the reader, and runs it on its own, first waiting a little for
// the turn's words when they have not come; heard closes when they do.
func (s *session) answerTurn(turn int, words string, heard <-chan struct{}) {
	id := fmt.Sprintf("dgagent-turn-%d", turn)
	args, _ := json.Marshal(voice.AnswerArgs{Question: words})
	s.emit(&voice.Event{Kind: voice.EventToolCall, Call: voice.ToolCall{ID: id, Name: voice.AnswerTool, Args: args}})
	go func() {
		if words == "" && heard != nil {
			select {
			case <-heard:
			case <-time.After(turnWordsWait):
			case <-s.done:
				return
			}
			s.mu.Lock()
			if s.userTurns == turn {
				words = s.turnWords
			}
			s.mu.Unlock()
			args, _ = json.Marshal(voice.AnswerArgs{Question: words})
		}
		result, err := s.tools.Run(s.ctx, voice.ToolCall{ID: id, Name: voice.AnswerTool, Args: args})
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			// The Supervisor says the Concierge's own sorry line. Its error
			// may quote the room's words, so it reaches no log level.
			s.log.Warn("the answer failed", "call_id", id)
			return
		}
		var answer voice.AnswerResult
		if json.Unmarshal(result, &answer) != nil {
			s.log.Warn("the answer is not an AnswerResult", "call_id", id)
			return
		}
		w, _ := s.inject(s.ctx, answer.Say, false, &held{say: answer.Say, turn: turn}, s.sayLimit)
		if w != nil {
			_ = s.await(s.ctx, w)
		}
	}()
}

// roomTurn notes that the room spoke: Deepgram heard a turn start
// (UserStartedSpeaking, words ""), or reports a turn's words at its end.
// A turn counts from its start, so an answer call that comes before the
// turn's words, as Deepgram sometimes sends it, answers that turn; words
// with no start before them are a turn of their own. The reply to a turn
// waits for its text, which says whether it plays: the model's own answer
// goes to the Concierge instead, and its audio may come first. An answer
// Deepgram refused is handed over again once a turn ends, unless the turn
// asks a new question.
func (s *session) roomTurn(words string, heard bool) {
	s.mu.Lock()
	if s.retry != nil {
		s.retry.Stop()
	}
	starts := !heard || !s.turnOpen
	var stale *sayWait
	if starts {
		s.userTurns++
		s.turnWords = ""
		s.modelQuiet = false
		s.turnHeard = make(chan struct{})
		// An answer to an earlier turn that Deepgram has not begun to speak
		// would hold this turn's answer up for the Say limit: it is held,
		// to hand over again when the room is done, and its words are not
		// played if Deepgram speaks them later.
		if w := s.say; w != nil && w.answer != nil && w.turn < s.userTurns && len(w.remaining) == w.words {
			s.say, stale = nil, w
			h := *w.answer
			if h.at.IsZero() {
				h.at = time.Now()
			}
			s.heldAnswer = h
			s.armRetryLocked()
		}
	}
	s.turnOpen = !heard
	if heard {
		s.turnWords = words
		close(s.turnHeard)
		if s.heldAnswer.say != "" {
			s.armRetryLocked()
		}
	}
	s.mu.Unlock()
	if stale != nil {
		stale.finish(ErrNotSpoken)
	}
	// Each reply starts and ends in speechQueue. The room turn changes no
	// speech authorization: a later Deepgram reply always has its own object.
}

// armRetryLocked hands the held answer over again after retryGrace, unless
// a turn of the room's comes first. s.mu is held.
func (s *session) armRetryLocked() {
	if s.retry != nil {
		s.retry.Stop()
	}
	if s.ctx.Err() == nil {
		s.retry = time.AfterFunc(s.retryGrace, s.retryRefused)
	}
}

// retryRefused hands over again the answer Deepgram refused, if the room
// asked nothing new since and it is not older than an injection may wait.
func (s *session) retryRefused() {
	s.mu.Lock()
	h := s.heldAnswer
	s.heldAnswer = held{}
	s.mu.Unlock()
	if h.say == "" || s.ctx.Err() != nil {
		return
	}
	if time.Since(h.at) > s.sayLimit {
		s.log.Warn("an answer Deepgram refused was not spoken within the limit")
		return
	}
	s.log.Info("handing a refused answer over again")
	w, err := s.inject(s.ctx, h.say, false, &h, s.sayLimit)
	if w == nil {
		if err != nil {
			s.log.Info("the refused answer could not be handed over again", "reason", err)
		}
		return
	}
	_ = s.await(s.ctx, w)
}

// askIfUnheard hands Deepgram the asker's words at the wake as the room's
// text when it has heard no turn of the room's since the session opened:
// the live transcript can lag the mic by more than the tap holds, and the
// question's audio is then gone before the session opens (the end-to-end
// proof at ace991b saw a 10 s lag, and the exchange ended in silence).
func (s *session) askIfUnheard(words string) {
	s.mu.Lock()
	heard := s.userTurns > 0
	s.mu.Unlock()
	if heard || s.ctx.Err() != nil {
		return
	}
	s.log.Info("Deepgram heard no turn; the asker's words at the wake go to it as text")
	if err := s.writeJSON(injectUserMessage{Type: "InjectUserMessage", Content: words}); err != nil {
		s.log.Info("the asker's words could not be sent", "reason", err)
	}
}

// wordsOf is text's words, lower case, without punctuation.
func wordsOf(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Deliver answers the answer call callID, still waiting, with result, an
// AnswerResult in place of the one the Concierge would return: its words
// are spoken word for word, then the call is answered. A result that is not
// an AnswerResult leaves the call waiting.
func (s *session) Deliver(ctx context.Context, callID string, result json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var answer voice.AnswerResult
	if err := json.Unmarshal(result, &answer); err != nil {
		return fmt.Errorf("deliver %s: the result is not an AnswerResult: %w", callID, err)
	}
	call := s.claim(callID)
	if call == nil {
		return fmt.Errorf("deliver %s: %w", callID, ErrNoPendingCall)
	}
	call.cancel() // the Concierge's run: Deliver answers instead
	// The call ends once answered; the wait for its words lasts as long as
	// the session and Deliver's ctx.
	waitCtx, stop := context.WithCancel(s.ctx)
	defer stop()
	defer context.AfterFunc(ctx, stop)()
	return s.answer(waitCtx, call, answer.Say)
}

// Interrupt stops the voice's speech now: what is queued is dropped, the
// rest of the reply Deepgram is still sending is not played, and a Say not
// yet played returns ErrInterrupted. The room audio held while it spoke
// goes on to Deepgram.
func (s *session) Interrupt() {
	// The flush is asked for before the queue is interrupted: once it is,
	// the player marks the speech stopped, and had it done so first, the
	// held audio would be dropped instead of sent on.
	s.holdMu.Lock()
	if s.speaking {
		s.speaking, s.flush = false, true
	}
	s.quietAt = time.Time{} // the room is heard at once: it interrupted
	s.holdMu.Unlock()
	s.endSay(ErrInterrupted)
	s.speech.interrupt()
}

// setSpeaking says whether the voice's speech plays, so the pump holds the
// room audio. Speech that ended by itself leaves the room silenced for the
// echo tail; speech that was stopped does not.
func (s *session) setSpeaking(on, ended bool) {
	s.holdMu.Lock()
	s.speaking = on
	if ended {
		s.quietAt = time.Now().Add(s.echoTail)
	}
	s.holdMu.Unlock()
}

// Close ends the session: the connection closes at once, calls are
// cancelled, and the session writes nothing to the Output after it returns.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
		s.speech.close(ErrClosed)
		s.mu.Lock()
		if s.retry != nil {
			s.retry.Stop()
		}
		if s.asker != nil {
			s.asker.Stop()
		}
		s.mu.Unlock()
		_ = s.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(20*time.Millisecond))
		s.abort()
	})
	s.emitters.Wait()
	s.workers.Wait()
	return nil
}

// abort closes the connection at once, with a reset so no unsent audio keeps
// it alive in the kernel; a write blocked on it returns. It closes the TCP
// connection under a TLS one itself: closing the TLS first sends close_notify,
// which waits up to 5 s on a network that takes no more data.
func (s *session) abort() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	nc := s.conn.NetConn()
	if tc, ok := nc.(*tls.Conn); ok {
		nc = tc.NetConn()
	}
	if tcp, ok := nc.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = nc.Close()
}

func (s *session) write(kind int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	// The websocket fork only stores the deadline here (conn.go:787-790);
	// WriteMessage applies it, and reports a closed connection.
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(kind, data); err != nil {
		s.abort()
		return fmt.Errorf("dgagent: write: %w", err)
	}
	return nil
}

func (s *session) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write(websocket.TextMessage, data)
}

// read hands Deepgram's messages to the session until the connection ends.
func (s *session) read() {
	defer s.emitters.Done()
	for {
		kind, data, err := s.conn.ReadMessage()
		if err != nil {
			s.lost(err)
			return
		}
		if kind == websocket.BinaryMessage {
			s.speech.audio(data)
			continue
		}
		var m serverMessage
		if err := json.Unmarshal(data, &m); err != nil {
			s.log.Warn("unreadable Voice Agent message", "error", err)
			continue
		}
		if m.Type == "LatencyReport" && m.STTLatency == nil {
			s.logLatency(data)
		}
		s.handle(&m)
	}
}

func (s *session) handle(m *serverMessage) {
	switch m.Type {
	case "ConversationText":
		switch m.Role {
		case "user":
			s.roomTurn(m.Content, true)
			s.emit(&voice.Event{Kind: voice.EventUserTurn, Text: m.Content})
		case "assistant":
			// Reported only when they play: the Supervisor's own-speech
			// filter reads them, and words the room never hears would
			// hide room words that match them.
			if s.agentSaid(m) {
				s.emit(&voice.Event{Kind: voice.EventAgentText, Text: m.Content})
			}
		}
	case "FunctionCallRequest":
		for _, f := range m.Functions {
			if f.ClientSide {
				s.call(f)
			}
		}
	case "FunctionCallCancelled":
		for _, f := range m.Functions {
			if call := s.take(f.ID); call != nil {
				s.forgetTurn(call)
				call.cancel()
				call.void()
			}
		}
	case "AgentAudioDone":
		s.speech.done()
	case "UserStartedSpeaking":
		// Deepgram stops its reply here: the reply ends before the turn
		// gates the next one.
		s.speech.userSpoke()
		s.roomTurn("", false)
	case "InjectionRefused":
		s.refused(m.Type)
	case "Warning":
		s.logProblem(slog.LevelWarn, "Voice Agent warning", m)
		if strings.HasPrefix(m.Code, "INJECT_AGENT_MESSAGE_DURING_") {
			s.refused(m.Type)
		}
	case "Error":
		s.logProblem(slog.LevelError, "Voice Agent error", m)
		s.emit(&voice.Event{Kind: voice.EventErr, Err: m.err()})
	}
}

// logProblem logs a Deepgram error or warning by its code only. Its free-text
// description can quote the room's words, and no deletion path reaches a
// log, even when log_level is Debug.
func (s *session) logProblem(level slog.Level, msg string, m *serverMessage) {
	s.log.Log(context.Background(), level, msg, "code", m.Code)
}

// logLatency logs the figures of one of Deepgram's latency reports for a
// turn (its model and speech times), and nothing that is not a number.
func (s *session) logLatency(data []byte) {
	var report map[string]any
	if json.Unmarshal(data, &report) != nil {
		return
	}
	attrs := make([]any, 0, 2*len(report))
	for k, v := range report {
		if f, ok := v.(float64); ok {
			attrs = append(attrs, k, f)
		}
	}
	s.log.Debug("Voice Agent latency", attrs...)
}

// lost ends what waits on the connection once it has ended.
func (s *session) lost(err error) {
	s.abort()
	reason := ErrClosed
	if s.ctx.Err() == nil { // not Close's doing
		err = withoutCloseText(err)
		reason = fmt.Errorf("%w: %w", ErrDisconnected, err)
		s.log.Warn("Voice Agent connection ended", "error", err)
		s.emit(&voice.Event{Kind: voice.EventErr, Err: reason})
	}
	s.mu.Lock()
	s.lostErr = reason
	calls := s.pending
	s.pending = make(map[string]*pendingCall)
	s.mu.Unlock()
	for _, c := range calls {
		c.cancel()
		c.void()
	}
	s.speech.close(reason)
	s.endSay(reason)
}

// call runs one call to the answer function: the exchange's tools find the
// answer, the session speaks its words word for word, then tells the model
// whether they were spoken. A call to any other function, which voice C
// never declared, reaches no tool.
func (s *session) call(f functionCall) {
	if f.Name != voice.AnswerTool {
		s.log.Warn("Deepgram called a function voice C does not declare", "call_id", f.ID, "function", f.Name)
		go func() { _ = s.respond(&f, errorContent("there is no function "+f.Name)) }()
		return
	}
	args := json.RawMessage(f.Arguments)
	if strings.TrimSpace(f.Arguments) == "" {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		s.log.Warn("function call arguments are not JSON", "call_id", f.ID, "function", f.Name)
		go func() { _ = s.respond(&f, errorContent("the arguments were not JSON")) }()
		return
	}
	runCtx, cancel := context.WithCancel(s.ctx)
	voided, void := context.WithCancel(s.ctx)
	call := &pendingCall{fn: f, cancel: cancel, voided: voided, void: void}
	s.mu.Lock()
	if s.lostErr != nil {
		s.mu.Unlock()
		cancel()
		return
	}
	if s.answeredTurn == s.userTurns {
		// One answer per turn of the room's: a model may call again at once
		// or later; the room has the turn's answer, or talked over it. The
		// model is told the turn is answered, so it stops: told it was not,
		// phonellm called again up to nine times in three seconds.
		s.mu.Unlock()
		cancel()
		s.log.Info("a second answer call for one turn is not run", "call_id", f.ID)
		go func() { _ = s.respond(&f, answered) }()
		return
	}
	call.turn = s.userTurns
	s.answeredTurn = call.turn
	s.modelQuiet = true // its words with the call, and after it, are not played
	if s.heldAnswer.turn != call.turn {
		s.heldAnswer = held{} // a new question: its answer replaces the refused one
	}
	args = withQuestion(args, s.turnWords)
	// Registered before it is reported, so a Deliver for it finds it.
	s.pending[f.ID] = call
	s.mu.Unlock()
	tc := voice.ToolCall{ID: f.ID, Name: f.Name, Args: args}
	s.emit(&voice.Event{Kind: voice.EventToolCall, Call: tc})
	go func() {
		defer cancel()
		result, err := s.tools.Run(runCtx, tc)
		if s.claim(f.ID) == nil || runCtx.Err() != nil {
			return // cancelled, delivered, or the session ended
		}
		if err != nil {
			// The error may quote the call's arguments, so it reaches no log
			// level. The Supervisor says the Concierge's own sorry line.
			s.log.Warn("the answer failed", "call_id", f.ID)
			_ = s.finish(call, notAnswered)
			return
		}
		var answer voice.AnswerResult
		if err := json.Unmarshal(result, &answer); err != nil {
			s.log.Warn("the answer is not an AnswerResult", "call_id", f.ID)
			_ = s.finish(call, notAnswered)
			return
		}
		_ = s.answer(s.ctx, call, answer.Say)
	}()
}

// withQuestion is the answer tool's arguments with turnWords as the
// question when the model gave none: the real probe saw phonellm call
// answer with {} as the turn ended.
func withQuestion(args json.RawMessage, turnWords string) json.RawMessage {
	var a voice.AnswerArgs
	if turnWords == "" || json.Unmarshal(args, &a) != nil || strings.TrimSpace(a.Question) != "" {
		return args
	}
	a.Question = turnWords
	filled, err := json.Marshal(a)
	if err != nil {
		return args
	}
	return filled
}

// answer hands say to Deepgram to speak word for word, then tells the model
// whether the question was answered, and waits for the words. It tells the
// model at once, without waiting for Deepgram to speak them: Deepgram
// queues an injection behind the model's reply in flight, which waits for
// this answer (the real probe sat 30 s in that wait). When the model's own
// reply, which does not play, is coming, the call is answered first and the
// words then cut that reply: they would wait behind it, and an injection
// that cuts it while the call is pending makes Deepgram cancel the call.
// The model is told nothing of the answer itself: a fact the model can read
// is one it speaks (the real probe heard gpt-4o-mini speak the context). An
// answer Deepgram refuses, because someone spoke as it was handed over, is
// held to hand over again when the room is done. It answers nothing for a
// call Deepgram cancelled, or once the session has ended.
func (s *session) answer(ctx context.Context, call *pendingCall, say string) error {
	h := &held{say: say, turn: call.turn}
	if len(wordsOf(say)) > 0 && s.speech.silenced() {
		if err := s.finish(call, answered); err != nil {
			return err
		}
		// The call is answered and its context ended; the words still go.
		w, err := s.inject(ctx, say, false, h, s.sayLimit)
		if w == nil {
			s.log.Info("the answer could not be handed to Deepgram", "call_id", call.fn.ID, "reason", err)
			return nil
		}
		s.awaitAnswer(ctx, call, w)
		return nil
	}
	w, err := s.inject(call.voided, say, false, h, s.sayLimit)
	if w == nil {
		if err != nil {
			s.log.Info("the answer could not be handed to Deepgram", "call_id", call.fn.ID, "reason", err)
		}
		return s.finish(call, notAnswered)
	}
	ferr := s.finish(call, answered)
	s.awaitAnswer(ctx, call, w)
	return ferr
}

// awaitAnswer waits for an answer's words, and logs why they were not spoken.
func (s *session) awaitAnswer(ctx context.Context, call *pendingCall, w *sayWait) {
	switch err := s.await(ctx, w); {
	case errors.Is(err, ErrRefused):
		s.log.Info("Deepgram refused the answer; it is handed over again when the room is done", "call_id", call.fn.ID)
	case err != nil:
		s.log.Info("the answer was not spoken", "call_id", call.fn.ID, "reason", err)
	}
}

// What the model is told of an answer: whether the Concierge's words were
// handed to Deepgram to speak, never the words, so it has nothing to say.
const (
	answered    = `{"answered":true}`
	notAnswered = `{"answered":false}`
)

// forgetTurn frees the turn of a call Deepgram cancelled, so a call it
// makes again for that turn runs.
func (s *session) forgetTurn(call *pendingCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answeredTurn == call.turn {
		s.answeredTurn = -1
	}
}

// claim marks the call waiting on id as being answered, and returns it, or
// nil when no call waits there or one is already being answered.
func (s *session) claim(id string) *pendingCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := s.pending[id]
	if call == nil || call.claimed {
		return nil
	}
	call.claimed = true
	return call
}

// take removes and returns the call waiting on id, or nil.
func (s *session) take(id string) *pendingCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := s.pending[id]
	delete(s.pending, id)
	return call
}

// finish answers call with content, unless Deepgram cancelled it meanwhile.
func (s *session) finish(call *pendingCall, content string) error {
	if s.take(call.fn.ID) == nil {
		return fmt.Errorf("answer %s: %w", call.fn.ID, ErrNoPendingCall)
	}
	defer call.cancel()
	defer call.void()
	return s.respond(&call.fn, content)
}

func (s *session) respond(f *functionCall, content string) error {
	return s.writeJSON(functionCallResponse{
		Type: "FunctionCallResponse", ID: f.ID, Name: f.Name, Content: content, ThoughtSignature: f.ThoughtSignature,
	})
}

func errorContent(msg string) string {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return string(data)
}

// keepAliveLoop tells Deepgram the session is still there.
func (s *session) keepAliveLoop() {
	defer s.workers.Done()
	ticker := time.NewTicker(s.keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.write(websocket.TextMessage, []byte(`{"type":"KeepAlive"}`)); err != nil {
				return
			}
		}
	}
}

// pump sends the room audio to Deepgram: silence while the voice speaks,
// holding the room's last 2 s, which go on after an Interrupt.
func (s *session) pump() {
	defer s.workers.Done()
	var (
		held    = newByteRing(holdLimit)
		scratch []byte
		silence []byte
		warned  bool
	)
	for {
		var f audio.Frame
		var ok bool
		select {
		case <-s.done:
			return
		case f, ok = <-s.tap:
			if !ok {
				return
			}
		}
		if f.Rate != InputRate || f.Channels != 1 {
			if !warned {
				s.log.Error("room audio is not 16 kHz mono; dropping it", "rate", f.Rate, "channels", f.Channels)
				warned = true
			}
			continue
		}
		scratch = encodePCM16(scratch[:0], f.PCM)
		s.holdMu.Lock()
		speaking, flush := s.speaking, s.flush
		echo := !speaking && time.Now().Before(s.quietAt)
		s.flush = false
		s.holdMu.Unlock()
		if flush {
			// Even if a next reply began since the Interrupt: the words
			// that interrupted go on now, not once that reply ends.
			for _, part := range held.parts() {
				if s.write(websocket.BinaryMessage, part) != nil {
					return
				}
			}
			held.reset()
		}
		out := scratch
		switch {
		case speaking:
			held.write(scratch)
			out = silenceOf(&silence, len(scratch))
		case echo:
			// The voice's own last words: nobody interrupted, so not held.
			held.reset()
			out = silenceOf(&silence, len(scratch))
		default:
			held.reset()
		}
		if s.write(websocket.BinaryMessage, out) != nil {
			return
		}
	}
}

// silenceOf returns n bytes of silence from buf, growing it as needed.
func silenceOf(buf *[]byte, n int) []byte {
	if len(*buf) < n {
		*buf = make([]byte, n)
	}
	return (*buf)[:n]
}

// encodePCM16 appends samples to b as PCM16 little-endian.
func encodePCM16(b []byte, samples []int16) []byte {
	for _, v := range samples {
		b = append(b, byte(v), byte(v>>8))
	}
	return b
}
