package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// DefaultSoundGap is the gap with no transcript word the answer sound
	// waits for (R10).
	DefaultSoundGap = time.Second
	// DefaultFollowUpWait is how long an exchange stays open after Wispr's
	// answer for a follow-up with no wake phrase (R10).
	DefaultFollowUpWait = 6 * time.Second
	// DefaultReplayWindow is how early in a held answer a stop keeps it
	// pending; cut later, it counts as heard (R11).
	DefaultReplayWindow = 2 * time.Second
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
	// sorryLine is said for a failed answer when the Concierge said nothing.
	sorryLine = "Sorry, I could not answer that."
)

// recentKept is how much of the transcript an exchange gets as Recent; a
// test shortens it.
var recentKept = 2 * time.Minute

// exchangeMax bounds one exchange, whatever its voice or Concierge do; a test
// shortens it.
var exchangeMax = 5 * time.Minute

// capExchange ends ex when it reaches exchangeMax, and says so: a voice whose
// session ends with the context would log it as ending by itself.
func (s *Supervisor) capExchange(ex *exchange) {
	limit := exchangeMax
	context.AfterFunc(ex.ctx, func() {
		if !errors.Is(ex.ctx.Err(), context.DeadlineExceeded) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ex == ex && !ex.ended {
			s.endLocked(ex, true, fmt.Sprintf("it reached its limit of %v", limit))
		}
	})
}

// backchannels are R11's words that alone never stop the voice: "mm-hm",
// "uh-huh", "yeah", "right" and "okay", in the spellings Deepgram writes.
var backchannels = map[string]bool{
	"mm-hm": true, "mm-hmm": true, "mhm": true, "mmhmm": true, "mm": true, "mmm": true,
	"uh-huh": true, "uh-uh": true, "uhhuh": true,
	"yeah": true, "yea": true,
	"right": true,
	"okay":  true, "ok": true,
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
	// Voice builds the voice for the next exchange: it is read at each
	// exchange, so a change of voice applies from the next one.
	Voice       func() (Voice, error)
	Concierge   Concierge
	Speaker     Speaker
	WakePhrases []string
	Session     func() string // the recording session now, "" before one starts
	Red         func() bool   // the light reads red and room audio passes the gate
	// Enabled reports voice.enabled, read on each result; nil is always on.
	Enabled func() bool
	Notify  func(kind, sessionID string)
	// ProjectKeys are Dispatch's project keys, whose spoken keys the turn
	// writes as keys; nil writes none. It must return at once.
	ProjectKeys func() []string
	// WaitingLight is told On while something waits for a person, and Off
	// once nothing does (Supervisor.waitingLocked); nil shows nothing.
	WaitingLight WaitingLight
	Tap          *Tap
	TurnWait     time.Duration // 0: DefaultTurnWait
	SilenceWait  time.Duration // 0: DefaultSilenceWait
	IdleWait     time.Duration // 0: DefaultIdleWait
	SoundGap     time.Duration // 0: DefaultSoundGap
	FollowUpWait time.Duration // 0: DefaultFollowUpWait
	ReplayWindow time.Duration // 0: DefaultReplayWindow
	// Timing gets the timing events the acceptance driver reads; nil drops
	// them.
	Timing func(event, sessionID string, at time.Time, kv ...any)
	// HandDone is told how each raised hand came down (RaiseHand); nil
	// tells nobody. It runs on a goroutine of its own, never under the
	// Supervisor's lock.
	HandDone func(h *Hand, outcome HandOutcome)
	// Segment gets each final transcript segment of the room, for the joined
	// agents' pointers; nil drops them. It runs on the transcript reader,
	// never under the Supervisor's lock, and must return at once.
	Segment func(s *Segment)
	// Meeting is the context of a recording session's meeting, which the
	// recorder cancels in the same step that ends the session; an exchange
	// ends with it. nil leaves an exchange to the session-ended hook. It runs
	// under the Supervisor's lock, so it must return at once and never call
	// the Supervisor.
	Meeting func(sessionID string) context.Context
	Logf    func(string, ...any)
	// followUpAfter starts a driving voice's follow-up window timer:
	// time.AfterFunc, unless a test of this package sets it to run the
	// window's expiry when the test says.
	followUpAfter func(d time.Duration, f func()) *time.Timer
}

// Supervisor owns every exchange's lifetime: it opens one on the wake phrase,
// hands the asker's turn to the Concierge, and stops it all on a gate closing
// (a pause or a light hold), the Jabra button, "Wispr, stop", barge-in and
// shutdown. One exchange holds the floor at a time, and other speech waits
// for it (WithFloor). Holders' answers wait for a red light and a free floor,
// or for a person's "Wispr, go ahead" (R7).
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

	mu         sync.Mutex
	closed     bool
	off        bool      // the voice is turned off, and offLocked has stopped it
	closings   int       // the gate's closings count after the latest closing
	ex         *exchange // the exchange holding the floor
	floor      chan struct{}
	lastStopAt time.Time // when the latest stop came
	// The notification sound being made, the answer sound or the hand
	// sound: chimeCancel ends it, nil while none is; chimeOwners are the
	// answers or hands it sounds for. It is theirs from its first frame
	// until the speaker has played its last (chimeDraining once every frame
	// is written), however late the speaker plays it.
	chimeCancel   context.CancelFunc
	chimeCtx      *soundContext
	chimeHands    bool
	chimeOwners   []*pendingAnswer
	chimeDraining bool
	// errorSounds are the error sounds scheduled and neither played out nor
	// cut: a cut of the chime input ends each of them, begun or not
	// (cancelChimeLocked), and one still here when its meeting ends cuts
	// what is queued of it itself.
	errorSounds   map[*soundContext]struct{}
	pending       []*pendingAnswer
	arrivals      uint64    // answers taken so far; each held answer's arrived
	lit           bool      // the waiting light was last told On (showWaitingLocked)
	voiceSpeaking bool      // the voice input plays
	voiceStarted  time.Time // when it last started
	voiceEnded    time.Time // when it last stopped
	own           []ownWords
	recent        []TurnText
	lastHeard     time.Time      // the last word heard
	lastRoomWord  time.Time      // the last word heard that was not Wispr's own
	lastTimes     *exchangeTimes // the newest exchange a wake opened: its times, no words
	chimedAt      time.Time
	heard         []heardCall // Heard calls OnTranscript makes once it unlocks
	segments      []Segment   // Segment calls OnTranscript makes once it unlocks
	// running counts the goroutines spawn started that Close waits for.
	// Once Close began and found none running, it closes idle, and a
	// goroutine spawn starts after that runs untracked: a WaitGroup's Add
	// from zero while its Wait runs is a race, and spawn runs on callers that
	// are no goroutine of the Supervisor's (LowerHands from the roster). All
	// three are guarded by spawnMu.
	spawnMu  sync.Mutex
	running  int
	closing  bool
	idle     chan struct{}
	idleDone bool // idle is closed

	// commands counts a person's commands on held answers: each "Wispr,
	// never mind" and each "Wispr, go ahead". neverMinds is how many never
	// minds still wait for the Concierge, and lastNeverMind closes when the
	// newest is done, so each decides after the one before it.
	commands      uint64
	neverMinds    int
	lastNeverMind chan struct{}
}

// heardCall is one Concierge.Heard call, made outside the lock.
type heardCall struct {
	ctx   context.Context
	x     Exchange
	words string
}

// pendingAnswer is a holder's answer or a joined agent's raised hand (hand)
// that waits to be spoken, or a line said once in an exchange of its own
// (line), such as who has a hand up, which is never held.
type pendingAnswer struct {
	PendingAnswer
	hand        *Hand
	line        bool
	needsInvite bool // a stop or a room word came after its question, or it is a hand: only "Wispr, go ahead" speaks it
	chimed      bool
	// arrived orders held answers by arrival: an answer cut early goes back
	// to the front of pending, and keeps its place in time.
	arrived uint64
	// takenAt is commands when a "Wispr, go ahead" last picked it, 0 before:
	// a never mind said before that no longer drops it.
	takenAt uint64
}

type ownWords struct {
	words map[string]bool
	at    time.Time
}

// exchangeTimes are when an exchange's wake and its last turn were; they hold
// no words, so they may outlive the exchange and a delete.
type exchangeTimes struct {
	wakeAt, lastTurnEnd time.Time
}

