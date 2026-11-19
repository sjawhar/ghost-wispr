package voice

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// The /ws events the Supervisor sends. Each carries a session id and never
// words, and none is stored.
const (
	EventWakeHeard       = "voice_wake"
	EventExchangeStarted = "voice_exchange_started"
	EventAnswerReady     = "voice_answer_ready"
	EventSpoken          = "voice_spoken"
)

// Defaults for SupervisorConfig's waits.
const (
	// DefaultTurnWait is how long an exchange waits for a word from the asker
	// before it ends with no question, and for Deepgram's endpoint after the
	// last word before it takes the turn as final.
	DefaultTurnWait = 4 * time.Second
	// DefaultSilenceWait is how long the room must be silent before a holder's
	// answer speaks by itself.
	DefaultSilenceWait = 3 * time.Second
	// DefaultIdleWait is how long an exchange with a voice that drives its
	// own turn stays open with nobody and nothing speaking.
	DefaultIdleWait = 10 * time.Second
)

const (
	// bargeInGap is how close two words of someone else's must start to stop
	// the voice: real words over it, not a cough and a word.
	bargeInGap = 0.5
	// echoHold is how long after the voice's speech ends its own words, heard
	// back, still count as its own.
	echoHold = 1500 * time.Millisecond
	// ownWordsKept is how long Wispr's spoken words are kept for the own-speech
	// match.
	ownWordsKept = 30 * time.Second
	// recentKept is how much of the transcript an exchange gets as Recent.
	recentKept = 2 * time.Minute
	// exchangeMax bounds one exchange, whatever its voice or Concierge do.
	exchangeMax = 2 * time.Minute
	// cutKeepsPending is how early in a held answer a stop keeps it pending;
	// cut later, it counts as heard.
	cutKeepsPending = 2 * time.Second
)

// backchannels are the words that alone never stop the voice.
var backchannels = map[string]bool{
	"mm": true, "mmm": true, "mhm": true, "mm-hm": true, "mm-hmm": true, "mmhmm": true, "hmm": true,
	"uh-huh": true, "uh": true, "um": true, "ah": true, "oh": true, "huh": true,
	"yeah": true, "yep": true, "yes": true, "ok": true, "okay": true, "right": true, "sure": true,
	"cool": true, "nice": true, "great": true, "alright": true, "gotcha": true, "thanks": true,
}

// Speaker is the speaker output as the Supervisor uses it; SpeakerOf adapts
// *audio.Output.
type Speaker interface {
	Begin(input string) Playback
	Cancel(input string)
	OnSpeech(fn func(audio.SpeechEvent)) (remove func())
}

// SpeakerOf adapts the speaker output.
func SpeakerOf(o *audio.Output) Speaker { return outputSpeaker{o} }

type outputSpeaker struct{ o *audio.Output }

func (s outputSpeaker) Begin(input string) Playback { return s.o.Begin(input) }
func (s outputSpeaker) Cancel(input string)         { s.o.Cancel(input) }
func (s outputSpeaker) OnSpeech(fn func(audio.SpeechEvent)) func() {
	return s.o.OnSpeech(fn)
}

// TurnDriver is implemented by a Voice whose vendor drives the turn itself
// (voices B and C): the Supervisor then hands the Concierge no turn, and the
// voice calls Exchange.Tools.
type TurnDriver interface {
	DrivesTurn() bool
}

// Transcript is one Deepgram result, as the Supervisor reads it.
type Transcript struct {
	Text         string
	Words        []Word
	IsFinal      bool
	SpeechFinal  bool // Deepgram's endpoint: the speaker's turn is over
	FromFinalize bool // the reply to a Finalize, which a capture pause sends
	Closings     int  // the capture gate's closings count when it arrived
	GateOpen     bool // the capture gate was open when it arrived
	At           time.Time
}

// Word is one word of a Transcript, with Deepgram's speaker label and its
// times on the socket, in seconds.
type Word struct {
	Text       string
	Speaker    int
	Start, End float64
}

// SupervisorConfig wires a Supervisor.
type SupervisorConfig struct {
	Voice       func() Voice // the voice for the next exchange
	Concierge   Concierge
	Speaker     Speaker
	WakePhrases []string
	Session     func() string // the recording session now, "" before one starts
	Red         func() bool   // the light reads red and room audio passes the gate
	Notify      func(kind, sessionID string)
	Tap         *Tap
	TurnWait    time.Duration // 0: DefaultTurnWait
	SilenceWait time.Duration // 0: DefaultSilenceWait
	IdleWait    time.Duration // 0: DefaultIdleWait
	Logf        func(string, ...any)
}

