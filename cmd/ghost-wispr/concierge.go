package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/joined"
)

// startConcierge builds the voice's Concierge (newConcierge) and, in the
// background, the unit's Envoy session for asking an issue's agent. With no
// Envoy the Concierge answers from the record alone until the session opens.
// The joined agents' roster, when set, takes the session's joins, floor
// requests and leaves, and the Concierge the rest. stop closes the Envoy
// session and stops the Concierge, waiting for each within its context: at
// the context's end it returns, and what still runs ends behind it, its
// store writes refused once the unit closed the store (storage).
func startConcierge(ctx context.Context, cfg *config.Config, store *storage.SQLiteStore, logger *slog.Logger, changed func(sessionID string), roster *joined.Roster) (*concierge.Concierge, func(context.Context), error) {
	c, err := newConcierge(ctx, cfg, store, logger, changed, joinedAgentsOf(roster))
	if err != nil {
		return nil, nil, err
	}
	stopConcierge := func(ctx context.Context) {
		if err := c.StopWithin(ctx); err != nil {
			logger.Warn("concierge: stopped waiting for its work at the shutdown's deadline; the store refuses what it writes once it closes", "error", err)
		}
	}
	if cfg.Concierge.EnvoyURL == "" {
		logger.Warn("concierge: no concierge.envoy_url; the voice cannot ask an issue's agent")
		return c, stopConcierge, nil
	}
	listener, err := envoy.NewListener(cfg.Concierge.EnvoyURL, newEnvoyHTTP(logger))
	if err != nil {
		c.Stop()
		return nil, nil, fmt.Errorf("concierge: %w", err)
	}
	state, err := concierge.LoadEnvoyState(store)
	if err != nil {
		c.Stop()
		return nil, nil, fmt.Errorf("concierge: the Envoy session's state: %w", err)
	}
	sessions := make(chan envoySession, 1)
	sessionCtx, cancel := context.WithCancel(ctx)
	// This start's opening of the roster: an earlier start's opening that
	// completes, or closes, late touches no session or membership of it
	// (joined.Opening).
	var opening *joined.Opening
	if roster != nil {
		opening = roster.Open()
	}
	go func() {
		defer close(sessions)
		wait := envoyRetryFirst
		for {
			session, err := openEnvoySession(sessionCtx, listener, &envoy.SessionConfig{
				SessionID:     state.SessionID,
				Resume:        state.Resume,
				SaveResume:    state.SaveResume,
				Title:         "Ghost Wispr",
				Dir:           "ghost-wispr",
				Host:          "ghost-wispr",
				Plugin:        "ghost-wispr",
				PluginVersion: Version,
				Deliver:       deliverTo(roster, c),
				Logger:        logger,
			})
			if err == nil {
				c.SetAgents(session)
				if opening != nil {
					opening.SetAgents(session)
				}
				logger.Info("concierge: the Envoy session is open")
				sessions <- session
				return
			}
			logger.Warn("concierge: the Envoy session did not open; trying again", "error", err, "retry_in", wait)
			select {
			case <-sessionCtx.Done():
				return
			case <-time.After(wait):
			}
			wait = nextEnvoyRetry(wait)
		}
	}()
	closeSession := func(ctx context.Context, session envoySession) {
		if opening != nil {
			// The joined agents hear nothing more, and are told so within ctx.
			opening.Close(ctx)
		}
		closeCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		if err := session.Close(closeCtx); err != nil {
			logger.Warn("concierge: close the Envoy session", "error", err)
		}
	}
	stop := func(ctx context.Context) {
		cancel()
		defer stopConcierge(ctx)
		// A session already open is closed now, even past the deadline:
		// its Close gives up at ctx's end (envoy.Session.Close).
		select {
		case session := <-sessions:
			if session != nil {
				closeSession(ctx, session)
			}
			return
		default:
		}
		select {
		case session := <-sessions:
			if session != nil {
				closeSession(ctx, session)
			}
		case <-ctx.Done():
			logger.Warn("concierge: the Envoy session's opening did not end by the shutdown's deadline; a session it still opens is closed when it does", "error", context.Cause(ctx))
			// Nothing else would close a session that opening completes
			// later, nor remove its registration from Envoy.
			go func() {
				if session := <-sessions; session != nil {
					closeSession(context.Background(), session)
				}
			}()
		}
	}
	return c, stop, nil
}

