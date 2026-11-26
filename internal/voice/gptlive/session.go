// Package gptlive is voice B: OpenAI's GPT-Live-1 speaks for Wispr, and the
// Concierge does the work through GPT-Live's client delegation.
//
// A session lives for one exchange. It opens the primary Live WebSocket with
// an access token from the unit's own OpenAI provider (Config.Token), starts
// a session seeded with the meeting's transcript, streams the capture tap
// in, and plays the model's speech on the speaker output's voice input. When
// GPT-Live delegates, the session runs the Concierge's answer tool and
// returns its result as silent context and as commentary for GPT-Live to say.
// Audio plays only while the session is open and not interrupted: GPT-Live
// has no command that cancels its own speech, so this playback gate is the
// hard stop.
package gptlive

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/openaiwif"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

const (
	// DefaultURL is GPT-Live's primary WebSocket. It takes no query
	// parameters: the model goes in session.start.
	DefaultURL = "wss://api.openai.com/v1/live/sessions"
	// DefaultModel is the Live model.
	DefaultModel = "gpt-live-1"

	// SampleRate is the session's audio rate both ways: audio/pcm, mono
	// PCM16 little-endian. The tap's 16 kHz is read at this rate, and the
	// model's speech plays at it.
	SampleRate = 24000
)

const (
	// writeFrame is the most audio one Playback.Write carries: 20 ms.
	writeFrame = SampleRate / 50
	// speechGap is how long the model's audio pauses before its speech
	// ends. GPT-Live sends no event at the end of its speech.
	speechGap = 400 * time.Millisecond
	// earlyAudio is the most tap audio the session keeps while it starts, to
	// send once the session has started: the first of it, which holds the
	// asker's first words.
	earlyAudio = 10 * SampleRate
	// maxQueuedAudio is the most model speech the session holds to play;
	// past it the oldest goes.
	maxQueuedAudio = 120 * SampleRate
	// startTimeout bounds the wait for session.started.
	startTimeout = 10 * time.Second
	// handshakeTimeout bounds the WebSocket handshake.
	handshakeTimeout = 15 * time.Second
	// writeTimeout bounds one write to the socket; a write that takes longer
	// ends the connection.
	writeTimeout = 5 * time.Second
	// closeGrace is how long Close gives the server to finalize the session
	// after session.close, inside Close's 100 ms.
	closeGrace = 60 * time.Millisecond
	// roomPoll is how often a delegation waiting for the room re-reads it
	// when the room gives no Changed to wait on (a Room outside the
	// Supervisor's).
	roomPoll = 40 * time.Millisecond
	// nothingFound answers a delegation the Concierge had nothing to say on.
	nothingFound = "There is nothing to report on this. Say so in one short sentence."
	// lookupFailed closes a delegation whose answer tool failed, as silent
	// context: the Supervisor has already spoken the failure, so the voice
	// says nothing of its own.
	lookupFailed = "The lookup failed, and the room has already been told. Do not mention it again unless someone asks."
)

// stopInstruction is what Interrupt tells GPT-Live when someone talks over it.
const stopInstruction = "Stop speaking now: someone in the room talked over you. Do not finish what you were saying. Listen, and answer only if someone asks you something."

var (
	// ErrUnknownCall is a Deliver for a delegation this session never had.
	ErrUnknownCall = errors.New("gptlive: no such delegation in this session")
	// ErrDisconnected is the Live connection ending before the session did.
	ErrDisconnected = errors.New("gptlive: the Live connection ended")
	// ErrClosed is a call on a session that has been closed.
	ErrClosed = errors.New("gptlive: session closed")
	// ErrNoSpeech is a Say that GPT-Live did not speak.
	ErrNoSpeech = errors.New("gptlive: GPT-Live did not speak the text")
)

// sayStartTimeout is how long Say waits for GPT-Live to start speaking the
// text it was given.
var sayStartTimeout = 15 * time.Second

// sayQuiet is how long Say waits, after GPT-Live's speech has played, for
// more of it before it takes the text as spoken. GPT-Live marks no end of
// an answer, and an exchange that ends when Say returns would cut a second
// sentence. How long GPT-Live pauses between sentences is unmeasured.
const sayQuiet = 1200 * time.Millisecond

// APIError is an error event from the Live API. Its text is the structured
// code or type only, when it is a plain token (providererr.Token): message
// and param are free-form server text, and a provider can echo what the
// session sent into any of them. Errors reach the journal, which no delete
// path reaches.
type APIError struct {
	Type, Code, Message, Param string
}

func (e *APIError) Error() string {
	return "gpt-live error " + e.code()
}

// code is the error's code, else its type, when either is a plain token, and
// "server_error" otherwise.
func (e *APIError) code() string {
	return cmp.Or(providererr.Token(e.Code), providererr.Token(e.Type), "server_error")
}

// Config configures voice B.
type Config struct {
	// URL is the Live WebSocket; empty means DefaultURL.
	URL string
	// Model is the Live model; empty means DefaultModel.
	Model string
	// Voice is GPT-Live's voice, such as "marin"; empty means the API's
	// default.
	Voice string
	// Token gives the bearer for each session: the OpenAI provider's
	// credential, such as openaiwif.TokenSource.Token.
	Token func(ctx context.Context) (string, error)
	// Logger logs without words of the room; nil means slog.Default().
	Logger *slog.Logger
}