// Supervisor owns every exchange's lifetime: it opens one on the wake phrase,
// hands the asker's turn to the Concierge, and stops it all on a gate closing
// (a pause or a light hold), the Jabra button, "Wispr, stop", barge-in and
// shutdown. One exchange holds the floor at a time, and other speech waits
// for it (WaitFloor). Holders' answers wait for a red light and silence, or,
// after any stop, for a person's "Wispr, go ahead".
//
// Its methods never block on a voice, the Concierge or the speaker: those run
// in goroutines, so a capture pause and the transcript reader are never held.
type Supervisor struct {
	cfg    SupervisorConfig
	wake   *transcript.Detector
	ctx    context.Context
	cancel context.CancelFunc
	notes  chan [2]string
	logf   func(string, ...any)

	unlisten func()

	mu            sync.Mutex
	closed        bool
	closings      int       // the gate's closings count after the latest closing
	ex            *exchange // the exchange holding the floor
	floor         chan struct{}
	lastStopAt    time.Time          // when the latest stop came
	chimeCancel   context.CancelFunc // ends the answer chime playing or about to
	pending       []*pendingAnswer
	voiceSpeaking bool      // the voice input plays
	voiceEnded    time.Time // when it last stopped
	own           []ownWords
	recent        []TurnText
	lastHeard     time.Time // the last word heard
	chimedAt      time.Time
	heard         []heardCall    // Heard calls OnTranscript makes once it unlocks
	wg            sync.WaitGroup // the goroutines Close waits for
}

// heardCall is one Concierge.Heard call, made outside the lock.
type heardCall struct {
	ctx   context.Context
	x     Exchange
	words string
}

type pendingAnswer struct {
	PendingAnswer
	needsInvite bool // a stop came after its question: only "Wispr, go ahead" speaks it
	chimed      bool
}

type ownWords struct {
	words map[string]bool
	at    time.Time
}

// exchange is one exchange: a wake, or a held answer spoken. Its fields are
// guarded by the Supervisor's mu.
type exchange struct {
	ctx       context.Context
	cancel    context.CancelFunc
	x         Exchange
	session   Session
	opened    chan struct{} // closed once Open returned, or failed
	drives    bool          // the voice drives its own turn
	answering bool          // the turn went to the Concierge (or a held answer is spoken)
	answer    *pendingAnswer
	wakeStart float64 // the wake word's start on the socket; later results of its utterance repeat it
	wakeAt    time.Time
	turn      []string // final words of the asker's turn
	interim   string   // the turn's words not final yet
	timer     *time.Timer
	said      bool
	spokeAt   time.Time  // when its speech first started
	last      Playback   // its latest playback on the voice input
	ended     bool       // torn down
	saidMu    sync.Mutex // guards said texts for the answer tool
	firstOut  sync.Once  // logs the time to first audio
	turnAt    time.Time  // when the turn was handed to the Concierge
	notified  map[string]bool
	idle      *time.Timer // a driving voice's idle wait
	// dropsAtOpen is the tap's drop count when the exchange opened.
	dropsAtOpen uint64
}

// NewSupervisor starts a Supervisor; Close stops it.
func NewSupervisor(c *SupervisorConfig) *Supervisor {
	cfg := *c
	if cfg.TurnWait <= 0 {
		cfg.TurnWait = DefaultTurnWait
	}
	if cfg.SilenceWait <= 0 {
		cfg.SilenceWait = DefaultSilenceWait
	}
	if cfg.IdleWait <= 0 {
		cfg.IdleWait = DefaultIdleWait
	}
	if cfg.Notify == nil {
		cfg.Notify = func(string, string) {}
	}
	if cfg.Tap == nil {
		cfg.Tap = NewTap()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	ctx, cancel := context.WithCancel(context.Background())
	floor := make(chan struct{})
	close(floor)
	s := &Supervisor{
		cfg:    cfg,
		wake:   transcript.New(cfg.WakePhrases),
		ctx:    ctx,
		cancel: cancel,
		notes:  make(chan [2]string, 64),
		logf:   logf,
		floor:  floor,
	}
	s.unlisten = cfg.Speaker.OnSpeech(s.onSpeech)
	s.spawn(s.sendNotes)
	s.spawn(s.offerAnswers)
	return s
}

// closeWait bounds how long Close waits for the Supervisor's goroutines.
const closeWait = 2 * time.Second

// Close stops every exchange, as shutdown does, and the Supervisor with it.
// It waits, up to closeWait, for every goroutine it started to return.
func (s *Supervisor) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.stopLocked("shutdown")
	s.closed = true
	s.mu.Unlock()
	s.unlisten()
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeWait):
		s.logf("voice: a goroutine of the voice did not return within %v of shutdown", closeWait)
	}
}

