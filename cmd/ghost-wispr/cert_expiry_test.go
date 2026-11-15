package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

func TestCertificateExpiryWarningRegistry(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	registry := newWarningsRegistry()
	reader := func(path string) (time.Time, bool, error) {
		switch path {
		case "soon":
			return now.AddDate(0, 0, 10), true, nil
		case "later":
			return now.AddDate(0, 0, 40), true, nil
		case "none":
			return time.Time{}, false, nil
		default:
			return time.Time{}, false, errors.New("unreadable credential")
		}
	}

	refreshCertificateExpiryWarning(registry, "soon", now, reader)
	warnings := statusWarnings(t, registry)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "2026-11-21") || !strings.Contains(warnings[0], "docs/pi-setup.md") {
		t.Fatalf("soon certificate warnings = %q", warnings)
	}

	refreshCertificateExpiryWarning(registry, "later", now, reader)
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("later certificate warnings = %q, want none", warnings)
	}

	refreshCertificateExpiryWarning(registry, "none", now, reader)
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("certificate-free credential warnings = %q, want none", warnings)
	}

	refreshCertificateExpiryWarning(registry, "unreadable", now, reader)
	if warnings := statusWarnings(t, registry); len(warnings) != 1 || !strings.Contains(warnings[0], "could not be read") || !strings.Contains(warnings[0], "docs/pi-setup.md") {
		t.Fatalf("unreadable certificate warnings = %q", warnings)
	}
}

func TestCertificateExpiryWarningRefreshesAfterCredentialPathChange(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	registry := newWarningsRegistry()
	reader := func(path string) (time.Time, bool, error) {
		if path == "first" {
			return now.AddDate(0, 0, 10), true, nil
		}
		return now.AddDate(0, 0, 40), true, nil
	}

	refreshCertificateExpiryWarning(registry, "first", now, reader)
	refreshCertificateExpiryWarning(registry, "second", now, reader)
	if warnings := registry.List(); len(warnings) != 0 {
		t.Fatalf("credential path update left warning behind: %q", warnings)
	}
}

func TestCertificateExpiryWarningUsesADCFallback(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	registry := newWarningsRegistry()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "adc")
	refreshCertificateExpiryWarning(registry, "", now, func(path string) (time.Time, bool, error) {
		if path != "adc" {
			t.Fatalf("credential path = %q, want ADC path", path)
		}
		return now.AddDate(0, 0, 10), true, nil
	})
	if warnings := registry.List(); len(warnings) != 1 {
		t.Fatalf("ADC certificate warnings = %q", warnings)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	refreshCertificateExpiryWarning(registry, "", now, func(string) (time.Time, bool, error) {
		t.Fatal("reader called without a configured or ADC path")
		return time.Time{}, false, nil
	})
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("empty credential path warnings = %q, want none", warnings)
	}
}

func TestCertificateExpiryChecksRefreshOnConfigUpdateAndStop(t *testing.T) {
	store, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) { cfg.GoogleCredentialsFile = "first" }); err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan string, 8)
	done := startCertificateExpiryChecks(ctx, store, registry, time.Hour, func(path string) (time.Time, bool, error) {
		calls <- path
		return time.Now().AddDate(0, 0, 40), true, nil
	})
	select {
	case path := <-calls:
		if path != "first" {
			t.Fatalf("startup path = %q, want first", path)
		}
	case <-time.After(time.Second):
		t.Fatal("certificate expiry check did not run at startup")
	}
	if err := store.Update(func(cfg *config.Config) { cfg.GoogleCredentialsFile = "second" }); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-calls:
		if path != "second" {
			t.Fatalf("settings refresh path = %q, want second", path)
		}
	case <-time.After(time.Second):
		t.Fatal("certificate expiry check did not run after settings update")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("certificate expiry ticker did not stop after cancellation")
	}
	for len(calls) > 0 {
		<-calls
	}
	select {
	case path := <-calls:
		t.Fatalf("certificate expiry ticker continued after cancellation with %q", path)
	case <-time.After(5 * time.Millisecond):
	}
}

func TestCertificateExpiryWarningReadsRealCertificate(t *testing.T) {
	registry := newWarningsRegistry()
	path := googleauthtest.X509CredentialFile(t, "certificate-warning-token")
	refreshCertificateExpiryWarning(registry, path, time.Now(), googleauth.CertExpiry)
	if warnings := registry.List(); len(warnings) != 1 || !strings.Contains(warnings[0], "docs/pi-setup.md") {
		t.Fatalf("real certificate warnings = %q", warnings)
	}
}

func TestCertificateExpiryTickRaisesWarning(t *testing.T) {
	store, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) { cfg.GoogleCredentialsFile = "tick" }); err != nil {
		t.Fatal(err)
	}
	registry := newWarningsRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := startCertificateExpiryChecks(ctx, store, registry, time.Millisecond, func(string) (time.Time, bool, error) {
		if calls.Add(1) == 1 {
			return time.Now().AddDate(0, 0, 40), true, nil
		}
		return time.Now().AddDate(0, 0, 10), true, nil
	})
	deadline := time.After(time.Second)
	for {
		warnings := statusWarnings(t, registry)
		if len(warnings) == 1 && strings.Contains(warnings[0], "expires on") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("tick warnings = %q, want a 10-day expiry warning", warnings)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ticker did not stop after cancellation")
	}
}

func statusWarnings(t *testing.T, registry *warningsRegistry) []string {
	t.Helper()
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), nil, &server.ControlHooks{Warnings: registry.List}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var response struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response.Warnings
}