// Voice opens GPT-Live sessions.
type Voice struct {
	cfg    Config
	dialer websocket.Dialer
}

var (
	_ voice.Voice      = (*Voice)(nil)
	_ voice.TurnDriver = (*Voice)(nil)
)

// New makes voice B.
func New(cfg Config) (*Voice, error) {
	if cfg.Token == nil {
		return nil, errors.New("gptlive: no access token source")
	}
	if cfg.URL == "" {
		cfg.URL = DefaultURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	cfg.Logger = logging.WithModule(cfg.Logger, "gptlive")
	return &Voice{cfg: cfg, dialer: websocket.Dialer{HandshakeTimeout: handshakeTimeout, Proxy: http.ProxyFromEnvironment}}, nil
}

// DrivesTurn reports that GPT-Live drives the turn: it decides when to
// answer and delegates to the Concierge's answer tool.
func (v *Voice) DrivesTurn() bool { return true }

// Open connects to GPT-Live and starts a session for x. It returns once the
// session has started, so x's tap audio, which it keeps from the start,
// reaches the model. ctx is the exchange's: when it ends, after Open has
// returned too, the session closes, and with it the room audio to OpenAI.
// A caller must not pass a context that ends with the call alone.
func (v *Voice) Open(ctx context.Context, x *voice.Exchange) (voice.Session, error) {
	if x.Out == nil {
		return nil, errors.New("gptlive: the exchange has no output")
	}
	if x.Tools == nil {
		return nil, errors.New("gptlive: the exchange has no tools")
	}
	token, err := v.cfg.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("gptlive: access token: %w", err)
	}
	conn, resp, err := v.dialer.DialContext(ctx, v.cfg.URL, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("gptlive: connect: HTTP %d%s: %w", resp.StatusCode, refusal(resp), err)
		}
		return nil, fmt.Errorf("gptlive: connect: %w", providererr.From("openai", err))
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	s := newSession(ctx, v.cfg, x, conn)
	start := sessionStart{
		Type:    evSessionStart,
		EventID: "gw_start",
		Session: sessionConfig{
			Model:        v.cfg.Model,
			Instructions: instructions,
			Store:        false,
			Delegation:   delegation{Type: delegationTargetClient},
			Audio:        sessionAudio{Format: audioFormat{Type: "audio/pcm", Rate: SampleRate}},
			Input:        history(x),
		},
	}
	if v.cfg.Voice != "" {
		start.Session.Audio.Output = &audioOutput{Voice: v.cfg.Voice}
	}
	if err := s.send(start); err != nil {
		s.teardown()
		return nil, fmt.Errorf("gptlive: start: %w", err)
	}
	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case <-s.started:
		// The exchange's context bounds the session: its time limit and
		// every ending close it, and with it the room audio to OpenAI.
		release := context.AfterFunc(ctx, func() { _ = s.Close() })
		s.mu.Lock()
		s.release = release
		s.mu.Unlock()
		return s, nil
	case err := <-s.startErr:
		s.teardown()
		return nil, fmt.Errorf("gptlive: start: %w", err)
	case <-ctx.Done():
		s.teardown()
		return nil, ctx.Err()
	case <-timer.C:
		s.teardown()
		return nil, fmt.Errorf("gptlive: no session.started within %v", startTimeout)
	}
}

// session is one GPT-Live session for one exchange.
type session struct {
	cfg  Config
	x    voice.Exchange
	conn *websocket.Conn
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	events chan voice.Event
	// wg counts the goroutines that emit events or write the socket for the
	// session: its reader, tap pump, player, event forwarder, delegations
	// waiting for their question and Interrupt's sends. Tool runs are not
	// counted: Close cancels them and does not wait.
	wg sync.WaitGroup

	// evQueue holds events for forward to hand to Events, so a slow reader
	// of Events never stops the socket's reader, and with it the speech.
	evMu    sync.Mutex
	evQueue []voice.Event
	evWake  chan struct{}

	writeMu sync.Mutex
	dead    atomic.Bool // the connection is closed

	started      chan struct{} // session.started arrived
	startErr     chan error    // the session failed before it started
	ended        chan struct{} // the reader returned
	serverClosed chan struct{} // session.closed arrived
	finished     chan struct{} // Close returned

	player *player

	factMu     sync.Mutex
	facts      []*factAudit
	factErrors atomic.Int64

	mu                  sync.Mutex
	closing             bool
	release             func() bool // stops the close on the exchange's end
	over                bool        // the session ended and said so on Events
	nextID              int
	pending             map[string]chan error // acknowledgements awaited, by client event id
	pendingDone         bool
	delegations         map[string]bool
	lastAppliedRevision uint64          // newest RoomView revision a delegation applied
	turn                strings.Builder // heard since the last EventUserTurn, for the record only
}

var _ voice.Session = (*session)(nil)

// newSession starts a session's goroutines on conn. It keeps a copy of x.
func newSession(ctx context.Context, cfg Config, x *voice.Exchange, conn *websocket.Conn) *session {
	sctx, cancel := context.WithCancel(ctx)
	s := &session{
		cfg:          cfg,
		x:            *x,
		conn:         conn,
		log:          cfg.Logger.With("session_id", x.SessionID),
		ctx:          sctx,
		cancel:       cancel,
		events:       make(chan voice.Event, 64),
		started:      make(chan struct{}),
		startErr:     make(chan error, 1),
		ended:        make(chan struct{}),
		serverClosed: make(chan struct{}),
		finished:     make(chan struct{}),
		pending:      map[string]chan error{},
		delegations:  map[string]bool{},
		evWake:       make(chan struct{}, 1),
	}
	if x.Room != nil {
		s.lastAppliedRevision = x.Room().Revision
	}
	s.player = newPlayer(s)
	s.wg.Add(4)
	go s.read()
	go s.pumpTap()
	go s.player.run()
	go s.forward()
	return s
}