// spawn runs f on a goroutine that Close waits for. A goroutine it starts
// runs while another of its goroutines runs, or before Close.
func (s *Supervisor) spawn(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// GateClosed stops every exchange because the capture gate closed, for a pause
// or a light hold; closings is the gate's closings count after it. A wake in a
// transcript from before it is dropped.
func (s *Supervisor) GateClosed(closings int) {
	s.cfg.Tap.Reset()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closings = max(s.closings, closings)
	s.recent = nil // a delete may follow, and a pause ends the session
	s.stopLocked("capture gate closed")
}

// Stop stops every exchange, as the Jabra button does.
func (s *Supervisor) Stop(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(reason)
}

// SessionEnded drops the held answers of a session that ended: the Concierge
// keeps them for its page and never announces them in a later meeting.
func (s *Supervisor) SessionEnded(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPendingLocked(func(id string) bool { return id == sessionID })
}

// SessionsDeleted forgets deleted sessions: their held answers and the recent
// transcript, and ends an exchange speaking for one of them.
func (s *Supervisor) SessionsDeleted(sessionIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPendingLocked(func(id string) bool { return slices.Contains(sessionIDs, id) })
	s.recent = nil
	if ex := s.ex; ex != nil && slices.Contains(sessionIDs, ex.x.SessionID) {
		s.endLocked(ex, true, "its session was deleted")
	}
}

// SegmentsDeleted forgets the recent transcript, which may hold the deleted
// words, after a segment delete or cut in sessionID.
func (s *Supervisor) SegmentsDeleted(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = nil
}

func (s *Supervisor) dropPendingLocked(of func(sessionID string) bool) {
	s.pending = slices.DeleteFunc(s.pending, func(p *pendingAnswer) bool { return of(p.SessionID) })
}

// dropEndedLocked drops the held answers of every session but the one
// recording now: their meeting ended, so they are never spoken, and they stay
// on their session's page. SessionEnded drops them too; this closes the window
// in which an answer is handed over just as its session ends.
func (s *Supervisor) dropEndedLocked() {
	if len(s.pending) == 0 {
		return
	}
	current := s.cfg.Session()
	s.dropPendingLocked(func(id string) bool { return id != current })
}

// WaitFloor waits until no exchange holds the floor, so other speech (POST
// /api/tts/speak) never plays over an exchange.
func (s *Supervisor) WaitFloor(ctx context.Context) error {
	for {
		s.mu.Lock()
		free := s.floor
		taken := s.ex != nil
		s.mu.Unlock()
		if !taken {
			return nil
		}
		select {
		case <-free:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// OnUtteranceEnd takes Deepgram's UtteranceEnd as the end of the asker's
// turn, as SpeechFinal is.
func (s *Supervisor) OnUtteranceEnd(closings int, gateOpen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !gateOpen || closings < s.closings {
		return
	}
	if ex := s.ex; ex != nil && !ex.answering && s.turnOf(ex) != "" {
		s.handTurnLocked(ex)
	}
}

// OnTranscript reads one Deepgram result: the wake phrase, the asker's turn,
// commands, and barge-in.
func (s *Supervisor) OnTranscript(t *Transcript) {
	s.mu.Lock()
	s.onTranscriptLocked(t)
	heard := s.heard
	s.heard = nil
	s.mu.Unlock()
	// The Concierge's Heard takes its own lock: never under the Supervisor's,
	// so a stop never waits on it. One reader calls OnTranscript, so the calls
	// keep their order.
	for i := range heard {
		s.cfg.Concierge.Heard(heard[i].ctx, &heard[i].x, heard[i].words)
	}
}

func (s *Supervisor) onTranscriptLocked(t *Transcript) {
	// A Finalize reply, a result that arrived while the gate was closed, or one
	// from before the latest closing is about audio from before a stop.
	if s.closed || t.FromFinalize || !t.GateOpen || t.Closings < s.closings || strings.TrimSpace(t.Text) == "" {
		return
	}
	now := time.Now()
	s.lastHeard = now
	own := s.ownLocked(t.Words, now)
	if t.IsFinal && !own {
		s.keepRecentLocked(t, now)
	}

	if m, ok := s.wake.Find(t.Text); ok {
		cmd := transcript.CommandOf(m.Rest)
		if cmd == transcript.Stop {
			// Always, even over Wispr's own words.
			s.stopLocked("Wispr, stop")
			return
		}
		if own {
			return
		}
		ex := s.ex
		same := ex != nil && ex.answer == nil && sameStart(ex.wakeStart, wakeStart(t, m))
		switch {
		case cmd == transcript.GoAhead:
			if ex != nil && ex.answer == nil && !ex.answering && !ex.drives {
				// The wake that this invitation's own interim opened.
				s.endLocked(ex, true, "Wispr, go ahead")
			}
			s.goAheadLocked()
		case same && !ex.answering:
			s.turnLocked(ex, t, m.Rest)
		case same:
			// More of the utterance the exchange already answers.
		case ex != nil && !ex.answering && ex.answer == nil:
			// "Wispr." then "Wispr, what …": the same call.
			ex.wakeStart = wakeStart(t, m)
			ex.turn, ex.interim = nil, ""
			s.turnLocked(ex, t, m.Rest)
		default:
			if ex != nil {
				s.endLocked(ex, true, "a new call")
			}
			s.startLocked(t, m)
		}
		return
	}
	if own {
		return
	}
	ex := s.ex
	if ex == nil {
		return
	}
	s.touchLocked(ex)
	if t.IsFinal && ex.answer == nil && !ex.answering && !ex.drives && s.nameAt(t.Words, ex.wakeStart) {
		// The final still hears the name where the interim wake heard it, but
		// not as a call ("Whisper networks are a problem."): the wake was
		// misheard, and nobody addressed Wispr. A final that dropped the name
		// keeps the call: Deepgram's live finals sometimes do.
		s.endLocked(ex, true, "the wake was misheard")
		return
	}
	if !ex.answering && !ex.drives {
		s.turnLocked(ex, t, t.Text)
		return
	}
	if s.voiceSpeaking && bargesIn(t.Words, s.ownSetLocked(now)) {
		s.logf("voice: barge-in")
		if !ex.drives {
			s.endLocked(ex, true, "barge-in")
			return
		}
		// A voice that drives its own turn stops speaking at once and stays
		// open: its vendor hears the interruption and answers it.
		s.cfg.Speaker.Cancel(audio.InputVoice)
		if sess := ex.session; sess != nil {
			s.spawn(sess.Interrupt)
		}
	}
}

// startLocked opens an exchange on a wake: the wake chime, the voice, and a
// tap of room audio from the wake word on.
func (s *Supervisor) startLocked(t *Transcript, m transcript.Match) {
	if !s.cfg.Red() {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, exchangeMax)
	ex := &exchange{ctx: ctx, cancel: cancel, opened: make(chan struct{}), wakeStart: wakeStart(t, m), wakeAt: t.At, notified: map[string]bool{}}
	voice := s.cfg.Voice()
	if d, ok := voice.(TurnDriver); ok && d.DrivesTurn() {
		ex.drives = true
	}
	ex.dropsAtOpen = s.cfg.Tap.Dropped()
	s.takeFloorLocked(ex)
	sessionID := s.cfg.Session()
	ex.x = Exchange{
		SessionID: sessionID,
		WakeAt:    t.At,
		AskerTurn: strings.TrimSpace(m.Rest),
		Recent:    s.recentLocked(),
		Tap:       s.tapFor(voice, wakeLag(t, m)),
		Out:       exchangeOutput{s: s, ex: ex},
	}
	ex.x.Tools = exchangeTools{s: s, ex: ex}
	s.noteLocked(ex, EventWakeHeard, sessionID)
	s.turnLocked(ex, t, m.Rest)
	x := ex.x
	s.spawn(func() { s.open(ex, voice, &x, true) })
}

// open opens ex's voice and, for a wake, plays the wake chime alongside.
// Other speech on the voice input, such as POST /api/tts/speak, is cut
// first, whether or not it has sounded yet.
func (s *Supervisor) open(ex *exchange, voice Voice, x *Exchange, chime bool) {
	s.cfg.Speaker.Cancel(audio.InputVoice)
	if chime {
		s.spawn(func() { s.playChime(ex.ctx, WakeChime(), ex.wakeAt) })
	}
	sess, err := voice.Open(ex.ctx, x)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(ex.opened)
	if err != nil {
		s.logf("voice: open the voice: %v", err)
		if p := ex.answer; p != nil && !ex.ended {
			// Not heard: it waits for "Wispr, go ahead" again.
			p.needsInvite = true
			s.pending = append([]*pendingAnswer{p}, s.pending...)
		}
		s.endLocked(ex, false, "the voice did not open")
		return
	}
	if ex.ended {
		s.spawn(func() { _ = sess.Close() })
		return
	}
	ex.session = sess
	if ex.drives {
		s.noteLocked(ex, EventExchangeStarted, x.SessionID)
		s.touchLocked(ex)
	}
	s.spawn(func() { s.readEvents(ex, sess) })
}

// playChime plays pcm on the chime input, in 20 ms frames.
func (s *Supervisor) playChime(ctx context.Context, pcm []byte, since time.Time) {
	pb := s.cfg.Speaker.Begin(audio.InputChime)
	const frame = 960 // 20 ms at 24 kHz mono PCM16
	for off := 0; off < len(pcm); off += frame {
		if off == 0 && !since.IsZero() {
			s.logf("voice: chime queued %v after the wake transcript", time.Since(since).Round(time.Millisecond))
		}
		if err := pb.Write(ctx, pcm[off:min(off+frame, len(pcm))], ChimeFormat); err != nil {
			return
		}
	}
}

// readEvents follows a session's events until they end. A voice that drives
// its own turn and fails ends its exchange, as does any voice whose events
// end before the Supervisor closed it.
func (s *Supervisor) readEvents(ex *exchange, sess Session) {
	for e := range sess.Events() {
		s.mu.Lock()
		switch e.Kind {
		case EventAgentText:
			s.addOwnLocked(e.Text, time.Now())
		case EventSpeechStarted:
			if ex.spokeAt.IsZero() {
				ex.spokeAt = time.Now()
			}
			s.touchLocked(ex)
		case EventSpeechEnded:
			if ex.drives && !ex.spokeAt.IsZero() && !ex.ended {
				s.noteLocked(ex, EventSpoken, ex.x.SessionID)
			}
			s.touchLocked(ex)
		case EventErr:
			s.logf("voice: %v", e.Err)
			if ex.drives && s.ex == ex {
				s.endLocked(ex, true, "the voice failed")
			}
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex == ex {
		s.endLocked(ex, true, "the voice ended by itself")
	}
}

// touchLocked restarts the idle wait of an exchange whose voice drives its own
// turn: something was said, by the room or the voice.
func (s *Supervisor) touchLocked(ex *exchange) {
	if !ex.drives || ex.ended {
		return
	}
	if ex.idle != nil {
		ex.idle.Stop()
	}
	ex.idle = time.AfterFunc(s.cfg.IdleWait, func() { s.idleWaited(ex) })
}

// idleWaited ends an exchange nobody and nothing spoke in for IdleWait.
func (s *Supervisor) idleWaited(ex *exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex != ex || ex.ended {
		return
	}
	if s.voiceSpeaking {
		s.touchLocked(ex)
		return
	}
	s.endLocked(ex, false, "idle")
}

// turnLocked adds words of the asker's turn, and hands the turn to the
// Concierge once Deepgram marks it final.
func (s *Supervisor) turnLocked(ex *exchange, t *Transcript, words string) {
	words = strings.TrimSpace(words)
	if t.IsFinal {
		if words != "" {
			ex.turn = append(ex.turn, words)
		}
		ex.interim = ""
	} else {
		ex.interim = words
	}
	// The recording session starts with Deepgram's first final, often after
	// the wake: the exchange takes its id as soon as there is one.
	if ex.x.SessionID == "" {
		ex.x.SessionID = s.cfg.Session()
	}
	turn := s.turnOf(ex)
	ex.x.AskerTurn = turn
	if turn != "" {
		s.heard = append(s.heard, heardCall{ctx: ex.ctx, x: ex.x, words: turn})
	}
	if ex.drives {
		return // the vendor drives the turn
	}
	if t.IsFinal && t.SpeechFinal && s.turnOf(ex) != "" {
		s.handTurnLocked(ex)
		return
	}
	if ex.timer != nil {
		ex.timer.Stop()
	}
	ex.timer = time.AfterFunc(s.cfg.TurnWait, func() { s.turnWaited(ex) })
}

func (s *Supervisor) turnOf(ex *exchange) string {
	return strings.TrimSpace(strings.Join(append(ex.turn[:len(ex.turn):len(ex.turn)], ex.interim), " "))
}

// turnWaited ends an exchange nobody asked anything in, or takes what the
// asker said as the turn when Deepgram's endpoint never came.
func (s *Supervisor) turnWaited(ex *exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex != ex || ex.answering || ex.drives {
		return
	}
	if s.turnOf(ex) == "" {
		s.endLocked(ex, false, "no question after the wake phrase")
		return
	}
	s.handTurnLocked(ex)
}

// handTurnLocked hands the asker's turn to the Concierge.
func (s *Supervisor) handTurnLocked(ex *exchange) {
	if ex.answering || ex.drives {
		return
	}
	if ex.timer != nil {
		ex.timer.Stop()
	}
	ex.answering = true
	ex.turnAt = time.Now()
	turn := s.turnOf(ex)
	ex.x.AskerTurn = turn
	if ex.x.SessionID == "" {
		ex.x.SessionID = s.cfg.Session()
	}
	ex.x.Recent = s.recentLocked()
	s.noteLocked(ex, EventExchangeStarted, ex.x.SessionID)
	x := ex.x
	s.spawn(func() { s.answer(ex, &x, turn) })
}

// answer runs the Concierge on the turn and speaks what it says, then ends
// the exchange once the speech has played.
func (s *Supervisor) answer(ex *exchange, x *Exchange, turn string) {
	select {
	case <-ex.opened:
	case <-ex.ctx.Done():
		return
	}
	s.mu.Lock()
	sess := ex.session
	s.mu.Unlock()
	if sess == nil {
		return
	}
	say := func(ctx context.Context, text string, mode SayMode) error {
		s.mu.Lock()
		s.addOwnLocked(text, time.Now())
		s.mu.Unlock()
		if err := sess.Say(ctx, text, mode); err != nil {
			return err
		}
		s.mu.Lock()
		ex.said = true
		s.mu.Unlock()
		return nil
	}
	err := s.cfg.Concierge.Answer(ex.ctx, x, turn, say)
	if err != nil && ex.ctx.Err() == nil {
		s.logf("voice: the Concierge did not answer: %v", err)
		s.mu.Lock()
		said := ex.said
		s.mu.Unlock()
		if !said {
			_ = say(ex.ctx, "Sorry, I could not answer that.", Ack)
		}
	}
	s.finish(ex, x.SessionID)
}

// finish waits until ex's speech has played, then ends ex. It reports
// whether ex spoke and played to its end, with no stop.
func (s *Supervisor) finish(ex *exchange, sessionID string) bool {
	s.mu.Lock()
	last, said := ex.last, ex.said
	s.mu.Unlock()
	played := true
	if said && last != nil {
		played = last.Drain(ex.ctx) == nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex != ex || ex.ended {
		return false
	}
	spoken := said && played && ex.ctx.Err() == nil
	if spoken {
		s.noteLocked(ex, EventSpoken, sessionID)
	}
	s.endLocked(ex, false, "done")
	return spoken
}

// goAheadLocked speaks the oldest held answer, on "Wispr, go ahead" while
// the light reads red.
func (s *Supervisor) goAheadLocked() {
	s.dropEndedLocked()
	if !s.cfg.Red() || s.ex != nil || len(s.pending) == 0 {
		return
	}
	p := s.pending[0]
	p.needsInvite = false
	s.speakLocked(p)
}

// speakLocked speaks a held answer in an exchange of its own.
func (s *Supervisor) speakLocked(p *pendingAnswer) {
	s.pending = slices.DeleteFunc(s.pending, func(q *pendingAnswer) bool { return q == p })
	ctx, cancel := context.WithTimeout(s.ctx, exchangeMax)
	ex := &exchange{ctx: ctx, cancel: cancel, opened: make(chan struct{}), answering: true, answer: p, wakeStart: -1, notified: map[string]bool{}}
	ex.dropsAtOpen = s.cfg.Tap.Dropped()
	s.takeFloorLocked(ex)
	voice := s.cfg.Voice()
	ex.x = Exchange{SessionID: p.SessionID, WakeAt: time.Now(), Recent: s.recentLocked(), Tap: s.tapFor(voice, 0), Out: exchangeOutput{s: s, ex: ex}}
	ex.x.Tools = exchangeTools{s: s, ex: ex}
	x := ex.x
	s.spawn(func() {
		s.open(ex, voice, &x, false)
		select {
		case <-ex.opened:
		case <-ex.ctx.Done():
			return
		}
		s.mu.Lock()
		sess := ex.session
		s.addOwnLocked(p.Text, time.Now())
		s.mu.Unlock()
		if sess == nil {
			return
		}
		if err := sess.Say(ex.ctx, p.Text, Answer); err != nil {
			s.logf("voice: speak a held answer: %v", err)
			// Not heard: it waits for "Wispr, go ahead" again, unless a stop
			// already put it back.
			s.mu.Lock()
			if !ex.ended && ex.ctx.Err() == nil {
				p.needsInvite = true
				s.pending = append([]*pendingAnswer{p}, s.pending...)
			}
			s.mu.Unlock()
		} else {
			s.mu.Lock()
			ex.said = true
			s.mu.Unlock()
		}
		if s.finish(ex, p.SessionID) {
			s.spoken(p.TaskID)
		}
	})
}

// tapFor opens the tap for voice when it drives its own turn and so hears the
// room (voices B and C), replaying replay of the ring. Voice A never reads room
// audio, so it gets none: nothing is copied or dropped for it.
func (s *Supervisor) tapFor(voice Voice, replay time.Duration) <-chan audio.Frame {
	if d, ok := voice.(TurnDriver); !ok || !d.DrivesTurn() {
		return nil
	}
	return s.cfg.Tap.Open(replay)
}

func (s *Supervisor) spoken(taskID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.cfg.Concierge.Spoken(ctx, taskID); err != nil {
		s.logf("voice: record a held answer as spoken: %v", err)
	}
}

// offerAnswers takes holders' answers as they land, and speaks each once the
// light is red and the room silent, unless a stop came after its question.
func (s *Supervisor) offerAnswers() {
	tick := time.NewTicker(min(s.cfg.SilenceWait/4, 250*time.Millisecond))
	defer tick.Stop()
	answers := s.cfg.Concierge.Answers()
	for {
		select {
		case <-s.ctx.Done():
			return
		case a, ok := <-answers:
			if !ok {
				answers = nil
				continue
			}
			s.mu.Lock()
			if !s.closed {
				// It waits for "Wispr, go ahead" when a stop came after its
				// question, or it does not say when that was.
				stoppedSince := !s.lastStopAt.IsZero() && !s.lastStopAt.Before(a.AskedAt)
				s.pending = append(s.pending, &pendingAnswer{PendingAnswer: a, needsInvite: a.AskedAt.IsZero() || stoppedSince})
				select {
				case s.notes <- [2]string{EventAnswerReady, a.SessionID}:
				default:
					s.logf("voice: dropped a %s event", EventAnswerReady)
				}
			}
			s.mu.Unlock()
		case <-tick.C:
		}
		s.offer()
	}
}

// offer chimes each held answer once while the light reads red, and speaks by
// itself the oldest one that needs no invitation, once the room is silent.
func (s *Supervisor) offer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropEndedLocked()
	if s.closed || s.ex != nil || len(s.pending) == 0 || !s.cfg.Red() {
		return
	}
	now := time.Now()
	for _, p := range s.pending {
		if p.chimed {
			continue
		}
		p.chimed = true
		s.chimedAt = now
		ctx, cancel := context.WithCancel(s.ctx)
		s.chimeCancel = cancel
		s.spawn(func() {
			defer cancel()
			s.playChime(ctx, AnswerChime(), time.Time{})
		})
		return // one chime at a time; the next offer chimes the next
	}
	quietSince := s.lastHeard
	for _, t := range []time.Time{s.voiceEnded, s.chimedAt} {
		if t.After(quietSince) {
			quietSince = t
		}
	}
	if s.voiceSpeaking || now.Sub(quietSince) < s.cfg.SilenceWait {
		return
	}
	for _, p := range s.pending {
		if !p.needsInvite {
			s.speakLocked(p)
			return
		}
	}
}

// stopLocked ends the exchange holding the floor for reason, and makes every
// held answer wait for an invitation.
func (s *Supervisor) stopLocked(reason string) {
	s.lastStopAt = time.Now()
	for _, p := range s.pending {
		p.needsInvite = true
	}
	if s.chimeCancel != nil {
		s.chimeCancel() // an answer chime, even one whose playback has not begun
		s.chimeCancel = nil
	}
	if ex := s.ex; ex != nil {
		s.endLocked(ex, true, reason)
		return
	}
	s.cfg.Speaker.Cancel(audio.InputChime)
}

// endLocked ends ex. cut says a stop or a barge-in ended it: its voice and
// chime are cut at once. A held answer cut in its first 2 s stays pending.
func (s *Supervisor) endLocked(ex *exchange, cut bool, reason string) {
	if ex.ended {
		return
	}
	ex.ended = true
	if ex.timer != nil {
		ex.timer.Stop()
	}
	if ex.idle != nil {
		ex.idle.Stop()
	}
	if s.ex == ex {
		s.ex = nil
		close(s.floor)
	}
	s.cfg.Tap.CloseOut()
	if dropped := s.cfg.Tap.Dropped() - ex.dropsAtOpen; dropped > 0 {
		s.logf("voice: the tap dropped %d frames of room audio the voice read too slowly", dropped)
	}
	if cut && ex.answer != nil {
		if ex.spokeAt.IsZero() || time.Since(ex.spokeAt) < cutKeepsPending {
			ex.answer.needsInvite = true
			s.pending = append([]*pendingAnswer{ex.answer}, s.pending...)
		} else {
			s.spawn(func() { s.spoken(ex.answer.TaskID) })
		}
	}
	sess := ex.session
	s.logf("voice: exchange ended (%s)", reason)
	// Its context first, then the speaker, both at once under the lock: a
	// chime or a speech whose playback begins after this cannot play, and a
	// cut landing late can never end the next exchange's chime. The voice's
	// own interrupt and close may wait on its vendor, so they run apart.
	ex.cancel()
	if cut {
		s.cfg.Speaker.Cancel(audio.InputVoice)
		s.cfg.Speaker.Cancel(audio.InputChime)
	}
	s.spawn(func() {
		if sess == nil {
			return
		}
		if cut {
			sess.Interrupt()
		}
		if err := sess.Close(); err != nil {
			s.logf("voice: close the voice: %v", err)
		}
	})
}

// takeFloorLocked makes ex the exchange holding the floor.
func (s *Supervisor) takeFloorLocked(ex *exchange) {
	s.ex = ex
	s.floor = make(chan struct{})
}

// noteLocked queues a /ws event for ex, once per kind.
func (s *Supervisor) noteLocked(ex *exchange, kind, sessionID string) {
	if ex.notified[kind] {
		return
	}
	ex.notified[kind] = true
	select {
	case s.notes <- [2]string{kind, sessionID}:
	default:
		s.logf("voice: dropped a %s event", kind)
	}
}

func (s *Supervisor) sendNotes() {
	for {
		select {
		case n := <-s.notes:
			s.cfg.Notify(n[0], n[1])
		case <-s.ctx.Done():
			return
		}
	}
}

// onSpeech follows the voice input's speech, for barge-in and the own-speech
// filter.
func (s *Supervisor) onSpeech(e audio.SpeechEvent) {
	if e.Input != audio.InputVoice {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.voiceSpeaking = e.Started
	if !e.Started {
		s.voiceEnded = time.Now()
	}
	if ex := s.ex; ex != nil {
		s.touchLocked(ex)
		if e.Started && !ex.turnAt.IsZero() {
			ex.firstOut.Do(func() {
				s.logf("voice: first spoken audio %v after the turn ended", e.At.Sub(ex.turnAt).Round(time.Millisecond))
			})
		}
	}
}

// addOwnLocked keeps the words of text, which Wispr speaks, for the own-speech
// filter.
func (s *Supervisor) addOwnLocked(text string, now time.Time) {
	words := map[string]bool{}
	for _, w := range strings.Fields(text) {
		if n := normWord(w); n != "" {
			words[n] = true
		}
	}
	kept := s.own[:0]
	for _, o := range s.own {
		if now.Sub(o.at) < ownWordsKept {
			kept = append(kept, o)
		}
	}
	kept = append(kept, ownWords{words: words, at: now})
	s.own = kept
}

// ownSetLocked returns Wispr's recent words while its speech can still be
// heard back, else nil.
func (s *Supervisor) ownSetLocked(now time.Time) map[string]bool {
	if !s.voiceSpeaking && now.Sub(s.voiceEnded) >= echoHold {
		return nil
	}
	set := map[string]bool{}
	for _, o := range s.own {
		if now.Sub(o.at) < ownWordsKept {
			for w := range o.words {
				set[w] = true
			}
		}
	}
	return set
}

// ownLocked reports whether words are Wispr's own, heard back: most of them
// are words it is speaking, or just spoke.
func (s *Supervisor) ownLocked(words []Word, now time.Time) bool {
	set := s.ownSetLocked(now)
	if len(set) == 0 || len(words) == 0 {
		return false
	}
	matched := 0
	for _, w := range words {
		if set[normWord(w.Text)] {
			matched++
		}
	}
	return matched*10 >= 6*len(words)
}

// bargesIn reports two words, not backchannels and not Wispr's own, that
// start within bargeInGap of each other.
func bargesIn(words []Word, own map[string]bool) bool {
	var prev *Word
	for i := range words {
		n := normWord(words[i].Text)
		if n == "" || backchannels[n] || own[n] {
			continue
		}
		if prev != nil && words[i].Start-prev.Start <= bargeInGap {
			return true
		}
		prev = &words[i]
	}
	return false
}

func (s *Supervisor) keepRecentLocked(t *Transcript, now time.Time) {
	speaker := -1
	if len(t.Words) > 0 {
		speaker = t.Words[0].Speaker
	}
	kept := s.recent[:0]
	for _, r := range s.recent {
		if now.Sub(r.At) < recentKept {
			kept = append(kept, r)
		}
	}
	kept = append(kept, TurnText{Speaker: speaker, Text: strings.TrimSpace(t.Text), At: now})
	s.recent = kept
}

func (s *Supervisor) recentLocked() []TurnText {
	return append([]TurnText(nil), s.recent...)
}

// wakeStart is the start on the socket of the wake phrase's first word in t.
func wakeStart(t *Transcript, m transcript.Match) float64 {
	rest := len(strings.Fields(m.Rest))
	phrase := len(strings.Fields(m.Phrase))
	i := len(t.Words) - rest - phrase
	if i < 0 || i >= len(t.Words) {
		if len(t.Words) > 0 {
			return t.Words[0].Start
		}
		return 0
	}
	return t.Words[i].Start
}

// wakeLag is how far back from the end of t's audio the wake word started:
// the tap replays that much, so the voice hears no audio from before it.
func wakeLag(t *Transcript, m transcript.Match) time.Duration {
	if len(t.Words) == 0 {
		return 0
	}
	lag := t.Words[len(t.Words)-1].End - wakeStart(t, m)
	return max(0, time.Duration(lag*float64(time.Second)))
}

// nameAt reports whether words hold the name at stream time at, within the
// 0.3 s Deepgram's finals may move a word from where an interim put it.
func (s *Supervisor) nameAt(words []Word, at float64) bool {
	for _, w := range words {
		if w.Start >= at-0.3 && w.Start <= at+0.3 && s.wake.IsName(w.Text) {
			return true
		}
	}
	return false
}

func sameStart(a, b float64) bool {
	d := a - b
	return d > -0.05 && d < 0.05
}

func normWord(w string) string {
	return strings.ToLower(strings.Trim(w, ".,!?;:\"'()[]"))
}

// exchangeOutput is ex's voice input: it keeps ex's latest playback, so the
// Supervisor can wait for its speech to play.
type exchangeOutput struct {
	s  *Supervisor
	ex *exchange
}

func (o exchangeOutput) Begin() Playback {
	pb := o.s.cfg.Speaker.Begin(audio.InputVoice)
	o.s.mu.Lock()
	o.ex.last = pb
	o.s.mu.Unlock()
	return pb
}

// exchangeTools runs the Concierge's tools for ex, and the answer tool
// through Concierge.Answer.
type exchangeTools struct {
	s  *Supervisor
	ex *exchange
}

func (t exchangeTools) Run(ctx context.Context, c ToolCall) (json.RawMessage, error) {
	s := t.s
	s.mu.Lock()
	if t.ex.x.SessionID == "" {
		t.ex.x.SessionID = s.cfg.Session()
	}
	x := t.ex.x
	s.mu.Unlock()
	if c.Name != AnswerTool {
		return s.cfg.Concierge.Tools(&x).Run(ctx, c)
	}
	var args AnswerArgs
	if err := json.Unmarshal(c.Args, &args); err != nil {
		return nil, errors.New("answer tool: arguments are not an AnswerArgs object")
	}
	if strings.TrimSpace(args.Question) == "" {
		// A vendor may delegate before its own transcript of the turn.
		args.Question = x.AskerTurn
	}
	var said []string
	err := s.cfg.Concierge.Answer(ctx, &x, args.Question, func(_ context.Context, text string, _ SayMode) error {
		t.ex.saidMu.Lock()
		defer t.ex.saidMu.Unlock()
		said = append(said, text)
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.ex.saidMu.Lock()
	defer t.ex.saidMu.Unlock()
	return json.Marshal(AnswerResult{Say: strings.Join(said, " ")})
}