// exchange is one exchange: a wake, or a held answer spoken. Its fields are
// guarded by the Supervisor's mu.
type exchange struct {
	ctx    context.Context
	cancel context.CancelFunc
	// authority is ctx as an exchangeContext, which bindMeetingLocked binds
	// to the recorder's meeting; nil for an exchange made otherwise.
	authority    *exchangeContext
	x            Exchange
	session      Session
	opened       chan struct{} // closed once Open returned, or failed
	drives       bool          // the voice drives its own turn
	answering    bool          // the turn went to the Concierge (or a held answer is spoken)
	answer       *pendingAnswer
	wakeStart    float64 // the wake word's start on the socket; later results of its utterance repeat it
	wakeAt       time.Time
	turn         []string // final words of the asker's turn
	interim      string   // the turn's words not final yet
	timer        *time.Timer
	said         bool
	spokeAt      time.Time  // when its speech first started
	last         Playback   // its latest playback on the voice input
	ended        bool       // torn down
	saidMu       sync.Mutex // guards said texts for the answer tool
	firstOutDone bool       // the turn's first audio was reported
	factPending  bool       // a fact was queued while nothing played: its audio is the next start
	factDone     bool       // the turn's first fact audio was reported
	factText     string     // that fact, for a voice that reports when a text plays
	followUp     bool       // the exchange waits for a follow-up after Wispr's answer
	// followUpTurn: the turn now heard began in the follow-up window with no
	// wake phrase, so it continues the exchange only as a question (R10).
	followUpTurn bool
	turnFinal    bool // the turn now heard has a final result
	// askedBack: Wispr's last line asked a question ("Did you mean A or
	// B?"), so the next utterance answers it and continues the exchange.
	askedBack bool
	// times are when its wake and last turn were: R7's t0 for an answer to a
	// question it asked. The Supervisor keeps the newest exchange's after it
	// ends, and they hold no words.
	times *exchangeTimes
	// unspokenAnswer: the answer tool returned words the driving voice has
	// not begun to speak.
	unspokenAnswer bool
	turnAt         time.Time // when the turn was handed to the Concierge
	notified       map[string]bool
	idle           *time.Timer // a driving voice's idle wait
	// A driving voice's follow-up window: armed when its speech ends, unless
	// a barge-in cut it; a person's word disarms it (R10).
	followUpTimer *time.Timer
	followUpGen   int  // a stale window's timer finds it changed
	cutSpeech     bool // a barge-in cut the driving voice's speech
	newTurn       bool // the driving voice spoke: the room's next words are a new turn
	toolCalls     int  // answer tools still running: the exchange stays open
	// dropsAtOpen is the tap's drop count when the exchange opened.
	dropsAtOpen uint64
	// sayCtx, when set, is the context of a hand's words, derived from its
	// membership's: its end stops them, and exchangeOutput refuses any frame
	// written once it ended.
	sayCtx context.Context
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
	if cfg.SoundGap <= 0 {
		cfg.SoundGap = DefaultSoundGap
	}
	if cfg.FollowUpWait <= 0 {
		cfg.FollowUpWait = DefaultFollowUpWait
	}
	if cfg.followUpAfter == nil {
		cfg.followUpAfter = time.AfterFunc
	}
	if cfg.ReplayWindow <= 0 {
		cfg.ReplayWindow = DefaultReplayWindow
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
		idle:   make(chan struct{}),
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
	s.showWaitingLocked()
	s.mu.Unlock()
	s.unlisten()
	s.cancel()
	s.spawnMu.Lock()
	s.closing = true
	s.idleIfDoneLocked()
	s.spawnMu.Unlock()
	select {
	case <-s.idle:
	case <-time.After(closeWait):
		s.logf("voice: a goroutine of the voice did not return within %v of shutdown", closeWait)
	}
}

// spawn runs f on a goroutine that Close waits for, unless Close already
// found none of them running: then f runs untracked, as the shutdown no
// longer waits.
func (s *Supervisor) spawn(f func()) {
	s.spawnMu.Lock()
	tracked := !s.idleDone
	if tracked {
		s.running++
	}
	s.spawnMu.Unlock()
	go func() {
		if tracked {
			defer s.spawned()
		}
		f()
	}()
}

// spawned counts a tracked goroutine's return.
func (s *Supervisor) spawned() {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	s.running--
	s.idleIfDoneLocked()
}

// idleIfDoneLocked closes idle once Close began and no tracked goroutine
// runs. spawnMu is held.
func (s *Supervisor) idleIfDoneLocked() {
	if s.closing && s.running == 0 && !s.idleDone {
		s.idleDone = true
		close(s.idle)
	}
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

// Started plays the start sound: capture moved into recording, from any
// control. It plays with the voice off too (R13).
func (s *Supervisor) Started() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.soundLocked(StartChime(), "start_sound", "")
}

// RedCheckFailed plays the error sound: the light did not read back red. It
// plays with the voice off too (R13).
func (s *Supervisor) RedCheckFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.soundLocked(ErrorChime(), "error_sound", "")
}

// errorLocked plays the error sound, for a failed voice, a failed vendor or a
// Concierge that cannot reach Dispatch or Envoy (R10).
func (s *Supervisor) errorLocked(sessionID string) {
	s.soundLocked(ErrorChime(), "error_sound", sessionID)
}

// soundLocked plays pcm on the chime input at once, unless the Supervisor is
// closed. A sound for a meeting (sessionID) is that meeting's until the
// speaker has played its last frame: it is written and drained under the
// meeting's context (SupervisorConfig.Meeting), read at each frame, so none
// of it is written once the recorder ended the meeting, and its producer then
// cuts what is still queued of it. It is in errorSounds from now until it
// plays out: a cut of the chime input meanwhile ends it whether or not its
// playback has begun, and then it cuts nothing more, so a sound begun after
// that cut, a newer one, plays on.
func (s *Supervisor) soundLocked(pcm []byte, event, sessionID string) {
	if s.closed {
		return
	}
	var meeting context.Context
	if s.cfg.Meeting != nil && sessionID != "" {
		meeting = s.cfg.Meeting(sessionID)
	}
	ctx := newSoundContext(s.ctx, meeting, nil)
	if s.errorSounds == nil {
		s.errorSounds = map[*soundContext]struct{}{}
	}
	s.errorSounds[ctx] = struct{}{}
	s.spawn(func() {
		defer ctx.cancel()
		pb, err := s.playChime(ctx, pcm, event, sessionID)
		if err == nil {
			err = pb.Drain(ctx)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		_, uncut := s.errorSounds[ctx]
		delete(s.errorSounds, ctx)
		if err != nil && uncut && ctx.Err() != nil && !s.closed {
			s.cutChimeLocked(true) // its meeting ended it: what is still queued of it plays no further
		}
	})
}

// timing reports a timing event, for the acceptance driver.
func (s *Supervisor) timing(event, sessionID string, at time.Time, kv ...any) {
	if s.cfg.Timing != nil {
		s.cfg.Timing(event, sessionID, at, kv...)
	}
}

// enabled reports voice.enabled now.
func (s *Supervisor) enabled() bool {
	return s.cfg.Enabled == nil || s.cfg.Enabled()
}

// EnabledChanged follows a settings change of voice.enabled: turned off, the
// voice stops at once (offLocked).
func (s *Supervisor) EnabledChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offLocked()
}

// offLocked reports whether the voice is turned off (R13). The first call
// that sees it off ends every exchange and sound, as a stop does, and drops
// the held answers, which stay in writing on their session's page; every
// path that could speak, sound or hand a turn asks it first.
func (s *Supervisor) offLocked() bool {
	if s.enabled() {
		s.off = false
		return false
	}
	if !s.off {
		s.off = true
		s.stopLocked("voice turned off")
		s.dropPendingLocked(func(string) bool { return true })
		s.lastTimes = nil
		s.recent = nil // the room's words from before the off go with it
	}
	return true
}