// Say has GPT-Live say text, in its own words: session-wide commentary. It
// returns once GPT-Live has spoken it: a speech began after the text went
// in, and every speech since has played and been followed by sayQuiet of
// quiet. The server's acknowledgement says only that the text reached the
// model, so an exchange that ended on it would cut the speech.
func (s *session) Say(ctx context.Context, text string, mode voice.SayMode) error {
	items := make([]appendItem, 0, 1)
	for _, c := range chunks(text) {
		items = append(items, appendItem{typ: evCommentaryAppend, content: c})
	}
	s.log.Debug("gptlive say", "mode", mode.String(), "appends", len(items))
	var audit *factAudit
	if mode == voice.Fact || mode == voice.Answer {
		audit = s.beginFactAudit(text, s.x.IssueKey)
	}
	mark, _, _ := s.player.state()
	if err := s.appendAll(ctx, nil, items); err != nil {
		if audit != nil {
			s.discardFactAudit(audit)
		}
		return err
	}
	return s.spoken(ctx, mark)
}

// spoken waits until GPT-Live has spoken since mark speeches had begun.
func (s *session) spoken(ctx context.Context, mark int) error {
	start := time.NewTimer(sayStartTimeout)
	defer start.Stop()
	quiet := time.NewTimer(sayQuiet)
	defer quiet.Stop()
	for {
		begun, ended, changed := s.player.state()
		startC := start.C
		if begun > mark {
			startC = nil
		}
		if begun > mark && begun == ended {
			quiet.Reset(sayQuiet)
		} else {
			quiet.Stop()
		}
		select {
		case <-changed:
		case <-quiet.C:
			return nil
		case <-startC:
			return ErrNoSpeech
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ended:
			return s.lostErr()
		}
	}
}

// Deliver returns result, an AnswerResult, on delegation callID: its
// Context as silent thinking, then its Say as commentary for GPT-Live to
// say. A holder's late answer reaches the delegation that asked for it this
// way.
func (s *session) Deliver(ctx context.Context, callID string, result json.RawMessage) error {
	s.mu.Lock()
	known := s.delegations[callID]
	s.mu.Unlock()
	if !known {
		return fmt.Errorf("%w: %q", ErrUnknownCall, callID)
	}
	return s.deliver(ctx, callID, result)
}

func (s *session) deliver(ctx context.Context, callID string, result json.RawMessage) error {
	var r voice.AnswerResult
	if err := json.Unmarshal(result, &r); err != nil {
		return fmt.Errorf("gptlive: result for %q is not an answer result: %w", callID, err)
	}
	var audit *factAudit
	if r.Say != "" {
		audit = s.beginFactAudit(r.Say, r.IssueKey)
	}
	var items []appendItem
	for _, c := range chunks(r.Context) {
		items = append(items, appendItem{typ: evThinkingAppend, content: c})
	}
	for _, c := range chunks(r.Say) {
		items = append(items, appendItem{typ: evCommentaryAppend, content: c})
	}
	if len(items) == 0 {
		// GPT-Live is told not to guess while it waits: a delegation with
		// no result would leave it waiting.
		items = append(items, appendItem{typ: evCommentaryAppend, content: nothingFound})
	}
	if err := s.appendAll(ctx, &callID, items); err != nil {
		if audit != nil {
			s.discardFactAudit(audit)
		}
		return err
	}
	return nil
}

// Interrupt stops the model's speech now: nothing more of it plays, and
// GPT-Live is told to stop. Its next speech, after a pause, plays.
func (s *session) Interrupt() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	id := s.newIDLocked()
	s.wg.Add(1)
	s.mu.Unlock()
	s.player.interrupt()
	go func() {
		defer s.wg.Done()
		if err := s.send(contextAppend{Type: evInstructionsAppend, EventID: id, Content: stopInstruction}); err != nil {
			s.log.Warn("gptlive: tell the model to stop", "error", err)
		}
	}()
}

// Events reports the session's speech, what GPT-Live heard and said, its
// delegations and its failures. It closes once Close returns.
func (s *session) Events() <-chan voice.Event { return s.events }

// Close ends the session within 100 ms: nothing more plays, the server is
// asked to finalize for at most closeGrace, and the connection closes. A
// tool run under way is cancelled, not awaited.
func (s *session) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		<-s.finished
		return nil
	}
	s.closing = true
	release := s.release
	s.mu.Unlock()
	if release != nil {
		release()
	}

	deadline := time.Now().Add(closeGrace)
	s.player.stop()
	if s.lockWrite(deadline) {
		sent := false
		if !s.dead.Load() {
			data, _ := json.Marshal(sessionClose{Type: evSessionClose, EventID: "gw_close"})
			_ = s.conn.SetWriteDeadline(deadline)
			sent = s.conn.WriteMessage(websocket.TextMessage, data) == nil
		}
		s.writeMu.Unlock()
		if sent {
			wait := time.NewTimer(time.Until(deadline))
			select {
			case <-s.serverClosed:
			case <-s.ended:
			case <-wait.C:
			}
			wait.Stop()
		}
	}
	s.cancel()
	s.abort()
	s.wg.Wait()
	s.failPending(ErrClosed)
	if s.player.speaking() {
		select {
		case s.events <- voice.Event{Kind: voice.EventSpeechEnded}:
		default:
		}
	}
	close(s.events)
	close(s.finished)
	return nil
}

