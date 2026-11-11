package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
)

const (
	certificateExpiryWarningKey = "google-certificate-expiry"
	certificateExpiryCheckEvery = 24 * time.Hour
)

type certificateExpiryReader func(string) (time.Time, bool, error)

func certificateCredentialPath(configuredPath string) string {
	if configuredPath = strings.TrimSpace(configuredPath); configuredPath != "" {
		return configuredPath
	}
	return strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
}

func refreshCertificateExpiryWarning(registry *warningsRegistry, configuredPath string, now time.Time, readExpiry certificateExpiryReader) {
	credentialPath := certificateCredentialPath(configuredPath)
	if credentialPath == "" {
		registry.Clear(certificateExpiryWarningKey)
		return
	}
	notAfter, ok, err := readExpiry(credentialPath)
	if err != nil {
		registry.Set(certificateExpiryWarningKey, fmt.Sprintf("Google credential certificate could not be read: %v; see the renewal runbook in docs/pi-setup.md.", err))
		return
	}
	if warning := googleauth.CertificateExpiryWarning(now, notAfter, ok); warning != "" {
		registry.Set(certificateExpiryWarningKey, warning)
		return
	}
	registry.Clear(certificateExpiryWarningKey)
}

func startCertificateExpiryChecks(ctx context.Context, cfgStore *config.Store, registry *warningsRegistry, interval time.Duration, readExpiry certificateExpiryReader) <-chan struct{} {
	var refreshMu sync.Mutex
	refresh := func() {
		refreshMu.Lock()
		defer refreshMu.Unlock()
		refreshCertificateExpiryWarning(registry, cfgStore.Get().GoogleCredentialsFile, time.Now(), readExpiry)
	}
	refresh()
	onConfigChange(cfgStore, func(*config.Config) {
		refresh()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	return done
}
