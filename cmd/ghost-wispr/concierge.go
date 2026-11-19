package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// startConcierge builds the voice's Concierge: its Gemini model client on the
// unit's genai settings, Dispatch's record with the unit's own token, and,
// in the background, the unit's Envoy session for asking an issue's agent. With
// no token or no Dispatch URL the tools say they cannot reach Dispatch yet;
// with no Envoy the Concierge answers from the record alone until the session
// opens. stop closes the Envoy session.
func startConcierge(ctx context.Context, cfg *config.Config, store *storage.SQLiteStore, logger *slog.Logger, changed func(sessionID string)) (*concierge.Concierge, func(), error) {
	options := genAIOptions(cfg, cfg.GeminiAPIKey)
	if err := addVertexCredentials(ctx, cfg, options); err != nil {
		return nil, nil, fmt.Errorf("concierge: %w", err)
	}
	generator, err := concierge.NewGenerator(ctx, options)
	if err != nil {
		return nil, nil, fmt.Errorf("concierge: the model client: %w", err)
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
		client, err := dispatch.NewClient(cfg.Concierge.DispatchURL, cfg.DispatchToken, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("concierge: %w", err)
		}
		records = client
	}
	c, err := concierge.New(&concierge.Config{
		Model:     cfg.Concierge.Model,
		Generator: generator,
		Records:   records,
		Store:     store,
		PublicURL: cfg.PublicURL,
		Logger:    logger,
		Changed:   changed,
	})
	if err != nil {
		return nil, nil, err
	}
	if cfg.Concierge.EnvoyURL == "" {
		logger.Warn("concierge: no concierge.envoy_url; the voice cannot ask an issue's agent")
		return c, func() {}, nil
	}
	listener, err := envoy.NewListener(cfg.Concierge.EnvoyURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("concierge: %w", err)
	}
	state, err := concierge.LoadEnvoyState(store)
	if err != nil {
		return nil, nil, fmt.Errorf("concierge: the Envoy session's state: %w", err)
	}
	sessions := make(chan envoySession, 1)
	sessionCtx, cancel := context.WithCancel(ctx)
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
				Deliver:       c.Deliver,
				Logger:        logger,
			})
			if err == nil {
				c.SetAgents(session)
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
	stop := func() {
		cancel()
		if session, ok := <-sessions; ok && session != nil {
			closeCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			if err := session.Close(closeCtx); err != nil {
				logger.Warn("concierge: close the Envoy session", "error", err)
			}
		}
	}
	return c, stop, nil
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
// deleted; *concierge.Concierge implements it.
type conciergeSessions interface {
	SessionEnded(sessionID string) error
	SessionDeleted(sessionID string)
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
			log.Printf("warning: concierge: cancel the questions of ended session %s: %v", sessionID, err)
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

// cancelRecoveredQuestions cancels the questions still working of each session
// the start-up recovery ended, as a session end does while the unit runs: their
// late answers stay on the session's page and are never announced.
func cancelRecoveredQuestions(store *storage.SQLiteStore, sessionIDs []string, logf func(string, ...any)) {
	for _, id := range sessionIDs {
		if _, err := store.CancelWorkingVoiceTasks(id); err != nil {
			logf("warning: cancel the questions of recovered session %s: %v", id, err)
		}
	}
}

// lazyConcierge is the voice's Concierge, started at the first call that
// needs it: a unit whose voice is off at start-up runs no Concierge (R13)
// until a person turns the voice on. One that cannot start fails each answer
// as unreachable, so the voice says sorry and plays the error sound.
type lazyConcierge struct {
	startFn func() (*concierge.Concierge, func(), error)
	onStart func(*concierge.Concierge)
	logger  *slog.Logger
	answers chan voice.PendingAnswer
	done    chan struct{}

	once   sync.Once
	mu     sync.Mutex
	c      *concierge.Concierge
	stop   func()
	err    error
	closed bool
}

func newLazyConcierge(start func() (*concierge.Concierge, func(), error), onStart func(*concierge.Concierge), logger *slog.Logger) *lazyConcierge {
	return &lazyConcierge{startFn: start, onStart: onStart, logger: logger, answers: make(chan voice.PendingAnswer), done: make(chan struct{})}
}

// start starts the Concierge once, and returns it or why it did not start.
func (l *lazyConcierge) start() (*concierge.Concierge, error) {
	l.once.Do(func() {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			l.err = errors.New("the unit is shutting down")
			return
		}
		c, stop, err := l.startFn()
		l.mu.Lock()
		l.c, l.stop, l.err = c, stop, err
		l.mu.Unlock()
		if err != nil {
			l.logger.Warn("concierge: did not start; the voice answers that it cannot reach Dispatch", "error", err)
			return
		}
		if l.onStart != nil {
			l.onStart(c)
		}
		go l.forward(c.Answers())
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c, l.err
}

// started returns the Concierge if it has started, without starting it.
func (l *lazyConcierge) started() *concierge.Concierge {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c
}

func (l *lazyConcierge) forward(in <-chan voice.PendingAnswer) {
	for {
		select {
		case a, ok := <-in:
			if !ok {
				return
			}
			select {
			case l.answers <- a:
			case <-l.done:
				return
			}
		case <-l.done:
			return
		}
	}
}

// Stop stops the Concierge if it started, at shutdown.
func (l *lazyConcierge) Stop() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	stop := l.stop
	l.mu.Unlock()
	close(l.done)
	if stop != nil {
		stop()
	}
}

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

func (l *lazyConcierge) NeverMind(ctx context.Context, sessionID string) error {
	if c := l.started(); c != nil {
		return c.NeverMind(ctx, sessionID)
	}
	return nil
}

// unreachableTools answers every tool of a Concierge that did not start.
type unreachableTools struct{ err error }

func (u unreachableTools) Run(context.Context, voice.ToolCall) (json.RawMessage, error) {
	return nil, fmt.Errorf("concierge: %w: %w", voice.ErrUnreachable, u.err)
}