// newConcierge builds the Concierge without its Envoy session: its Gemini
// model client on the unit's genai settings, and Dispatch's record with the
// unit's own token. With no token or no Dispatch URL the tools say they
// cannot reach Dispatch yet.
func newConcierge(ctx context.Context, cfg *config.Config, store *storage.SQLiteStore, logger *slog.Logger, changed func(sessionID string), joinedAgents func() []concierge.JoinedAgent) (*concierge.Concierge, error) {
	options := genAIOptions(cfg, cfg.GeminiAPIKey)
	if err := addVertexCredentials(ctx, cfg, options); err != nil {
		return nil, fmt.Errorf("concierge: %w", err)
	}
	generator, err := concierge.NewGenerator(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("concierge: the model client: %w", err)
	}
	// A nil *dispatch.Client in the interface would be called; keep the
	// interface itself nil when there is none.
	var records concierge.Records
	switch {
	case cfg.DispatchToken == "":
		logger.Warn("concierge: no Dispatch token; the voice says it cannot reach Dispatch yet")
	case cfg.Concierge.DispatchURL == "":
		logger.Warn("concierge: no concierge.dispatch_url; the voice says it cannot reach Dispatch yet")
	default:
		client, err := dispatch.NewClient(cfg.Concierge.DispatchURL, cfg.DispatchToken, newDispatchHTTP(logger))
		if err != nil {
			return nil, fmt.Errorf("concierge: %w", err)
		}
		records = client
	}
	return concierge.New(&concierge.Config{
		Model:     cfg.Concierge.Model,
		Generator: generator,
		Records:   records,
		Store:     store,
		PublicURL: cfg.PublicURL,
		Joined:    joinedAgents,
		Logger:    logger,
		Changed:   changed,
	})
}

// envoySession is the unit's Envoy session as the Concierge uses it.
type envoySession interface {
	concierge.Agents
	Close(ctx context.Context) error
}

// openEnvoySession opens the unit's Envoy session; a test replaces it.
var openEnvoySession = func(ctx context.Context, listener *envoy.Listener, cfg *envoy.SessionConfig) (envoySession, error) {
	session, err := envoy.OpenSession(ctx, listener, cfg)
	if err != nil {
		return nil, err // never a nil *Session in the interface
	}
	return session, nil
}

// envoyRetryFirst is the first wait before the Envoy session is opened again;
// each wait doubles, up to envoyRetryMax, for as long as the unit runs: reach
// can come back without a restart. Until the session opens, the Concierge says
// it cannot reach the agents yet.
var envoyRetryFirst = time.Second

// envoyRetryMax caps the wait between attempts.
const envoyRetryMax = time.Minute

// nextEnvoyRetry is the wait after wait.
func nextEnvoyRetry(wait time.Duration) time.Duration {
	return min(2*wait, envoyRetryMax)
}

// conciergeSessions is what the Concierge does when a session ends or is
// deleted, and when capture closes; *concierge.Concierge implements it.
type conciergeSessions interface {
	SessionEnded(sessionID string) error
	SessionDeleted(sessionID string)
	GateClosed()
}

// conciergeHooks reaches the Concierge from the session manager and the
// session deletes, which are wired before it starts. Until set, and with the
// voice off, they do nothing.
type conciergeHooks struct {
	mu sync.RWMutex
	c  conciergeSessions
}

func (h *conciergeHooks) set(c conciergeSessions) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.c = c
}

func (h *conciergeHooks) get() conciergeSessions {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.c
}

// SessionEnded cancels the questions an ended session still waits on: their
// answers are kept for its page and never announced in a later meeting.
func (h *conciergeHooks) SessionEnded(sessionID string) {
	if c := h.get(); c != nil {
		if err := c.SessionEnded(sessionID); err != nil {
			slog.Warn("concierge: cancel the questions of an ended session", "session_id", sessionID, "error", err)
		}
	}
}

// SessionsDeleted drops what the Concierge holds of deleted sessions.
func (h *conciergeHooks) SessionsDeleted(sessionIDs []string) {
	if c := h.get(); c != nil {
		for _, id := range sessionIDs {
			c.SessionDeleted(id)
		}
	}
}

// GateClosed tells the Concierge that capture closed: it drops what it read
// before a recording session existed. A nil hooks does nothing.
func (h *conciergeHooks) GateClosed() {
	if h == nil {
		return
	}
	if c := h.get(); c != nil {
		c.GateClosed()
	}
}