// SessionEnded drops the held answers of a session that ended, and the recent
// transcript: the Concierge keeps the answers for its page and never
// announces them in a later meeting.
func (s *Supervisor) SessionEnded(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPendingLocked(func(id string) bool { return id == sessionID })
	s.recent = nil // the next meeting gets none of this one's words
	if ex := s.ex; ex != nil && ex.answer != nil && ex.answer.SessionID == sessionID {
		ex.answer = nil // not put back: its meeting ended
		s.endLocked(ex, true, "its session ended")
	}
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

// dropPendingLocked drops the held answers and raised hands of the meetings
// that of names; a hand's agent is told it was withdrawn.
func (s *Supervisor) dropPendingLocked(of func(sessionID string) bool) {
	s.pending = slices.DeleteFunc(s.pending, func(p *pendingAnswer) bool {
		if !of(p.SessionID) {
			return false
		}
		s.handDoneLocked(p, HandWithdrawn)
		return true
	})
	s.showWaitingLocked()
}

// waitingLocked reports whether something waits for a person: a held answer
// or a raised hand waits for "Wispr, go ahead", or a held answer to speak by
// itself. Nothing waits once the Supervisor closed.
func (s *Supervisor) waitingLocked() bool {
	return !s.closed && len(s.pending) > 0
}

// showWaitingLocked tells the waiting light On or Off when waitingLocked
// changed, and cuts a notification sound none of whose answers or hands is
// left (endOrphanSoundsLocked). Every change of what waits calls it.
func (s *Supervisor) showWaitingLocked() {
	s.endOrphanSoundsLocked()
	waiting := s.waitingLocked()
	if waiting == s.lit {
		return
	}
	s.lit = waiting
	switch light := s.cfg.WaitingLight; {
	case light == nil:
	case waiting:
		light.On()
	default:
		light.Off()
	}
}

// endOrphanSoundsLocked cuts the chime input once the notification sound
// under way, being made or still playing from the speaker, has no owner
// left: its answers were dropped, its hands came down, or their memberships
// ended. Its owners alone own it, so it does not begin, or plays no further,
// after the last of them is gone. Its producer reads its hands' memberships
// at each frame itself (soundContext); this cuts what it already queued.
func (s *Supervisor) endOrphanSoundsLocked() {
	if s.chimeCancel != nil && !s.ownedLocked(s.chimeOwners) {
		s.cutChimeLocked(true)
	}
}

// ownedLocked reports whether any of owners still waits: a held answer, or a
// raised hand whose membership is live.
func (s *Supervisor) ownedLocked(owners []*pendingAnswer) bool {
	return slices.ContainsFunc(owners, func(p *pendingAnswer) bool {
		return slices.Contains(s.pending, p) && (p.hand == nil || p.hand.Context == nil || p.hand.Context.Err() == nil)
	})
}

// cutChimeLocked cuts the chime input: the notification sound under way,
// being made or still playing from the speaker, with whatever else plays
// there. With rearm, each of its owners still waiting sounds again at the
// next gap: a notification counts as given only once it played out.
func (s *Supervisor) cutChimeLocked(rearm bool) {
	if s.chimeCancel != nil {
		s.chimeCancel() // even one whose playback has not begun
		for _, p := range s.chimeOwners {
			if rearm && slices.Contains(s.pending, p) {
				p.chimed = false
			}
		}
		s.chimeCancel, s.chimeCtx, s.chimeOwners, s.chimeDraining = nil, nil, nil, false
	}
	s.cancelChimeLocked()
}

// cancelChimeLocked cancels the chime input, every playback begun on it, and
// every error sound scheduled for it, begun or not (errorSounds).
func (s *Supervisor) cancelChimeLocked() {
	for c := range s.errorSounds {
		c.cancel()
	}
	clear(s.errorSounds)
	s.cfg.Speaker.Cancel(audio.InputChime)
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

// WithFloor waits until no exchange holds the floor, then runs start, which
// begins other speech (POST /api/tts/speak) on the voice input, while no
// exchange can take the floor: an exchange that takes it afterwards cuts that
// speech, and none can take it in between. start runs under the Supervisor's
// lock, so it must only begin the playback.
func (s *Supervisor) WithFloor(ctx context.Context, start func()) error {
	for {
		s.mu.Lock()
		if s.ex == nil {
			start()
			s.mu.Unlock()
			return nil
		}
		free := s.floor
		s.mu.Unlock()
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
	if s.closed || !gateOpen || closings < s.closings || s.offLocked() {
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
	heard, segments := s.heard, s.segments
	s.heard, s.segments = nil, nil
	s.mu.Unlock()
	// The Concierge's Heard takes its own lock: never under the Supervisor's,
	// so a stop never waits on it. One reader calls OnTranscript, so the calls
	// keep their order.
	for i := range heard {
		s.cfg.Concierge.Heard(heard[i].ctx, &heard[i].x, heard[i].words)
	}
	for i := range segments {
		s.cfg.Segment(&segments[i])
	}
}

func (s *Supervisor) onTranscriptLocked(t *Transcript) {
	// A Finalize reply, a result that arrived while the gate was closed, or one
	// from before the latest closing is about audio from before a stop.
	if s.closed || t.FromFinalize || !t.GateOpen || t.Closings < s.closings || strings.TrimSpace(t.Text) == "" {
		return
	}
	// With the voice off, nothing reads the transcript for it (R13), and the
	// first result heard with it off stops what still runs.
	if s.offLocked() {
		return
	}
	now := time.Now()
	s.lastHeard = now
	own := s.ownLocked(t.Words, now)
	if !own {
		s.lastRoomWord = now
	}
	if t.IsFinal && !own {
		s.keepRecentLocked(t, now)
		s.segmentLocked(t)
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
				// The wake that this invitation's own interim opened, or the
				// follow-up window of the last call.
				s.endLocked(ex, true, "Wispr, go ahead")
			}
			s.goAheadLocked(transcript.IssueKeys(m.Rest))
		case cmd == transcript.HandsUp:
			if !t.IsFinal {
				return // once, on the final: it says the hands up
			}
			if ex != nil && ex.answer != nil && ex.answer.line {
				return // already said
			}
			if ex != nil {
				// The wake its own interim opened, or any other exchange: a
				// new call ends it.
				s.endLocked(ex, true, "Wispr, who has a hand up")
			}
			s.handsUpLocked(s.cfg.Session())
		case cmd == transcript.NeverMind:
			if !t.IsFinal {
				return // once, on the final: it cancels a question
			}
			if ex != nil && ex.answer == nil {
				s.endLocked(ex, true, "Wispr, never mind")
			}
			s.neverMindLocked(s.cfg.Session())
		case same && !ex.answering:
			s.turnLocked(ex, t, m.Rest)
		case same:
			// More of the utterance the exchange already answers.
		case ex != nil && !ex.answering && ex.answer == nil:
			// "Wispr." then "Wispr, what …", or a call in the follow-up
			// window: the same exchange. Over a driving voice's speech, it is
			// a barge-in with new words.
			ex.wakeStart = wakeStart(t, m)
			ex.turn, ex.interim, ex.turnFinal = nil, "", false
			ex.followUpTurn = false // a call, with the wake phrase
			if ex.drives {
				s.touchLocked(ex)
				s.disarmFollowUpLocked(ex)
				ex.newTurn = false
				if s.voiceSpeaking {
					s.logf("voice: barge-in with a call")
					s.cutDriverLocked(ex)
				}
			}
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
	if t.IsFinal && ex.answer == nil && s.wake.Thanks(t.Text) {
		s.endLocked(ex, true, "thanks")
		return
	}
	if ex.answer == nil && !ex.answering && !ex.drives && s.nameAt(t.Words, ex.wakeStart) {
		// The result still hears the name where the interim wake heard it, but
		// not as a call ("Whisper networks are a problem."): not the asker's
		// turn, so nothing of it is read for or handed over. On the final, the
		// wake was misheard, and nobody addressed Wispr. A final that dropped
		// the name keeps the call: Deepgram's live finals sometimes do.
		if t.IsFinal {
			s.endLocked(ex, true, "the wake was misheard")
		}
		return
	}
	if ex.drives && ex.answer == nil {
		// A person spoke: the follow-up window holds for the vendor's answer,
		// and after the voice spoke, the words are the asker's next turn,
		// a follow-up when the window was open (R10).
		if ex.newTurn {
			ex.newTurn = false
			ex.turn, ex.interim, ex.turnFinal = nil, "", false
			ex.followUpTurn = ex.followUpTimer != nil && !ex.askedBack
		}
		s.disarmFollowUpLocked(ex)
		s.turnLocked(ex, t, t.Text)
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
		s.cutDriverLocked(ex)
	}
}

// neverMindLocked answers "Wispr, never mind" (R7): the Concierge cancels the
// session's newest working question; with none, the session's newest held
// answer by arrival is dropped, and nothing of it plays. A raised hand is no
// answer: never mind leaves it up; only its agent, its expiry or its meeting
// takes it down. Never minds decide one after another, each once the
// Concierge answered it, so two in a row cancel and drop as if each waited
// for the last. The answer a never mind drops is the newest of those held
// when it was said that no "Wispr, go ahead" picked since: the later word
// wins. The Concierge cancels only a question asked before it was said.
// While a never mind waits, no answer sounds or speaks by itself (offer): an
// answer sound already chosen, being made or still in the speaker's queue,
// is cut when it is said, and sounds again for its answers still waiting
// once the never mind is decided. When its Concierge call fails, the answer
// it would have dropped waits for "Wispr, go ahead". One still queued when
// the Supervisor closes asks nothing.
func (s *Supervisor) neverMindLocked(sessionID string) {
	s.commands++
	// The questions asked by now, which orders this never mind among them
	// whatever the wall clock does.
	said, asked := s.commands, s.cfg.Concierge.QuestionsAsked()
	var candidates []*pendingAnswer
	for _, p := range s.pending {
		if p.SessionID == sessionID && p.hand == nil {
			candidates = append(candidates, p)
		}
	}
	s.neverMinds++
	if s.answerSoundLocked() {
		s.cutChimeLocked(true)
	}
	before, done := s.lastNeverMind, make(chan struct{})
	s.lastNeverMind = done
	s.spawn(func() {
		defer close(done)
		if before != nil {
			<-before
		}
		cancelled, err := false, s.ctx.Err()
		if err == nil {
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			cancelled, err = s.cfg.Concierge.NeverMind(ctx, sessionID, asked)
			cancel()
			if err != nil {
				s.logf("voice: cancel the newest question: %v", err)
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.neverMinds--
		var held *pendingAnswer
		for _, p := range candidates {
			if p.takenAt < said && slices.Contains(s.pending, p) && (held == nil || p.arrived > held.arrived) {
				held = p
			}
		}
		switch {
		case held == nil || cancelled:
		case err != nil:
			held.needsInvite = true
		default:
			s.pending = slices.DeleteFunc(s.pending, func(p *pendingAnswer) bool { return p == held })
			s.logf("voice: never mind dropped a held answer (task %s)", held.TaskID)
			s.showWaitingLocked() // cuts an answer sound left with no answer
		}
	})
}

// answerSoundLocked reports whether the sound under way, being made or still
// playing from the speaker, is an answer sound.
func (s *Supervisor) answerSoundLocked() bool {
	return s.chimeCancel != nil && !s.chimeHands
}

// cutDriverLocked stops the speech of ex's voice, which drives its own turn,
// at once, and keeps it open: its vendor hears the interruption and answers
// it.
func (s *Supervisor) cutDriverLocked(ex *exchange) {
	ex.cutSpeech = true
	s.cfg.Speaker.Cancel(audio.InputVoice)
	if sess := ex.session; sess != nil {
		s.spawn(sess.Interrupt)
	}
}

// startLocked opens an exchange on a wake: the wake chime, the voice, and a
// tap of room audio from the wake word on. A notification sound gives way to
// the wake first, and sounds again after it, whether or not the exchange then
// starts: a wake with the light not red starts none, and one whose voice
// cannot be built plays the error sound, which must not play over it.
func (s *Supervisor) startLocked(t *Transcript, m transcript.Match) {
	s.cutChimeLocked(true)
	if !s.cfg.Red() {
		return
	}
	voice, err := s.cfg.Voice()
	if err != nil || voice == nil {
		if err == nil {
			err = errors.New("no voice")
		}
		s.logf("voice: no voice for a call: %v", err)
		s.errorLocked(s.cfg.Session())
		return
	}
	ctx := newExchangeContext(s.ctx)
	ex := &exchange{ctx: ctx, cancel: ctx.cancel, authority: ctx, opened: make(chan struct{}), wakeStart: wakeStart(t, m), wakeAt: t.At, notified: map[string]bool{}, times: &exchangeTimes{wakeAt: t.At}}
	s.capExchange(ex)
	if d, ok := voice.(TurnDriver); ok && d.DrivesTurn() {
		ex.drives = true
	}
	ex.dropsAtOpen = s.cfg.Tap.Dropped()
	s.takeFloorLocked(ex)
	s.lastTimes = ex.times
	// Before Deepgram's first final there is no session yet: the exchange
	// takes the one the final opens (adoptSessionLocked).
	sessionID := s.cfg.Session()
	s.bindMeetingLocked(ex, sessionID)
	s.timing("wake_heard", sessionID, t.At)
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

// adoptSessionLocked gives ex the recording session now, if it has none
// yet (a wake heard before Deepgram's first final opened one), and binds its
// meeting.
func (s *Supervisor) adoptSessionLocked(ex *exchange) {
	if ex.x.SessionID != "" {
		return
	}
	ex.x.SessionID = s.cfg.Session()
	s.bindMeetingLocked(ex, ex.x.SessionID)
}

// bindMeetingLocked binds ex to the meeting of recording session sessionID
// (SupervisorConfig.Meeting): ex's context is done the moment the recorder
// ends that meeting, read at each check (exchangeContext), so Open, Say, a
// Write waiting for room and Drain stop then, with no lock and no session-
// ended hook. Once the meeting ended, a callback ends ex's state under s.mu;
// it authorises nothing. An exchange with no session yet binds none: the
// meeting of no session is ended from the start, and a wake heard before
// Deepgram's first final takes its meeting once the final opens it.
func (s *Supervisor) bindMeetingLocked(ex *exchange, sessionID string) {
	if s.cfg.Meeting == nil || sessionID == "" || ex.authority == nil {
		return
	}
	meeting := s.cfg.Meeting(sessionID)
	ex.authority.bind(meeting)
	stop := context.AfterFunc(meeting, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ex == ex && !ex.ended {
			s.endLocked(ex, true, "its meeting ended")
		}
	})
	context.AfterFunc(ex.ctx, func() { stop() })
}

// exchangeContext is an exchange's context: done when the exchange is (its
// exchangeMax, or an end), and, once bound to its recording session's meeting
// (bindMeetingLocked), the moment the recorder ends that meeting. Its Err
// reads the meeting at each check, so the recorder's end stops any check at
// that very moment, before a callback runs; Done closes once either is done.
type exchangeContext struct {
	context.Context // the exchange's own: its Done, Value and Deadline
	cancel          context.CancelFunc
	meeting         atomic.Pointer[meetingRef]
}

// meetingRef holds a bound meeting context.
type meetingRef struct{ ctx context.Context }

func newExchangeContext(parent context.Context) *exchangeContext {
	ctx, cancel := context.WithTimeout(parent, exchangeMax)
	return &exchangeContext{Context: ctx, cancel: cancel}
}

// bind ties the context to meeting: done once it is.
func (c *exchangeContext) bind(meeting context.Context) {
	c.meeting.Store(&meetingRef{ctx: meeting})
	stop := context.AfterFunc(meeting, c.cancel)
	context.AfterFunc(c.Context, func() { stop() })
}

func (c *exchangeContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if m := c.meeting.Load(); m != nil && m.ctx.Err() != nil {
		c.cancel()
		return context.Canceled
	}
	return nil
}

// open opens ex's voice and, for a wake, plays the wake chime alongside.
func (s *Supervisor) open(ex *exchange, voice Voice, x *Exchange, chime bool) {
	if chime {
		sessionID := x.SessionID
		s.spawn(func() { _, _ = s.playChime(ex.ctx, WakeChime(), "wake_chime_queued", sessionID) })
	}
	sess, err := voice.Open(ex.ctx, x)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(ex.opened)
	if err != nil {
		s.logf("voice: open the voice: %v", err)
		if ex.ctx.Err() == nil {
			s.errorLocked(x.SessionID)
		}
		if p := ex.answer; p != nil && !p.line && !ex.ended {
			// Not heard: it waits for "Wispr, go ahead" again.
			p.needsInvite = true
			s.pending = append([]*pendingAnswer{p}, s.pending...)
			s.showWaitingLocked()
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

// playChime plays pcm on the chime input, in 20 ms frames, and reports event
// at its first frame. Each Write reads ctx and queues nothing once it is done
// (audio.Playback.Write), so a sound whose owner is gone (soundContext) writes
// no further frame. It returns the playback, and the error of a Write that
// refused a frame.
func (s *Supervisor) playChime(ctx context.Context, pcm []byte, event, sessionID string) (Playback, error) {
	pb := s.cfg.Speaker.Begin(audio.InputChime)
	const frame = 960 // 20 ms at 24 kHz mono PCM16
	for off := 0; off < len(pcm); off += frame {
		if err := pb.Write(ctx, pcm[off:min(off+frame, len(pcm))], ChimeFormat); err != nil {
			return pb, err
		}
		if off == 0 {
			s.timing(event, sessionID, time.Now())
		}
	}
	return pb, nil
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
		case EventFactCheck:
			s.logf("voice: GPT-Live spoken fact errors %d", e.Count)
		case EventSpeechStarted:
			if ex.spokeAt.IsZero() {
				ex.spokeAt = time.Now()
			}
			s.touchLocked(ex)
		case EventTextAudio:
			if e.Text != "" && e.Text == ex.factText && !ex.factDone {
				ex.factDone = true
				s.timing("fact_audio", ex.x.SessionID, time.Now())
			}
		case EventSpeechEnded:
			if ex.drives && !ex.spokeAt.IsZero() && !ex.ended {
				s.noteLocked(ex, EventSpoken, ex.x.SessionID)
			}
			s.touchLocked(ex)
		case EventErr:
			s.logf("voice: %v", e.Err)
			current := s.ex == ex && !ex.ended
			if ex.drives && s.ex == ex {
				// The cut first: it cancels the chime input, and the error
				// sound begun before it would go with it.
				s.endLocked(ex, true, "the voice failed")
			}
			if current && ex.answer == nil {
				s.errorLocked(ex.x.SessionID)
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
	if s.voiceSpeaking || ex.toolCalls > 0 {
		s.touchLocked(ex)
		return
	}
	unspoken := ex.unspokenAnswer
	s.endLocked(ex, false, "idle")
	if unspoken {
		// The voice got an answer to speak and never began it: the room
		// hears that it failed (R10), not silence.
		s.logf("voice: the voice did not speak the answer it got")
		s.errorLocked(ex.x.SessionID)
	}
}

// driverSpeechLocked follows the speech of ex's voice, which drives its own
// turn: the room's next words are a new turn, and once its speech ends by
// itself, the follow-up window opens (R10).
func (s *Supervisor) driverSpeechLocked(ex *exchange, started bool) {
	s.disarmFollowUpLocked(ex)
	ex.newTurn = true
	if started {
		ex.unspokenAnswer = false
	}
	if started || ex.ended {
		return
	}
	if ex.cutSpeech { // a barge-in: the person speaks now
		ex.cutSpeech = false
		return
	}
	gen := ex.followUpGen
	ex.followUpTimer = s.cfg.followUpAfter(s.cfg.FollowUpWait, func() { s.driverFollowUpWaited(ex, gen) })
}

// disarmFollowUpLocked closes ex's follow-up window without ending ex.
func (s *Supervisor) disarmFollowUpLocked(ex *exchange) {
	ex.followUpGen++
	if ex.followUpTimer != nil {
		ex.followUpTimer.Stop()
		ex.followUpTimer = nil
	}
}

// driverFollowUpWaited ends ex when nobody spoke in its follow-up window.
func (s *Supervisor) driverFollowUpWaited(ex *exchange, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex != ex || ex.ended || ex.followUpGen != gen || s.voiceSpeaking || ex.toolCalls > 0 {
		return
	}
	s.endLocked(ex, false, "no follow-up")
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
		ex.turnFinal = true
		if ex.drives {
			ex.times.lastTurnEnd = time.Now()
		}
	} else {
		ex.interim = words
	}
	// The recording session starts with Deepgram's first final, often after
	// the wake: the exchange takes its id as soon as there is one.
	s.adoptSessionLocked(ex)
	turn := s.turnOf(ex)
	ex.x.AskerTurn = turn
	// A follow-up with no wake phrase may be side talk, which leaves the
	// unit for nothing: it is read for only once it is the next turn.
	if turn != "" && !ex.followUpTurn {
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

// turnOf is the asker's turn so far. A key of Dispatch's projects said in a
// spoken form ("in for forty five ninety two"), whose words may come in two
// results, is written as the key, which the Concierge reads (R4).
func (s *Supervisor) turnOf(ex *exchange) string {
	turn := strings.TrimSpace(strings.Join(append(ex.turn[:len(ex.turn):len(ex.turn)], ex.interim), " "))
	if s.cfg.ProjectKeys != nil {
		turn = transcript.WriteKeys(turn, s.cfg.ProjectKeys())
	}
	return turn
}

// isQuestion reports whether Deepgram punctuated turn as a question.
func isQuestion(turn string) bool {
	return strings.HasSuffix(strings.TrimSpace(turn), "?")
}

// followUpTurnWait bounds how long the answer tool waits for the final
// result of a follow-up turn, which tells a question from side talk: the
// vendor may call it before Deepgram's final comes.
const followUpTurnWait = 1500 * time.Millisecond

// followUpTurn reports whether the exchange's turn now is side talk in the
// follow-up window, a turn with no wake phrase that Deepgram did not punctuate
// as a question, and the turn as heard. It waits up to followUpTurnWait for the
// turn's final result.
func (t exchangeTools) followUpTurn(ctx context.Context) (side bool, turn string) {
	s := t.s
	deadline := time.Now().Add(followUpTurnWait)
	for {
		s.mu.Lock()
		follow, final, turn := t.ex.followUpTurn, t.ex.turnFinal, s.turnOf(t.ex)
		if turn == "" {
			turn = t.ex.x.AskerTurn
		}
		s.mu.Unlock()
		if !follow {
			return false, turn
		}
		if final || time.Now().After(deadline) || ctx.Err() != nil {
			return !isQuestion(turn), turn
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// turnWaited ends an exchange nobody asked anything in, or takes what the
// asker said as the turn when Deepgram's endpoint never came.
func (s *Supervisor) turnWaited(ex *exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex != ex || ex.answering || ex.drives || s.offLocked() {
		return
	}
	if s.turnOf(ex) == "" {
		reason := "no question after the wake phrase"
		if ex.followUp {
			reason = "no follow-up"
		}
		s.endLocked(ex, false, reason)
		return
	}
	s.handTurnLocked(ex)
}

// handTurnLocked hands the asker's turn to the Concierge.
func (s *Supervisor) handTurnLocked(ex *exchange) {
	if ex.answering || ex.drives || s.offLocked() {
		return
	}
	if ex.timer != nil {
		ex.timer.Stop()
	}
	if ex.followUpTurn && !isQuestion(s.turnOf(ex)) {
		s.endLocked(ex, false, "side talk in the follow-up window")
		return
	}
	ex.x.FollowUpTurn = ex.followUpTurn
	ex.followUpTurn = false
	ex.answering = true
	ex.turnAt = time.Now()
	ex.times.lastTurnEnd = ex.turnAt
	ex.firstOutDone, ex.factPending, ex.factDone, ex.factText = false, false, false, ""
	turn := s.turnOf(ex)
	ex.x.AskerTurn = turn
	s.adoptSessionLocked(ex)
	ex.x.Recent = s.recentLocked()
	s.timing("turn_end", ex.x.SessionID, ex.turnAt)
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
	_, reportsText := sess.(TextAudioReporter)
	say := func(ctx context.Context, text string, mode SayMode) error {
		s.mu.Lock()
		s.addOwnLocked(text, time.Now())
		if reportsText && mode == Fact && !ex.factDone && ex.factText == "" {
			// The voice reports when this text begins to play.
			ex.factText = text
		}
		s.mu.Unlock()
		called := time.Now()
		if err := sess.Say(ctx, text, mode); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		ex.said = true
		ex.askedBack = isQuestion(text)
		if !reportsText && mode == Fact && !ex.factDone && !ex.factPending {
			// The first fact's audio, for a voice whose Say returns once
			// the last of its audio is queued: at its speech's start when
			// it began during Say; when Say was called if earlier speech
			// still played, since the Output queues 200 ms at most; else
			// when it starts.
			switch {
			case !s.voiceStarted.Before(called):
				ex.factDone = true
				s.timing("fact_audio", x.SessionID, s.voiceStarted)
			case s.voiceSpeaking:
				ex.factDone = true
				s.timing("fact_audio", x.SessionID, called)
			default:
				ex.factPending = true
			}
		}
		return nil
	}
	err := s.cfg.Concierge.Answer(ex.ctx, x, turn, say)
	if err != nil && ex.ctx.Err() == nil {
		// A Concierge that cannot reach Dispatch or Envoy has said so; any
		// other failure is a vendor's. Both get the error sound (R10).
		s.logf("voice: the Concierge did not answer: %v", err)
		s.mu.Lock()
		said := ex.said
		s.errorLocked(x.SessionID)
		s.mu.Unlock()
		if !said {
			_ = say(ex.ctx, sorryLine, Ack)
		}
	}
	s.finish(ex, x.SessionID)
}

// finish waits until ex's speech has played, then ends ex. It reports
// whether ex spoke and played to its end, with no stop. A hand's words are
// waited for under their own context (exchange.sayCtx), and before anything
// is decided, the hand's membership is read under the lock: one that ended
// while its words played out is withdrawn, never reported spoken, put back or
// met with the error sound, whatever the speech did.
func (s *Supervisor) finish(ex *exchange, sessionID string) bool {
	s.mu.Lock()
	last, said, sess := ex.last, ex.said, ex.session
	authority := ex.ctx
	if ex.sayCtx != nil {
		authority = ex.sayCtx
	}
	s.mu.Unlock()
	played := true
	if said {
		if waiter, ok := sess.(SpeechWaiter); ok {
			played = waiter.WaitSpoken(authority) == nil
		} else if last != nil {
			played = last.Drain(authority) == nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ex != ex || ex.ended {
		return false
	}
	if p := ex.answer; p != nil && p.hand != nil && !p.hand.live() {
		s.handEndedLocked(ex, p)
		return false
	}
	spoken := said && played && ex.ctx.Err() == nil
	if spoken {
		s.noteLocked(ex, EventSpoken, sessionID)
	}
	if spoken && ex.answer == nil && !ex.drives {
		s.followUpLocked(ex)
		return spoken
	}
	if !spoken && said && ex.answer != nil && ex.ctx.Err() == nil {
		// The engine or Output cut a held answer after Say queued its first
		// frame. Apply R11's replay rule exactly as a stop does, then begin
		// the error sound on the new chime input the cut just opened.
		s.endLocked(ex, true, "the voice did not finish")
		s.errorLocked(sessionID)
		return false
	}
	s.endLocked(ex, false, "done")
	return spoken
}

// followUpLocked keeps ex open after Wispr's answer for FollowUpWait: an
// utterance in it is the next turn, with no wake phrase (R10).
func (s *Supervisor) followUpLocked(ex *exchange) {
	ex.followUp = true
	ex.followUpTurn = !ex.askedBack // an answer to Wispr's own question needs none
	ex.answering = false
	ex.turn, ex.interim = nil, ""
	ex.said, ex.last = false, nil
	delete(ex.notified, EventSpoken) // each spoken answer reports itself
	if ex.timer != nil {
		ex.timer.Stop()
	}
	ex.timer = time.AfterFunc(s.cfg.FollowUpWait, func() { s.turnWaited(ex) })
}

// goAheadLocked speaks a held answer on "Wispr, go ahead" while the light
// reads red: the one whose key the invitation names, else the oldest (R11).
func (s *Supervisor) goAheadLocked(keys []string) {
	s.dropEndedLocked()
	s.dropExpiredLocked(time.Now())
	s.dropWithdrawnLocked() // asked now, right before an invitation is accepted
	if !s.cfg.Red() || s.ex != nil || len(s.pending) == 0 {
		return
	}
	p := s.pending[0]
	for _, q := range s.pending {
		if q.IssueKey != "" && slices.Contains(keys, q.IssueKey) {
			p = q
			break
		}
	}
	p.needsInvite = false
	s.commands++
	p.takenAt = s.commands // the later word: a never mind said before no longer drops it
	s.speakLocked(p)
}

// speakLocked speaks a held answer, a raised hand or a line in an exchange of
// its own.
func (s *Supervisor) speakLocked(p *pendingAnswer) {
	voice, err := s.cfg.Voice()
	if err != nil || voice == nil {
		s.logf("voice: no voice for a held answer: %v", err)
		s.errorLocked(p.SessionID)
		p.needsInvite = true
		return
	}
	s.pending = slices.DeleteFunc(s.pending, func(q *pendingAnswer) bool { return q == p })
	s.showWaitingLocked()
	ctx := newExchangeContext(s.ctx)
	ex := &exchange{ctx: ctx, cancel: ctx.cancel, authority: ctx, opened: make(chan struct{}), answering: true, answer: p, wakeStart: -1, notified: map[string]bool{}, times: &exchangeTimes{}}
	s.capExchange(ex)
	ex.dropsAtOpen = s.cfg.Tap.Dropped()
	s.takeFloorLocked(ex)
	ex.x = Exchange{SessionID: p.SessionID, WakeAt: time.Now(), IssueKey: p.IssueKey, Recent: s.recentLocked(), Tap: s.tapFor(voice, 0), Out: exchangeOutput{s: s, ex: ex}}
	ex.x.Tools = exchangeTools{s: s, ex: ex}
	s.bindMeetingLocked(ex, p.SessionID)
	x := ex.x
	sayCtx, release := context.Context(ex.ctx), func() {}
	if p.hand != nil && p.hand.Context != nil {
		// A hand's words run under a context done once their exchange or the
		// hand's membership is, the membership read at each check
		// (joinContexts): its end stops them at once, wherever they have got
		// to, and ends their exchange.
		var stopJoin func()
		sayCtx, stopJoin = joinContexts(ex.ctx, p.hand.Context)
		stopHand := context.AfterFunc(p.hand.Context, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.handEndedLocked(ex, p)
		})
		release = func() { stopJoin(); stopHand() }
		ex.sayCtx = sayCtx
	}
	s.spawn(func() {
		defer release()
		s.open(ex, voice, &x, false)
		select {
		case <-ex.opened:
		case <-ex.ctx.Done():
			return
		}
		s.mu.Lock()
		sess := ex.session
		if p.hand != nil && !p.hand.live() {
			// Its membership ended after the invitation: withdrawn, not said.
			s.handEndedLocked(ex, p)
			s.mu.Unlock()
			return
		}
		s.addOwnLocked(p.Text, time.Now())
		s.mu.Unlock()
		if sess == nil {
			return
		}
		if err := sess.Say(sayCtx, p.Text, Answer); err != nil {
			s.logf("voice: speak a held answer: %v", err)
			s.mu.Lock()
			if p.hand != nil && p.hand.Context != nil && p.hand.Context.Err() != nil {
				// Its membership ended while the words were on their way.
				s.handEndedLocked(ex, p)
				s.mu.Unlock()
				return
			}
			if ex.ctx.Err() == nil {
				s.errorLocked(p.SessionID)
			}
			s.mu.Unlock()
			// Not heard: it waits for "Wispr, go ahead" again, unless a stop
			// already put it back.
			s.mu.Lock()
			if !ex.ended && !p.line && ex.ctx.Err() == nil {
				p.needsInvite = true
				s.pending = append([]*pendingAnswer{p}, s.pending...)
				s.showWaitingLocked()
			}
			s.mu.Unlock()
		} else {
			s.mu.Lock()
			ex.said = true
			s.mu.Unlock()
		}
		if s.finish(ex, p.SessionID) {
			s.heardOut(p)
		}
	})
}

// handEndedLocked ends ex, whose words are hand p's, as p's membership ended:
// p is withdrawn, never spoken and never put back, and the words are cut.
// It runs once for p, whichever of the membership's end and the failed Say
// comes first.
func (s *Supervisor) handEndedLocked(ex *exchange, p *pendingAnswer) {
	if ex.answer != p {
		return
	}
	ex.answer = nil
	s.pending = slices.DeleteFunc(s.pending, func(q *pendingAnswer) bool { return q == p })
	s.showWaitingLocked()
	s.handDoneLocked(p, HandWithdrawn)
	s.endLocked(ex, true, "its membership ended")
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
			// An answer that lands with the voice off is dropped, as offLocked
			// drops those held when it turns off: it stays in writing on its
			// session's page.
			if !s.closed && !s.offLocked() {
				// It waits for "Wispr, go ahead" unless the floor stayed free
				// since its question (R7): no stop and no room word since t0.
				// t0 is the end of the last turn of the exchange that asked,
				// so a reply to Wispr's own question ("The first one.") is part
				// of the exchange, not room speech. One that does not say when
				// it was asked waits too.
				t0 := a.AskedAt
				if lt := s.lastTimes; lt != nil && !lt.wakeAt.After(a.AskedAt) && lt.lastTurnEnd.After(t0) {
					t0 = lt.lastTurnEnd
				}
				stoppedSince := !s.lastStopAt.IsZero() && !s.lastStopAt.Before(a.AskedAt)
				talkedSince := !s.lastRoomWord.IsZero() && s.lastRoomWord.After(t0)
				s.arrivals++
				s.pending = append(s.pending, &pendingAnswer{PendingAnswer: a, arrived: s.arrivals, needsInvite: a.AskedAt.IsZero() || stoppedSince || talkedSince})
				s.showWaitingLocked()
				s.timing("answer_ready", a.SessionID, time.Now())
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

// offer chimes each held answer and raised hand once while the light reads
// red, and speaks by itself the oldest answer that needs no invitation, once
// the room is silent. It takes down the hands that expired or were withdrawn.
// While a "Wispr, never mind" waits for the Concierge, answers wait: the one
// it may drop must not sound or speak first. Hands do not wait for it.
func (s *Supervisor) offer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropEndedLocked()
	if s.closed || s.offLocked() {
		return
	}
	now := time.Now()
	s.dropExpiredLocked(now)
	s.dropWithdrawnLocked()
	if len(s.pending) == 0 || !s.cfg.Red() {
		return
	}
	holdAnswers := s.neverMinds > 0
	if ex := s.ex; ex != nil && !s.idleLocked(ex) {
		return
	}
	// The answer sound first, then the hand sound: each sounds for its own.
	hands := holdAnswers || !slices.ContainsFunc(s.pending, func(p *pendingAnswer) bool { return !p.chimed && p.hand == nil })
	unchimed := slices.ContainsFunc(s.pending, func(p *pendingAnswer) bool { return !p.chimed && (p.hand != nil) == hands })
	if unchimed {
		// The sound waits for a gap with no transcript word (R10). One sound
		// for every answer, or every hand, that has not had one: those that
		// land together, or while their sound is being made, share it, so
		// sounds never overlap on the one input. The other sound plays first,
		// and a sound whose every frame is written plays out before the next
		// begins: a hand raised then is not counted as sounded by it.
		if s.voiceSpeaking || now.Sub(s.lastHeard) < s.cfg.SoundGap {
			return
		}
		if s.chimeCancel != nil && (s.chimeHands != hands || s.chimeDraining) {
			return
		}
		var owners []*pendingAnswer
		for _, p := range s.pending {
			if (p.hand != nil) == hands && !p.chimed {
				p.chimed = true
				owners = append(owners, p)
			}
		}
		s.chimedAt = now
		if s.chimeCancel != nil { // the sound under way sounds for these too
			s.chimeOwners = append(s.chimeOwners, owners...)
			s.chimeCtx.own(owners)
			return
		}
		pcm, event := AnswerChime(), "answer_chime"
		if hands {
			pcm, event = HandChime(), "hand_chime"
		}
		sessionID := owners[len(owners)-1].SessionID
		var meeting context.Context
		if s.cfg.Meeting != nil && sessionID != "" {
			meeting = s.cfg.Meeting(sessionID)
		}
		ctx := newSoundContext(s.ctx, meeting, owners)
		s.chimeCancel, s.chimeCtx, s.chimeHands, s.chimeOwners = ctx.cancel, ctx, hands, owners
		s.spawn(func() {
			pb, err := s.playChime(ctx, pcm, event, sessionID)
			if err == nil {
				s.mu.Lock()
				if ctx == s.chimeCtx {
					s.chimeDraining = true
				}
				s.mu.Unlock()
				// Its owners keep it until the speaker has played its last
				// frame, however late: only then has it sounded, and until then
				// their going cuts it (endOrphanSoundsLocked).
				err = pb.Drain(ctx)
			}
			ctx.cancel()
			s.mu.Lock()
			defer s.mu.Unlock()
			if ctx != s.chimeCtx {
				return // cut meanwhile
			}
			s.chimeCancel, s.chimeCtx, s.chimeOwners, s.chimeDraining = nil, nil, nil, false
			if err != nil {
				// Its own authority ended it (its meeting, or the membership of
				// its last hand): what is still queued of it plays no further.
				s.cancelChimeLocked()
			}
		})
		return
	}
	if holdAnswers {
		return // an answer speaks by itself only once no never mind waits
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
			if ex := s.ex; ex != nil {
				s.endLocked(ex, false, "a holder's answer takes the floor")
			}
			s.speakLocked(p)
			return
		}
	}
}

// idleLocked reports whether ex holds the floor only by its follow-up window,
// with no turn begun, no tool call and no speech: a held answer may then
// sound and speak without waiting for the window to run out.
func (s *Supervisor) idleLocked(ex *exchange) bool {
	if ex.ended || ex.answer != nil || s.voiceSpeaking {
		return false
	}
	if ex.drives {
		return ex.followUpTimer != nil && ex.toolCalls == 0
	}
	return ex.followUp && !ex.answering && s.turnOf(ex) == ""
}

// stopLocked ends the exchange holding the floor for reason, and makes every
// held answer wait for an invitation.
func (s *Supervisor) stopLocked(reason string) {
	now := time.Now()
	s.lastStopAt = now
	sessionID := s.cfg.Session()
	if s.ex != nil && s.ex.x.SessionID != "" {
		sessionID = s.ex.x.SessionID
	}
	s.timing("stop", sessionID, now, "reason", reason)
	for _, p := range s.pending {
		p.needsInvite = true
	}
	// The notification sounds stop with the rest, even one whose playback
	// has not begun; a stop is heard as dismissing them, so they do not sound
	// again for it.
	s.cutChimeLocked(false)
	if ex := s.ex; ex != nil {
		s.endLocked(ex, true, reason)
	}
}

// endLocked ends ex. cut says a stop or a barge-in ended it: its voice and
// chime are cut at once. A held answer cut within ReplayWindow stays pending.
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
	if p := ex.answer; cut && p != nil && !p.line {
		switch {
		case ex.spokeAt.IsZero() || time.Since(ex.spokeAt) < s.cfg.ReplayWindow:
			p.needsInvite = true
			s.pending = append([]*pendingAnswer{p}, s.pending...)
			s.showWaitingLocked()
		case p.hand != nil:
			s.handDoneLocked(p, HandSpoken)
		default:
			s.spawn(func() { s.spoken(p.TaskID) })
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
		s.cutChimeLocked(true)
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

// takeFloorLocked makes ex the exchange holding the floor, and cuts other
// speech on the voice input, such as POST /api/tts/speak, whether or not it
// has sounded yet, and the answer sound. Speech begun after this waits for
// the floor (WithFloor).
func (s *Supervisor) takeFloorLocked(ex *exchange) {
	s.ex = ex
	s.floor = make(chan struct{})
	s.cfg.Speaker.Cancel(audio.InputVoice)
	s.cutChimeLocked(true) // a notification sound gives way to the exchange, and sounds again after it
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
	ex := s.ex
	sessionID := ""
	if ex != nil {
		sessionID = ex.x.SessionID
	}
	if e.Started {
		s.voiceStarted = e.At
	} else {
		s.voiceEnded = time.Now()
		s.timing("speech_end", sessionID, e.At)
	}
	if ex == nil {
		return
	}
	s.touchLocked(ex)
	if ex.drives {
		s.driverSpeechLocked(ex, e.Started)
	}
	if !e.Started {
		return
	}
	if !ex.turnAt.IsZero() && !ex.firstOutDone {
		ex.firstOutDone = true
		s.timing("first_audio", sessionID, e.At)
		s.logf("voice: first spoken audio %v after the turn ended", e.At.Sub(ex.turnAt).Round(time.Millisecond))
	}
	if ex.factPending {
		ex.factPending, ex.factDone = false, true
		s.timing("fact_audio", sessionID, e.At)
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

// recentLocked is the transcript of the last recentKept, pruned by age as it
// is read: nothing may have been heard since to prune it.
func (s *Supervisor) recentLocked() []TurnText {
	var kept []TurnText
	now := time.Now()
	for _, r := range s.recent {
		if now.Sub(r.At) < recentKept {
			kept = append(kept, r)
		}
	}
	return kept
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
// Supervisor can wait for its speech to play. A playback begun once ex has
// ended refuses every write: the speaker's Cancel ends only the playbacks
// begun before it, and a voice that synthesised ahead (voice A) may begin
// one for its next sentence before its own Interrupt runs. endLocked marks ex
// ended under s.mu before it cancels, and Begin reads that under s.mu, so no
// playback begun after the cut can play. Every playback writes and drains
// under ex's context (with the recorder's meeting, exchangeContext), or a
// hand's words under theirs (with its membership, exchange.sayCtx), read at
// each check, so a frame waiting for room commits nothing once either ended.
type exchangeOutput struct {
	s  *Supervisor
	ex *exchange
}

func (o exchangeOutput) Begin() Playback {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	authority := o.ex.ctx
	if o.ex.sayCtx != nil {
		authority = o.ex.sayCtx
	}
	if o.ex.ended || authority.Err() != nil {
		return endedPlayback{}
	}
	pb := sayPlayback{Playback: o.s.cfg.Speaker.Begin(audio.InputVoice), ctx: authority, joins: &frameJoin{}}
	o.ex.last = pb
	return pb
}

// errExchangeEnded refuses the writes of a playback begun after its exchange
// ended.
var errExchangeEnded = errors.New("voice: the exchange has ended")

// endedPlayback is a playback begun after its exchange ended: it plays
// nothing.
type endedPlayback struct{}

func (endedPlayback) Write(context.Context, []byte, audio.AudioFormat) error {
	return errExchangeEnded
}

func (endedPlayback) Drain(context.Context) error { return nil }

// sayPlayback is a playback of an exchange's speech: each Write and Drain
// runs under both the voice's context and the exchange's authority (its
// context, with the recorder's meeting, or a hand's words' context, with its
// membership), so the authority's end, read at each check, stops a write even
// while it waits for room in the speaker's queue, and commits nothing. With
// joins set, as Begin sets it, the frames written under one context share
// one join, so a frame allocates nothing: an allocating goroutine on the
// audio path is drafted into the garbage collector's assist, and the speaker
// underruns.
type sayPlayback struct {
	Playback
	ctx   context.Context
	joins *frameJoin
}

// frameJoin keeps the last context a playback was written under, joined with
// its authority. Contexts are compared with ==, as every context the voices
// pass is a pointer or an empty struct.
type frameJoin struct {
	mu     sync.Mutex
	caller context.Context
	joined context.Context
	stop   func()
}

// keepJoin is the release of a join frameJoin keeps: it is released when the
// next context replaces it, or ends with its contexts.
var keepJoin = func() {}

// under is ctx joined with the playback's authority, and its release.
func (p sayPlayback) under(ctx context.Context) (context.Context, func()) {
	j := p.joins
	if j == nil {
		return joinContexts(ctx, p.ctx)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.joined == nil || j.caller != ctx {
		if j.stop != nil {
			j.stop()
		}
		j.joined, j.stop = joinContexts(ctx, p.ctx)
		j.caller = ctx
	}
	return j.joined, keepJoin
}

func (p sayPlayback) Write(ctx context.Context, frame []byte, format audio.AudioFormat) error {
	both, release := p.under(ctx)
	defer release()
	if err := both.Err(); err != nil {
		return err
	}
	return p.Playback.Write(both, frame, format)
}

func (p sayPlayback) Drain(ctx context.Context) error {
	both, release := p.under(ctx)
	defer release()
	if err := both.Err(); err != nil {
		return err
	}
	return p.Playback.Drain(both)
}

// joinedContext is done when either of two contexts is. Its Err reads both
// at once, so a context whose own Err reads live state (a joined agent's
// membership, which ends the moment the recorder ends its meeting) stops a
// check at that very moment; Done closes once either is done, or once Err
// has seen it.
type joinedContext struct {
	context.Context // the first: its values, and its deadline with the other's
	other           context.Context
	done            chan struct{}
	once            sync.Once
}

// joinContexts joins a and b; stop releases the join once it is no longer
// used.
func joinContexts(a, b context.Context) (joined context.Context, stop func()) {
	c := &joinedContext{Context: a, other: b, done: make(chan struct{})}
	stopA := context.AfterFunc(a, c.close)
	stopB := context.AfterFunc(b, c.close)
	return c, func() { stopA(); stopB() }
}

func (c *joinedContext) close() { c.once.Do(func() { close(c.done) }) }

func (c *joinedContext) Done() <-chan struct{} { return c.done }

func (c *joinedContext) Err() error {
	err := c.other.Err()
	if err == nil {
		err = c.Context.Err()
	}
	if err != nil {
		c.close()
	}
	return err
}

func (c *joinedContext) Deadline() (time.Time, bool) {
	deadline, ok := c.Context.Deadline()
	if other, otherOK := c.other.Deadline(); otherOK && (!ok || other.Before(deadline)) {
		return other, true
	}
	return deadline, ok
}

// soundContext is a notification sound's context: done when the Supervisor
// ends the sound, when the recorder ends the meeting it sounds in, or, for
// the hand sound, once the membership of every hand it sounds for has ended.
// Its Err reads them at each check, with no lock, so the sound writes no
// further frame the moment its last owner is gone, before any cleanup runs;
// Done closes once any of them is done.
type soundContext struct {
	context.Context // the sound's own: its Done and Value
	cancel          context.CancelFunc
	meeting         context.Context // nil: none
	mu              sync.Mutex
	members         []context.Context // its hands' membership contexts
	unowned         bool              // it sounds for an answer, or a hand with no context: owners never end it
}

func newSoundContext(parent, meeting context.Context, owners []*pendingAnswer) *soundContext {
	ctx, cancel := context.WithCancel(parent)
	c := &soundContext{Context: ctx, cancel: cancel, meeting: meeting}
	if meeting != nil {
		stop := context.AfterFunc(meeting, cancel)
		context.AfterFunc(ctx, func() { stop() })
	}
	c.own(owners)
	return c
}

// own adds owners the sound now sounds for too.
func (c *soundContext) own(owners []*pendingAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range owners {
		if p.hand == nil || p.hand.Context == nil {
			c.unowned = true
			continue
		}
		c.members = append(c.members, p.hand.Context)
		stop := context.AfterFunc(p.hand.Context, func() { _ = c.Err() })
		context.AfterFunc(c.Context, func() { stop() })
	}
}

func (c *soundContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if c.meeting != nil && c.meeting.Err() != nil {
		c.cancel()
		return context.Canceled
	}
	c.mu.Lock()
	ended := !c.unowned && len(c.members) > 0 && !slices.ContainsFunc(c.members, func(m context.Context) bool { return m.Err() == nil })
	c.mu.Unlock()
	if ended {
		c.cancel()
		return context.Canceled
	}
	return nil
}

// exchangeTools runs the Concierge's tools for ex, and the answer tool
// through Concierge.Answer.
type exchangeTools struct {
	s  *Supervisor
	ex *exchange
}

// Run runs c for the exchange's voice, and keeps the exchange open while it
// runs. For the answer tool, the Concierge's Ack lines said before any other
// line (its routing line) are spoken at once through the session; the rest
// is the result, in order, for the voice to speak word for word. When the
// answer fails, the Supervisor speaks the Concierge's own lines for it, or one
// sorry, and plays the error sound: the voice says nothing of its own.
func (t exchangeTools) Run(ctx context.Context, c ToolCall) (json.RawMessage, error) {
	s := t.s
	s.mu.Lock()
	s.adoptSessionLocked(t.ex)
	x := t.ex.x
	t.ex.toolCalls++
	s.disarmFollowUpLocked(t.ex)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		t.ex.toolCalls--
		// The voice speaks the result next: a follow-up window that a line
		// said during the call opened closes, or it could end the exchange
		// before that speech starts. The idle wait covers a voice that then
		// says nothing.
		s.disarmFollowUpLocked(t.ex)
		s.touchLocked(t.ex)
	}()
	if c.Name != AnswerTool {
		out, err := s.cfg.Concierge.Tools(&x).Run(ctx, c)
		if errors.Is(err, ErrUnreachable) {
			// The result says so, for the vendor to speak; the room hears
			// the error sound (R10).
			s.logf("voice: a tool could not reach Dispatch or Envoy: %v", err)
			s.mu.Lock()
			if t.ex.ctx.Err() == nil {
				s.errorLocked(x.SessionID)
			}
			s.mu.Unlock()
			return out, nil
		}
		return out, err
	}
	var args AnswerArgs
	if err := json.Unmarshal(c.Args, &args); err != nil {
		return nil, errors.New("answer tool: arguments are not an AnswerArgs object")
	}
	side, turn := t.followUpTurn(ctx)
	if side {
		// Side talk in the follow-up window: no voice answers it, and the
		// window closes (R10). The vendor gets no words to say.
		s.mu.Lock()
		s.endLocked(t.ex, false, "side talk in the follow-up window")
		s.mu.Unlock()
		return json.Marshal(AnswerResult{})
	}
	if strings.TrimSpace(args.Question) == "" {
		// A vendor may delegate before its own transcript of the turn.
		args.Question = turn
	}
	// The turn as heard by now: a follow-up's final may have come while
	// followUpTurn waited for it.
	s.mu.Lock()
	x = t.ex.x
	x.FollowUpTurn = t.ex.followUpTurn
	s.mu.Unlock()
	var (
		said     []string  // the result's lines, in order
		spoken   bool      // a line was spoken at once
		lead     *leadLine // that line, while its Say may not have played
		factSaid bool      // a line of the result holds a fact
	)
	err := s.cfg.Concierge.Answer(ctx, &x, args.Question, func(sayCtx context.Context, text string, mode SayMode) error {
		t.ex.saidMu.Lock()
		first := (mode == Ack || mode == Lead) && len(said) == 0 && lead == nil
		t.ex.saidMu.Unlock()
		if first {
			// Spoken at once, and not waited for: a vendor such as
			// Deepgram's Voice Agent plays a line said during a call only
			// after the call's own reply, which this answer is. If it has
			// not played when the answer is ready, it leads the answer.
			l := t.speakLead(text, mode)
			t.ex.saidMu.Lock()
			spoken, lead = true, l
			t.ex.saidMu.Unlock()
			return nil
		}
		t.ex.saidMu.Lock()
		defer t.ex.saidMu.Unlock()
		said = append(said, text)
		factSaid = factSaid || mode == Fact
		return nil
	})
	t.ex.saidMu.Lock()
	lines, anySaid, l, fact := append([]string(nil), said...), spoken || len(said) > 0, lead, factSaid
	t.ex.saidMu.Unlock()
	if l != nil && !t.played(l) {
		// Main's rule on gap H: R5's line naming the issue a search chose
		// (Lead) always leads; a routing line that never played is dropped
		// when the answer is a fact ready within leadLineDropWithin of t0,
		// the end of the asker's turn, and else leads the answer.
		s.mu.Lock()
		t0 := t.ex.times.lastTurnEnd
		s.mu.Unlock()
		if t0.IsZero() {
			t0 = l.called
		}
		if !l.named && err == nil && fact && time.Since(t0) <= leadLineDropWithin {
			s.logf("voice: the routing line had not played, and the fact came in time: dropped")
		} else {
			s.logf("voice: the %s line had not played, so it leads the answer", l.mode)
			lines = append([]string{l.text}, lines...)
		}
	}
	if err != nil {
		if ctx.Err() == nil && t.ex.ctx.Err() == nil {
			s.logf("voice: the Concierge did not answer: %v", err)
			s.mu.Lock()
			s.errorLocked(x.SessionID)
			s.mu.Unlock()
			if !anySaid {
				lines = []string{sorryLine}
			}
			for _, line := range lines {
				if sayErr := t.speak(ctx, line, Ack); sayErr != nil {
					s.logf("voice: speak the Concierge's line for a failed answer: %v", sayErr)
					break
				}
			}
		}
		return nil, err
	}
	if len(lines) > 0 {
		s.mu.Lock()
		t.ex.askedBack = isQuestion(lines[len(lines)-1])
		t.ex.unspokenAnswer = true // until the voice's speech starts
		s.mu.Unlock()
	}
	return json.Marshal(AnswerResult{Say: strings.Join(lines, " "), IssueKey: x.IssueKey})
}

// leadLine is the first line an answer tool speaks at once, in the
// background: the routing line, or R5's line naming the issue a search chose
// (named, mode Lead), which is never dropped.
type leadLine struct {
	text   string
	mode   SayMode
	named  bool
	called time.Time // when its Say was called
	cancel context.CancelFunc
	done   chan struct{}
	ok     bool // its Say returned with no error; read once done is closed
}

// leadLineWait bounds how long after its call a lead line may take to
// start, and how long played waits for a cancelled Say to return.
const leadLineWait = 500 * time.Millisecond

// leadLineDropWithin: a routing line that never played is dropped when the
// answer is a fact ready this soon after t0, and else leads it (Main's rule
// on gap H); a test shortens it.
var leadLineDropWithin = 3 * time.Second

// speakLead speaks text at once in the background, through the exchange's
// session.
func (t exchangeTools) speakLead(text string, mode SayMode) *leadLine {
	ctx, cancel := context.WithCancel(t.ex.ctx)
	l := &leadLine{text: text, mode: mode, named: mode == Lead, called: time.Now(), cancel: cancel, done: make(chan struct{})}
	t.s.spawn(func() {
		defer close(l.done)
		err := t.speak(ctx, text, mode)
		l.ok = err == nil
		if err != nil && ctx.Err() == nil {
			t.s.logf("voice: the %s line was not spoken: %v", mode, err)
		}
	})
	return l
}

// played reports whether the line was heard: its Say returned with no
// error, or the voice began to speak after it was called, within
// leadLineWait. A Say still waiting with no speech then, as Deepgram's Voice
// Agent holds a line behind the model's muted reply, is cancelled: the line
// goes with the answer instead.
func (t exchangeTools) played(l *leadLine) bool {
	deadline := l.called.Add(leadLineWait) // a line held for long is cancelled at once
	for {
		select {
		case <-l.done:
			return l.ok
		default:
		}
		t.s.mu.Lock()
		began := !t.s.voiceStarted.Before(l.called)
		t.s.mu.Unlock()
		if began {
			return true
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.cancel()
	select {
	case <-l.done:
		return l.ok
	case <-time.After(leadLineWait):
		return false
	}
}

// speak says text through the exchange's session at once, in mode.
func (t exchangeTools) speak(ctx context.Context, text string, mode SayMode) error {
	s := t.s
	s.mu.Lock()
	sess := t.ex.session
	s.addOwnLocked(text, time.Now())
	s.mu.Unlock()
	if sess == nil {
		return errors.New("the voice is not open")
	}
	if err := sess.Say(ctx, text, mode); err != nil {
		return err
	}
	s.mu.Lock()
	t.ex.askedBack = isQuestion(text)
	s.mu.Unlock()
	return nil
}
