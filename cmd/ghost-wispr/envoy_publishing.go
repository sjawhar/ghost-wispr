package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// envoyWarningKey keys the /api/status warning that Envoy events are not sent
// because envoy_hub_url names no listener.
const envoyWarningKey = "envoy"

// envoyPublishing owns the Envoy publisher the settings choose. Each settings
// change builds a new one, or none, and hands it to the session manager. A
// publish reaches the hub's listener over the tailnet with no credential, so
// nothing is read before the first event; an event the hub does not take
// stays pending, and each sweep sends it again.
type envoyPublishing struct {
	store    *storage.SQLiteStore
	manager  *session.Manager
	warnings *warningsRegistry

	current atomic.Pointer[envoy.Publisher]
}

// configure builds the publisher cfg asks for, or turns Envoy off. A config
// with Envoy off or no envoy_hub_url sends nothing; the startup warning names
// a missing URL.
func (e *envoyPublishing) configure(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	if !cfg.EnvoyEnabled || strings.TrimSpace(cfg.EnvoyHubURL) == "" {
		e.warnings.Clear(envoyWarningKey)
		e.set(nil)
		return
	}
	pub, err := envoy.NewPublisher(cfg.EnvoyHubURL, e.store, envoy.WithLogger(logger), envoy.WithPublicURL(cfg.PublicURL))
	if err != nil {
		e.warnings.Set(envoyWarningKey, fmt.Sprintf("Envoy events are not sent: %v", err))
		e.set(nil)
		return
	}
	e.warnings.Clear(envoyWarningKey)
	e.set(pub)
	log.Printf("envoy hub publishing enabled")
	go func() {
		_ = e.manager.Run(ctx, func(ctx context.Context) error {
			e.sweepWith(ctx, pub)
			return nil
		})
	}()
}

func (e *envoyPublishing) set(pub *envoy.Publisher) {
	e.current.Store(pub)
	if pub == nil {
		// A nil *Publisher in the interface would not compare equal to nil.
		e.manager.SetEventPublisher(nil)
		return
	}
	e.manager.SetEventPublisher(pub)
}

// sweep sends what Envoy has not received: lifecycle events and completed
// summaries, from before an outage. It runs as work the stop cuts off and
// waits for (session.Manager.Run), since it writes each delivery's status.
func (e *envoyPublishing) sweep(ctx context.Context) {
	if pub := e.current.Load(); pub != nil {
		_ = e.manager.Run(ctx, func(ctx context.Context) error {
			e.sweepWith(ctx, pub)
			return nil
		})
	}
}

func (e *envoyPublishing) sweepWith(ctx context.Context, pub *envoy.Publisher) {
	if err := pub.PublishPendingLifecycleEvents(ctx); err != nil {
		log.Printf("envoy lifecycle sweep error: %v", err)
	}
	ids, err := e.store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		log.Printf("envoy sweep error: %v", err)
		return
	}
	for _, id := range ids {
		if err := pub.PublishSummaryReady(ctx, id); err != nil {
			log.Printf("envoy sweep: session %s: %v", id, err)
		}
	}
}

// publishSummaryReady sends a session's summary_ready in the background, as
// work the stop cuts off and waits for (session.Manager.Go): the publish
// writes its delivery status once the hub answers, and the store must not
// close under it. One the stop cuts off stays pending for the next sweep.
func (e *envoyPublishing) publishSummaryReady(sessionID string) {
	pub := e.current.Load()
	if pub == nil {
		return
	}
	e.manager.Go(func(ctx context.Context) {
		if err := pub.PublishSummaryReady(ctx, sessionID); err != nil {
			slog.Warn("envoy publish failed", "session_id", sessionID, "error", err)
		}
	})
}

// publishRecoveredEnds sends session_ended, in the background, for sessions a
// restart found still active and ended, as work the stop cuts off and waits
// for. An event the stop cuts off stays pending for the next sweep.
func (e *envoyPublishing) publishRecoveredEnds(ids []string) {
	pub := e.current.Load()
	if pub == nil || len(ids) == 0 {
		return
	}
	ids = slices.Clone(ids)
	e.manager.Go(func(ctx context.Context) {
		for _, sessionID := range ids {
			if err := pub.PublishSessionEnded(ctx, sessionID); err != nil {
				slog.Warn("failed to publish recovered session end", "session_id", sessionID, "error", err)
			}
		}
	})
}