// cancelRecoveredQuestions cancels the questions still working of each session
// the start-up recovery ended, as a session end does while the unit runs: their
// late answers stay on the session's page and are never announced.
func cancelRecoveredQuestions(store *storage.SQLiteStore, sessionIDs []string) {
	for _, id := range sessionIDs {
		if _, err := store.CancelWorkingVoiceTasks(id); err != nil {
			slog.Warn("cancel the questions of a recovered session", "session_id", id, "error", err)
		}
	}
}

// lazyConcierge is the voice's Concierge. It runs only while the voice is on
// (R13): it starts at the first call that needs it, or when a person turns
// the voice on, and stops when the voice is turned off, so a unit whose voice
// is off runs none. One that cannot start fails each answer as unreachable,
// so the voice says sorry and plays the error sound, until the next settings
// change tries again.
type lazyConcierge struct {
	startFn func() (*concierge.Concierge, func(context.Context), error)
	onStart func(*concierge.Concierge) // with nil when it stops
	logger  *slog.Logger
	answers chan voice.PendingAnswer

	starting sync.Mutex // one start at a time
	// turningOff counts each turn-off under way, which writes the store
	// (VoiceOff) after it detached its Concierge: the shutdown waits for it.
	turningOff sync.WaitGroup
	mu         sync.Mutex
	c          *concierge.Concierge
	stop       func(context.Context)
	fwd        chan struct{} // closed to end c's forward
	err        error         // why the last start failed
	gen        int           // each turn-off and the shutdown change it
	closed     bool
	off        bool // the voice is turned off: no call starts a Concierge
}

var errConciergeOff = errors.New("the voice was turned off")

func newLazyConcierge(start func() (*concierge.Concierge, func(context.Context), error), onStart func(*concierge.Concierge), logger *slog.Logger) *lazyConcierge {
	return &lazyConcierge{startFn: start, onStart: onStart, logger: logger, answers: make(chan voice.PendingAnswer)}
}

// start starts the Concierge if none runs, and returns it or why it did not
// start.
func (l *lazyConcierge) start() (*concierge.Concierge, error) {
	if c, known, err := l.current(); known {
		return c, err
	}
	l.starting.Lock()
	defer l.starting.Unlock()
	if c, known, err := l.current(); known {
		return c, err
	}
	l.mu.Lock()
	gen := l.gen
	l.mu.Unlock()
	c, stop, err := l.startFn()
	l.mu.Lock()
	if l.gen != gen || l.closed || l.off { // turned off or shut down meanwhile
		l.mu.Unlock()
		if err == nil {
			// The shutdown waits for this start within its own deadline.
			stop(context.Background())
		}
		return nil, errConciergeOff
	}
	if err != nil {
		l.err = err
		l.mu.Unlock()
		l.logger.Warn("concierge: did not start; the voice answers that it cannot reach Dispatch", "error", err)
		return nil, err
	}
	fwd := make(chan struct{})
	l.c, l.stop, l.fwd = c, stop, fwd
	l.mu.Unlock()
	if l.onStart != nil {
		l.onStart(c)
	}
	go l.forward(c.Answers(), fwd)
	return c, nil
}

// current returns the running Concierge, or why the last start failed, with
// known false when neither is known and a start is needed.
func (l *lazyConcierge) current() (c *concierge.Concierge, known bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.closed:
		return nil, true, errors.New("the unit is shutting down")
	case l.off:
		return nil, true, errConciergeOff
	case l.c != nil, l.err != nil:
		return l.c, true, l.err
	}
	return nil, false, nil
}

// started returns the Concierge if it runs, without starting it.
func (l *lazyConcierge) started() *concierge.Concierge {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c
}

// followEnabled follows voice.enabled on each settings change: on, the
// Concierge starts in the background, and a failed start is tried again;
// off, the running one stops.
func (l *lazyConcierge) followEnabled(on bool) {
	if !on {
		l.halt(context.Background(), false)
		return
	}
	l.mu.Lock()
	l.err, l.off = nil, false
	l.mu.Unlock()
	go func() { _, _ = l.start() }()
}

