package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/secrets"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// envoyWarningKey keys the /api/status warning that Envoy events are not sent
// because the signing key cannot be read.
const envoyWarningKey = "envoy"

// envoyPublishing owns the Envoy publisher the settings choose. Each settings
// change builds a new one, or none, and hands it to the session manager. Its
// signing key is read from Secret Manager in the background, so neither the
// settings change nor recording waits on Secret Manager; a key that cannot be
// read stops the events and shows in /api/status, nothing else.
type envoyPublishing struct {
	store    *storage.SQLiteStore
	manager  *session.Manager
	warnings *warningsRegistry
	// openSecrets opens Secret Manager with the unit's Google credential.
	openSecrets func(context.Context, *config.Config) (*secrets.Reader, error)

	current atomic.Pointer[envoy.Publisher]
}

// openSecretManager opens Secret Manager with the Google credential Drive and
// Gemini use (google_credentials_file, else Application Default Credentials).
func openSecretManager(ctx context.Context, cfg *config.Config) (*secrets.Reader, error) {
	credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("load Google credentials: %w", err)
	}
	return secrets.NewReader(ctx, option.WithAuthCredentials(credentials))
}

// configure builds the publisher cfg asks for, or turns Envoy off.
func (e *envoyPublishing) configure(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	if !cfg.EnvoyEnabled || strings.TrimSpace(cfg.EnvoyWebhookURL) == "" {
		e.warnings.Clear(envoyWarningKey)
		e.set(nil)
		return
	}
	settings := *cfg
	name := settings.EnvoyWebhookSecretName
	readKey := func(ctx context.Context) (string, error) {
		reader, err := e.openSecrets(ctx, &settings)
		if err != nil {
			return "", err
		}
		return reader.Read(ctx, name)
	}
	var pub *envoy.Publisher
	pub = envoy.NewPublisher(settings.EnvoyWebhookURL, readKey, e.store,
		envoy.WithLogger(logger),
		envoy.WithPublicURL(settings.PublicURL),
		envoy.WithKeyStatus(func(err error) { e.reportKey(pub, name, err) }),
	)
	e.warnings.Clear(envoyWarningKey)
	e.set(pub)
	log.Printf("envoy webhook publishing enabled")
	go func() {
		if err := pub.CheckSigningKey(ctx); err != nil {
			log.Printf("warning: %v", err)
		}
		e.sweepWith(ctx, pub)
	}()
}

// reportKey shows in /api/status whether pub's signing key reads, while pub
// is the publisher in use.
func (e *envoyPublishing) reportKey(pub *envoy.Publisher, name string, err error) {
	if e.current.Load() != pub {
		return
	}
	if err == nil {
		e.warnings.Clear(envoyWarningKey)
		return
	}
	e.warnings.Set(envoyWarningKey, fmt.Sprintf("Envoy events are not sent: the webhook signing key %s could not be read (%v). Recording is not affected; the events go out once the key reads.", name, err))
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
// summaries, from before an outage or a key that could not be read.
func (e *envoyPublishing) sweep(ctx context.Context) {
	if pub := e.current.Load(); pub != nil {
		e.sweepWith(ctx, pub)
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

// publishSummaryReady sends a session's summary_ready in the background.
func (e *envoyPublishing) publishSummaryReady(sessionID string) {
	pub := e.current.Load()
	if pub == nil {
		return
	}
	go func() {
		if err := pub.PublishSummaryReady(context.Background(), sessionID); err != nil {
			log.Printf("warning: envoy publish failed for %s: %v", sessionID, err)
		}
	}()
}

// publishRecoveredEnds sends session_ended, in the background, for sessions a
// restart found still active and ended.
func (e *envoyPublishing) publishRecoveredEnds(ids []string) {
	pub := e.current.Load()
	if pub == nil || len(ids) == 0 {
		return
	}
	ids = slices.Clone(ids)
	go func() {
		for _, sessionID := range ids {
			if err := pub.PublishSessionEnded(context.Background(), sessionID); err != nil {
				log.Printf("warning: failed to publish recovered session end for %s: %v", sessionID, err)
			}
		}
	}()
}
