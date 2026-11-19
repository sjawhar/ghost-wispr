package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// envoyRetryEvery is how often the unit tries again to open its Envoy session
// while it cannot: reach can come back without a restart.
const envoyRetryEvery = time.Minute

// startConcierge builds the voice's Concierge: its Gemini model client on the
// unit's genai settings, Dispatch's record with the unit's own token, and,
// in the background, the unit's Envoy session for asking an issue's agent. With
// no token or no Dispatch URL the tools say they cannot reach Dispatch yet;
// with no Envoy the Concierge answers from the record alone until the session
// opens. stop closes the Envoy session.
func startConcierge(ctx context.Context, cfg *config.Config, store *storage.SQLiteStore, logger *slog.Logger) (*concierge.Concierge, func(), error) {
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
	sessions := make(chan *envoy.Session, 1)
	sessionCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(sessions)
		for {
			session, err := envoy.OpenSession(sessionCtx, listener, &envoy.SessionConfig{
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
			logger.Warn("concierge: the Envoy session did not open; trying again", "error", err, "retry_in", envoyRetryEvery)
			select {
			case <-sessionCtx.Done():
				return
			case <-time.After(envoyRetryEvery):
			}
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