// halt stops the running Concierge, for a turn-off or, with shutdown, for
// good. A turn-off first cancels the questions still working and tells their
// holders (VoiceOff), while the Envoy session is open; a shutdown keeps them
// for the next start. A shutdown waits for the running Concierge's work and
// Envoy session within ctx (startConcierge's stop), and also, within ctx,
// for a start under way, which writes the store as it starts (concierge.New)
// and then sees the shutdown and stops what it started (start), and for a
// turn-off under way, which has detached its Concierge and writes the store
// as it turns it off. At ctx's end it waits no longer: the store refuses what
// they write once it closes, and is never closed under a statement they run
// (storage).
func (l *lazyConcierge) halt(ctx context.Context, shutdown bool) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = shutdown
	l.off = true // until followEnabled(true): a call with the voice off starts none
	l.gen++
	c, stop, fwd := l.c, l.stop, l.fwd
	l.c, l.stop, l.fwd, l.err = nil, nil, nil, nil
	if !shutdown && c != nil {
		// Counted under mu: a shutdown that comes after it sees it.
		l.turningOff.Add(1)
		defer l.turningOff.Done()
	}
	l.mu.Unlock()
	if c != nil {
		if l.onStart != nil {
			l.onStart(nil)
		}
		close(fwd)
		if !shutdown {
			c.VoiceOff()
		}
		stop(ctx)
	}
	if shutdown {
		l.waitForStartsAndTurnOffs(ctx)
	}
}

// waitForStartsAndTurnOffs waits, within ctx, for a start under way, which
// holds starting until it has seen the shutdown and stopped what it started
// (no later start takes it: current), and for each turn-off under way, which
// was counted before closed was set (no later one starts: closed).
func (l *lazyConcierge) waitForStartsAndTurnOffs(ctx context.Context) {
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		l.starting.Lock()
		defer l.starting.Unlock()
		l.turningOff.Wait()
	}()
	select {
	case <-ended:
	case <-ctx.Done():
		l.logger.Warn("concierge: a start or turn-off still runs at the shutdown's deadline; the store refuses what it writes once it closes", "error", context.Cause(ctx))
	}
}

func (l *lazyConcierge) forward(in <-chan voice.PendingAnswer, done <-chan struct{}) {
	for {
		select {
		case a, ok := <-in:
			if !ok {
				return
			}
			select {
			case l.answers <- a:
			case <-done:
				return
			}
		case <-done:
			return
		}
	}
}

// Stop stops the Concierge if it runs, at shutdown, waiting within ctx for a
// start or turn-off under way (halt).
func (l *lazyConcierge) Stop(ctx context.Context) { l.halt(ctx, true) }

func (l *lazyConcierge) Tools(x *voice.Exchange) voice.ToolRunner {
	c, err := l.start()
	if err != nil {
		return unreachableTools{err}
	}
	return c.Tools(x)
}

func (l *lazyConcierge) Heard(ctx context.Context, x *voice.Exchange, words string) {
	if c, err := l.start(); err == nil {
		c.Heard(ctx, x, words)
	}
}

func (l *lazyConcierge) Answer(ctx context.Context, x *voice.Exchange, turn string, say voice.SayFunc) error {
	c, err := l.start()
	if err != nil {
		return fmt.Errorf("concierge: %w: %w", voice.ErrUnreachable, err)
	}
	return c.Answer(ctx, x, turn, say)
}

func (l *lazyConcierge) Answers() <-chan voice.PendingAnswer { return l.answers }

// Spoken and NeverMind do nothing before the Concierge started: it then has
// no question to record or cancel.
func (l *lazyConcierge) Spoken(ctx context.Context, taskID string) error {
	if c := l.started(); c != nil {
		return c.Spoken(ctx, taskID)
	}
	return nil
}

// QuestionsAsked is the started Concierge's count, 0 before it started.
func (l *lazyConcierge) QuestionsAsked() uint64 {
	if c := l.started(); c != nil {
		return c.QuestionsAsked()
	}
	return 0
}

func (l *lazyConcierge) NeverMind(ctx context.Context, sessionID string, asked uint64) (bool, error) {
	if c := l.started(); c != nil {
		return c.NeverMind(ctx, sessionID, asked)
	}
	return false, nil
}

// unreachableTools answers every tool of a Concierge that did not start, as
// the Concierge answers a tool that cannot reach Dispatch: a result the voice
// says, with an error that wraps voice.ErrUnreachable (voice.ToolRunner).
type unreachableTools struct{ err error }

func (u unreachableTools) Run(context.Context, voice.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{"error":"I cannot reach Dispatch yet."}`), fmt.Errorf("concierge: %w: %w", voice.ErrUnreachable, u.err)
}