// lockWrite takes writeMu by deadline, and reports whether it did. A write
// under way holds it for a moment; one stuck on a stalled peer holds it for
// writeTimeout, and Close does not wait for that.
func (s *session) lockWrite(deadline time.Time) bool {
	for {
		if s.writeMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// teardown ends a session Open is abandoning.
func (s *session) teardown() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.cancel()
	s.abort()
	s.wg.Wait()
	s.failPending(ErrClosed)
	close(s.finished)
}

// abort closes the connection at once. A read or write blocked on it
// returns.
func (s *session) abort() {
	if s.dead.CompareAndSwap(false, true) {
		_ = s.conn.Close()
	}
}

// send writes one control event. Input audio uses sendAudio: it must stop
// under the exchange's context before Close waits for finalization.
func (s *session) send(event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("gptlive: encode event: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.dead.Load() {
		return s.lostErr()
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		s.abort()
		return ErrDisconnected
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		s.abort()
		return ErrDisconnected
	}
	return nil
}

// appendItem is one context append.
type appendItem struct{ typ, content string }

// appendAll sends items on delegationID (nil for the whole session), in
// order, then waits until the server has acknowledged each.
func (s *session) appendAll(ctx context.Context, delegationID *string, items []appendItem) error {
	waits := make([]chan error, 0, len(items))
	for _, item := range items {
		id, ack := s.track()
		if err := s.send(contextAppend{Type: item.typ, EventID: id, DelegationID: delegationID, Content: item.content}); err != nil {
			s.resolve(id, nil)
			return err
		}
		waits = append(waits, ack)
	}
	for _, ack := range waits {
		select {
		case err := <-ack:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// track makes a client event id and the channel its acknowledgement or
// error arrives on.
func (s *session) track() (string, chan error) {
	ack := make(chan error, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.newIDLocked()
	if s.pendingDone {
		if s.closing {
			ack <- ErrClosed
		} else {
			ack <- ErrDisconnected
		}
		return id, ack
	}
	s.pending[id] = ack
	return id, ack
}

func (s *session) newIDLocked() string {
	s.nextID++
	return "gw_" + strconv.Itoa(s.nextID)
}

// resolve answers the command with client event id, and reports whether
// one was awaited.
func (s *session) resolve(id string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ack, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
		ack <- err
	}
	return ok
}

// failPending answers every awaited command with err, and every later one.
func (s *session) failPending(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ack := range s.pending {
		ack <- err
		delete(s.pending, id)
	}
	s.pendingDone = true
}

// lostErr is why a call on a session whose connection has ended fails:
// ErrClosed after Close, else ErrDisconnected.
func (s *session) lostErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return ErrClosed
	}
	return ErrDisconnected
}

// emit queues e for Events. It never waits: forward hands the queue on.
func (s *session) emit(e *voice.Event) {
	s.evMu.Lock()
	s.evQueue = append(s.evQueue, *e)
	s.evMu.Unlock()
	signal(s.evWake)
}

// forward hands queued events to Events, in order, until the session closes.
// A session lives for one exchange, so the queue stays small even when
// nobody reads Events.
func (s *session) forward() {
	defer s.wg.Done()
	for {
		s.evMu.Lock()
		if len(s.evQueue) == 0 {
			s.evMu.Unlock()
			select {
			case <-s.evWake:
				continue
			case <-s.ctx.Done():
				return
			}
		}
		e := s.evQueue[0]
		s.evQueue = s.evQueue[1:]
		s.evMu.Unlock()
		select {
		case s.events <- e:
		case <-s.ctx.Done():
			return
		}
	}
}

// read handles the server's events until the connection ends.
func (s *session) read() {
	defer s.wg.Done()
	defer close(s.ended)
	started := false
	for {
		kind, data, err := s.conn.ReadMessage()
		if err != nil {
			s.lost(err, started)
			return
		}
		if kind != websocket.TextMessage {
			continue
		}
		var ev serverEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			s.log.Warn("gptlive: unreadable server event", "error", err)
			continue
		}
		switch ev.Type {
		case evSessionStarted:
			if !started {
				started = true
				close(s.started)
			}
		case evError:
			s.onError(&ev, started)
		case evInstructionsAppended, evThinkingAppended, evCommentaryAppended:
			s.resolve(ev.ClientEventID, nil)
		case evOutputAudioDelta:
			s.onAudio(ev.Delta)
		case evInputTranscriptDelta:
			s.onHeard(ev.Delta)
		case evOutputTranscript:
			s.onSaid(ev.Delta)
		case evDelegationCreated:
			s.onDelegation(&ev)
		case evSessionClosed:
			s.onClosed(ev.Reason, started)
		}
	}
}

// lost handles the end of the connection. A WebSocket close's text is
// server-controlled and can quote room words, so it never reaches an error
// or a log.
func (s *session) lost(_ error, started bool) {
	s.abort()
	s.failPending(s.lostErr())
	if !started {
		s.startFailed(ErrDisconnected)
		return
	}
	s.mu.Lock()
	report := !s.closing && !s.over
	s.over = true
	s.mu.Unlock()
	if report {
		s.log.Warn("gptlive: the Live connection ended")
		s.emit(&voice.Event{Kind: voice.EventErr, Err: ErrDisconnected})
	}
}

func (s *session) startFailed(err error) {
	select {
	case s.startErr <- err:
	default:
	}
}

func (s *session) onError(ev *serverEvent, started bool) {
	e := &APIError{Type: ev.Error.Type, Code: ev.Error.Code, Message: ev.Error.Message, Param: ev.Error.Param}
	id := ev.Error.ClientEventID
	if id == "" {
		id = ev.ClientEventID
	}
	if id != "" && s.resolve(id, e) {
		return
	}
	if !started {
		s.startFailed(e)
		return
	}
	s.log.Warn("gptlive: error event", "code", e.code())
	s.emit(&voice.Event{Kind: voice.EventErr, Err: e})
}

// onClosed handles the server's final event. Its reason is free-form server
// text, so it never reaches an error or a log.
func (s *session) onClosed(_ string, started bool) {
	select {
	case <-s.serverClosed:
		return
	default:
		close(s.serverClosed)
	}
	if !started {
		s.startFailed(ErrDisconnected)
		return
	}
	s.mu.Lock()
	report := !s.closing && !s.over
	s.over = true
	s.mu.Unlock()
	if report {
		s.log.Warn("gptlive: the server ended the session")
		s.emit(&voice.Event{Kind: voice.EventErr, Err: ErrDisconnected})
	}
}

// onAudio queues the model's speech to play, unless the session is closing:
// the playback gate.
func (s *session) onAudio(delta string) {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return
	}
	data, err := base64.StdEncoding.DecodeString(delta)
	if err != nil {
		s.log.Warn("gptlive: undecodable output audio", "error", err)
		return
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	if s.player.push(pcm, time.Now()) {
		s.flushTurn()
	}
}

// onHeard keeps what GPT-Live heard the room say, for the user-turn record
// only. It is never a delegation's question and never the backend's context
// (Main, after Sol's eighteenth and nineteenth reviews of #191): those come
// from the room's own finals, which a take-back can withdraw.
func (s *session) onHeard(delta string) {
	s.mu.Lock()
	s.turn.WriteString(delta)
	s.mu.Unlock()
}

// onSaid reports and keeps what GPT-Live says, as each fragment arrives.
// GPT-Live sends a fragment of its words just before their audio, in order,
// so the own-speech filter has the words before the room can hear them.
// The fragments carry start_ms on a session timeline that, measured from
// the office unit, runs about 1.5 times real time, so the session matches
// none of them to its audio. The words that arrive after Interrupt or the
// Supervisor cut the speech, until the model's audio pauses, are not
// Wispr's speech: the room did not hear them, and the Supervisor's own-speech
// filter would take a person's same words for Wispr's echo. Words that
// arrived before the cut whose audio never played, up to about 1.4 s of
// them, still count.
func (s *session) onSaid(delta string) {
	if s.player.dropping(time.Now()) {
		return
	}
	s.recordFactText(delta)
	if strings.TrimSpace(delta) != "" {
		s.emit(&voice.Event{Kind: voice.EventAgentText, Text: delta})
	}
}

// flushTurn reports what GPT-Live heard since the last report, as the
// user's turn: at each delegation and as the model starts to speak.
func (s *session) flushTurn() {
	s.mu.Lock()
	text := strings.TrimSpace(s.turn.String())
	s.turn.Reset()
	s.mu.Unlock()
	if text != "" {
		s.emit(&voice.Event{Kind: voice.EventUserTurn, Text: text})
	}
}

// onDelegation turns a client delegation, which carries no task text, into
// the Concierge's answer tool: the question is what the asker said, and the
// transcript is the meeting before the call and the exchange since. GPT-Live
// can delegate before its transcript of the question arrives, so ask waits
// for the transcript to settle first.
func (s *session) onDelegation(ev *serverEvent) {
	id := ev.Delegation.ID
	if ev.Delegation.Target != delegationTargetClient || id == "" {
		// The target GPT-Live named is its text, so the record names none.
		s.log.Warn("gptlive: delegation not for the client", "has_id", id != "")
		return
	}
	// Logged so a probe can time the vendor's own acknowledgement against the
	// moment the delegation reached us. It carries no words of the room.
	s.log.Info("gptlive: delegation reached the client", "delegation", id)
	// The room as the delegation arrived, read here, before the worker runs
	// and outside the session's lock: a take-back between the arrival and
	// the worker's first read must end this delegation, not become the
	// state it waits from (Sol's twentieth review of #191, finding 2).
	arrival := s.room()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.delegations[id] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.ask(id, &arrival)
}

// ask builds delegation id's question from the room, then reports the tool
// call and runs it. It first cancels GPT-Live's automatic acknowledgement,
// the documented way and only a best effort: GPT-Live speaks one of its own
// after delegating whatever it is told, and its audio carries no response or
// item id, so nothing here can tell that acknowledgement from an answer, and
// it plays as on main (Main, after Sol's nineteenth review of #191).
// resolveQuestion is the only wait; the user-turn record is flushed once a
// question is in hand.
func (s *session) ask(id string, arrival *voice.RoomView) {
	defer s.wg.Done()
	s.suppressAck(id)
	question, room, ok := s.resolveQuestion(id, arrival)
	if !ok {
		return
	}
	s.flushTurn()
	args, err := json.Marshal(voice.AnswerArgs{Question: question, Transcript: transcriptOf(&room)})
	if err != nil {
		s.log.Warn("gptlive: encode the answer tool's arguments", "error", err)
		return
	}
	call := voice.ToolCall{ID: id, Name: voice.AnswerTool, Args: args}
	s.emit(&voice.Event{Kind: voice.EventToolCall, Call: call})
	go s.answer(call)
}

// room is the room's words as a model may read them now (voice.Exchange.Room),
// or, with no Room, as the session opened with them. Every send after Open
// reads them here, never a copy the session kept (Main, after Sol's fifteenth
// review of #191).
func (s *session) room() voice.RoomView {
	if s.x.Room != nil {
		return s.x.Room()
	}
	return voice.RoomView{Live: true, Asker: s.x.AskerTurn, Request: s.x.AskerTurn, Recent: s.x.Recent}
}

// resolveQuestion is delegation id's question: the room's own request
// (RoomView.Request), owned by final identity and committed with
// Exchange.Claim. A vendor delegation only asks to look at the room; it is
// never the question's identity, and GPT-Live's own transcript is never its
// text (Main, after Sol's eighteenth and nineteenth reviews of #191).
//
// With no request in the room the delegation waits, with no deadline, for
// the room's next change: the final it waits for may come seconds after the
// delegation, and that final's arrival is what schedules the work (Sol's
// nineteenth review, findings 1 and 2). A claim that fails means the room
// moved on between the read and the commit, a later final, a take-back or
// another delegation's claim of the same words, so the room is read again
// rather than sent. The wait ends, completing the delegation silently, when
// the exchange ends (RoomView.Live false), when a newer delegation already
// applied a later revision, or when the room asks nothing though it changed
// since arrival, the room as onDelegation read it: a later final or
// take-back that leaves no request took it back.
func (s *session) resolveQuestion(id string, arrival *voice.RoomView) (question string, room voice.RoomView, ok bool) {
	for {
		room = s.room()
		if s.x.Room != nil && !room.Live {
			s.completeSilently(id)
			return "", room, false
		}
		s.mu.Lock()
		stale := room.Revision != 0 && room.Revision < s.lastAppliedRevision
		if !stale {
			s.lastAppliedRevision = max(s.lastAppliedRevision, room.Revision)
		}
		s.mu.Unlock()
		if stale {
			s.completeSilently(id)
			return "", room, false
		}
		if request := strings.TrimSpace(room.Request); request != "" {
			if s.x.Claim == nil || s.x.Claim(room.Turn, room.Revision) {
				return request, room, true
			}
			// The room moved on between the read and the claim: wait for
			// that change, then read it again.
		} else if s.x.Room == nil || room.Revision > arrival.Revision || room.TakeBacks > arrival.TakeBacks {
			s.completeSilently(id)
			return "", room, false
		}
		wait := room.Changed
		if wait == nil {
			poll := make(chan struct{})
			time.AfterFunc(roomPoll, func() { close(poll) })
			wait = poll
		}
		select {
		case <-wait:
		case <-s.ctx.Done():
			return "", room, false
		}
	}
}

func words(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// suppressAck cancels delegation id's pending automatic acknowledgement with
// an empty thinking append, the way OpenAI's live-delegation guide documents
// ("Suppress automatic acknowledgments while tools run"). It is sent as the
// delegation arrives, before the question is resolved. It is a best effort
// and no more: the guide says it "does not stop audio already generated or
// prevent independent model speech", and the office Pi heard GPT-Live's own
// acknowledgement after it on every run of 2026-11-26. The guide allows more
// updates for the same delegation afterwards, so this never closes it. The
// frame goes out in order, before any result, but its acknowledgement is
// awaited in the background: a server slow to acknowledge must not hold up
// the answer.
func (s *session) suppressAck(id string) {
	eventID, ack := s.track()
	if err := s.send(contextAppend{Type: evThinkingAppend, EventID: eventID, DelegationID: &id, Content: ""}); err != nil {
		s.resolve(eventID, nil)
		if s.ctx.Err() == nil {
			s.log.Warn("gptlive: cancel a delegation's automatic acknowledgement", "delegation", id, "error", err)
		}
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case err := <-ack:
			if err != nil && s.ctx.Err() == nil {
				s.log.Warn("gptlive: cancel a delegation's automatic acknowledgement", "delegation", id, "error", err)
			}
		case <-s.ctx.Done():
		}
	}()
}

// completeSilently ends delegation id with no answer: its request was taken
// back, a newer delegation owns the room, or the exchange ended. It closes
// the delegation thinking-only, with an empty thinking append on its id and
// no commentary, so GPT-Live has no result to speak. It sends no
// session-wide instruction to stay quiet: one would reach every delegation,
// and could silence another one's valid answer (Main, after Sol's nineteenth
// review of #191). What GPT-Live may still say of its own is its own
// acknowledgement, which carries no words of the room.
func (s *session) completeSilently(id string) {
	if err := s.appendAll(s.ctx, &id, []appendItem{{typ: evThinkingAppend, content: ""}}); err != nil && s.ctx.Err() == nil {
		s.log.Warn("gptlive: close a withdrawn delegation", "delegation", id, "error", err)
	}
}

func (s *session) answer(call voice.ToolCall) {
	result, err := s.x.Tools.Run(s.ctx, call)
	if s.ctx.Err() != nil {
		return
	}
	if err != nil {
		// The tool's error can quote the asker's words (a search query), and
		// the journal is outside every delete path, so its text stays out.
		s.log.Warn("gptlive: the answer tool failed", "delegation", call.ID, "timeout", errors.Is(err, context.DeadlineExceeded))
		result, _ = json.Marshal(voice.AnswerResult{Context: lookupFailed})
	}
	if err := s.deliver(s.ctx, call.ID, result); err != nil && s.ctx.Err() == nil {
		s.log.Warn("gptlive: return the answer", "delegation", call.ID, "error", err)
	}
}

// transcriptOf is the backend's context for a delegation: the meeting before
// the call, labelled by speaker, and the asker's words at the call, both as a
// model may read them now (room). It holds nothing GPT-Live heard or said: a
// copy the client kept of what the vendor heard has no final's ownership and
// would carry words a take-back withdrew into the backend (Main, after Sol's
// nineteenth review of #191).
func transcriptOf(room *voice.RoomView) string {
	recent, asker := room.Recent, room.Asker
	var b strings.Builder
	for _, t := range recent {
		if line := turnLine(t); line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if asker = strings.TrimSpace(asker); asker != "" && !askerInRecent(asker, recent) {
		b.WriteString("Asker, calling Wispr: ")
		b.WriteString(asker)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// pumpTap streams the tap's audio to the session at SampleRate. Audio that
// arrives before the session has started waits, up to earlyAudio, and goes
// first once it has.
func (s *session) pumpTap() {
	defer s.wg.Done()
	tap := s.x.Tap
	started := s.started
	conv := resampler{to: SampleRate}
	var early [][]int16
	held := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-started:
			started = nil
			for _, pcm := range early {
				if err := s.sendAudio(pcm); err != nil {
					return
				}
			}
			early, held = nil, 0
		case frame, ok := <-tap:
			if !ok {
				// The exchange ended: what it heard while the session
				// started must not reach OpenAI now.
				tap, early, held = nil, nil, 0
				continue
			}
			pcm := conv.convert(frame)
			if len(pcm) == 0 {
				continue
			}
			if started != nil {
				if held+len(pcm) <= earlyAudio {
					early = append(early, pcm)
					held += len(pcm)
				}
				continue
			}
			if err := s.sendAudio(pcm); err != nil {
				return
			}
		}
	}
}

// sendAudio writes room audio only while the exchange remains open. It
// rechecks closing and the exchange context after it owns the socket writer:
// a frame waiting for writeMu cannot cross cancellation or session.close.
func (s *session) sendAudio(pcm []int16) error {
	data := pcm16Bytes(pcm)
	event, err := json.Marshal(inputAudioAppend{Type: evInputAudioAppend, Audio: base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return fmt.Errorf("gptlive: encode input audio: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing || s.ctx.Err() != nil {
		return ErrClosed
	}
	if s.dead.Load() {
		return s.lostErr()
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		s.abort()
		return ErrDisconnected
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, event); err != nil {
		s.abort()
		return ErrDisconnected
	}
	return nil
}

func pcm16Bytes(pcm []int16) []byte {
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

// format is the model's speech as it plays.
var format = audio.AudioFormat{SampleRate: SampleRate, Channels: 1, Encoding: audio.EncodingPCM16}

// silenceLevel is the loudest sample the player takes for silence, about
// -48 dBFS. GPT-Live sends no event at the end of its speech, and audio of
// silence between its speeches must not open the speaker's stream, which
// holds back every Jabra light write while it is open.
const silenceLevel = 128

// player plays the model's speech on the voice input, one Playback a
// speech. A speech starts with audible audio and ends once the model's
// audio has been silent or absent for speechGap and what it queued has
// played. Silence inside a speech plays; silence outside one does not.
type player struct {
	s    *session
	wake chan struct{}

	mu      sync.Mutex
	queue   []int16
	last    time.Time // when the model's last audible audio arrived
	muted   bool      // drop the model's audio until it pauses: an interrupted speech
	cut     bool      // end the speech under way now
	stopped bool      // the session closed: nothing more plays
	// Every sample the model sends is counted once on arrival and once again
	// on the path it takes, each where it happens, never derived from the
	// others: recvFrames must equal the sum of the rest, and a path that
	// dropped audio without a counter of its own would show as the gap.
	recvFrames    int // every sample the model sent
	playedFrames  int // queued for the speaker
	mutedFrames   int // dropped as the rest of a cut speech
	quietFrames   int // silence outside a speech, which never plays
	stoppedFrames int // arrived after the session closed
	begun         int // speeches begun
	ended         int // speeches ended
	changed       chan struct{}
}

func newPlayer(s *session) *player {
	return &player{s: s, wake: make(chan struct{}, 1), changed: make(chan struct{})}
}

// reportAudio logs every sample the model has sent this session and where
// each went, from one snapshot under the lock. sent_ms must equal the sum of
// the rest; a path that dropped audio with no counter of its own shows as the
// difference.
func (p *player) reportAudio() {
	p.mu.Lock()
	recv, played := p.recvFrames, p.playedFrames
	muted, quiet, stopped := p.mutedFrames, p.quietFrames, p.stoppedFrames
	p.mu.Unlock()
	ms := func(n int) int { return n * 1000 / SampleRate }
	p.s.log.Info("gptlive: the model's audio this session",
		"sent_ms", ms(recv), "to_speaker_ms", ms(played),
		"muted_ms", ms(muted), "quiet_ms", ms(quiet), "stopped_ms", ms(stopped))
}

// push queues the model's audio and reports whether it starts a speech.
// GPT-Live's audio plays as it comes: its output carries no response or item
// id and has no completion event, so no client rule can tell a delegation's
// acknowledgement from an answer, and every hold tried either clipped a real
// answer or let an acknowledgement through (Sol's nineteenth review of #191,
// findings 4 to 6).
func (p *player) push(pcm []int16, now time.Time) bool {
	p.mu.Lock()
	defer signal(p.wake)
	defer p.mu.Unlock()
	p.recvFrames += len(pcm)
	if p.stopped {
		p.stoppedFrames += len(pcm)
		return false
	}
	inSpeech := !p.last.IsZero() && now.Sub(p.last) < speechGap
	if !audible(pcm) {
		switch {
		case inSpeech && !p.muted:
			p.enqueueLocked(pcm)
		case inSpeech:
			p.mutedFrames += len(pcm)
		default:
			// Silence outside a speech never plays.
			p.quietFrames += len(pcm)
		}
		return false
	}
	p.last = now
	if inSpeech {
		if !p.muted {
			p.enqueueLocked(pcm)
		} else {
			p.mutedFrames += len(pcm)
		}
		return false
	}
	p.muted = false
	p.enqueueLocked(pcm)
	return true
}

func (p *player) enqueueLocked(pcm []int16) {
	p.playedFrames += len(pcm)
	p.queue = append(p.queue, pcm...)
	if over := len(p.queue) - maxQueuedAudio; over > 0 {
		p.queue = p.queue[over:]
	}
}

func audible(pcm []int16) bool {
	for _, v := range pcm {
		if v > silenceLevel || v < -silenceLevel {
			return true
		}
	}
	return false
}

// stop drops what is queued and everything after it, and ends the speech
// under way: the session is closing.
func (p *player) stop() {
	p.mu.Lock()
	p.queue, p.muted, p.cut, p.stopped = nil, true, true, true
	p.mu.Unlock()
	signal(p.wake)
}

// dropping reports whether the model's words arriving at now are dropped:
// the session has closed, or they belong to a speech Interrupt or the
// Supervisor cut, which goes on until the model's audio pauses for
// speechGap.
func (p *player) dropping(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped || (p.muted && !p.last.IsZero() && now.Sub(p.last) < speechGap)
}

// interrupt drops what is queued, ends the speech under way, and drops the
// rest of it as it arrives.
func (p *player) interrupt() {
	p.mu.Lock()
	p.queue, p.muted, p.cut = nil, true, true
	p.mu.Unlock()
	signal(p.wake)
}

// state reports how many speeches have begun and ended, and a channel that
// closes when either changes.
func (p *player) state() (begun, ended int, changed <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.begun, p.ended, p.changed
}

// speaking reports whether a speech is under way.
func (p *player) speaking() bool {
	begun, ended, _ := p.state()
	return begun != ended
}

// count records a speech beginning or ending. At each end it reports where
// every sample the model has sent this session went (reportAudio), so a probe
// can tell the model repeating itself from the client dropping or duplicating
// a stretch.
func (p *player) count(began bool) {
	p.mu.Lock()
	if began {
		p.begun++
	} else {
		p.ended++
	}
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	if !began {
		p.reportAudio()
	}
}

func (p *player) run() {
	defer p.s.wg.Done()
	ctx := p.s.ctx
	var speech voice.Playback
	end := func() {
		speech = nil
		p.s.finishFactAudits()
		p.count(false)
		p.s.emit(&voice.Event{Kind: voice.EventSpeechEnded})
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		p.mu.Lock()
		cut := p.cut
		p.cut = false
		n := min(len(p.queue), writeFrame)
		var frame []byte
		if n > 0 {
			frame = pcm16Bytes(p.queue[:n])
			p.queue = p.queue[n:]
		}
		idle := time.Since(p.last)
		p.mu.Unlock()

		switch {
		case cut && speech != nil:
			end()
			continue
		case n > 0:
			// An Interrupt or Close since the frame was taken dropped it
			// with the rest of the speech.
			if p.dropped() {
				continue
			}
			if speech == nil {
				speech = p.s.x.Out.Begin()
				p.count(true)
				p.s.emit(&voice.Event{Kind: voice.EventSpeechStarted})
			}
			if err := speech.Write(ctx, frame, format); err != nil {
				if ctx.Err() != nil {
					return
				}
				// The Supervisor cancelled the voice input: the rest of this
				// speech does not play.
				p.mu.Lock()
				p.queue, p.muted = nil, true
				p.mu.Unlock()
				end()
			}
			continue
		case speech != nil && idle >= speechGap:
			if err := speech.Drain(ctx); err != nil && ctx.Err() != nil {
				return
			}
			end()
			continue
		}
		wait := time.Hour
		if speech != nil {
			wait = speechGap - idle
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// refusal is a structured code for a refused upgrade. The body can echo a
// credential or room words and Open's caller logs its error, so it is never
// returned.
func refusal(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	return " (" + openaiwif.ErrorCode(data, "") + ")"
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// dropped reports whether an Interrupt or Close has dropped the speech
// since the run loop last looked.
func (p *player) dropped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cut || p.muted || p.stopped
}
