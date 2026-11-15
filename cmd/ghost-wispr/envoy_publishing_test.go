package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/secrets"
)

const testSigningSecret = "projects/theorem-agent-infra/secrets/envoy-ghostwispr-signing-secret/versions/latest"

// fakeSecretManager answers Secret Manager's access call for
// testSigningSecret with key, or with status when it is not 200; block, when
// set, holds every call until the test ends.
func fakeSecretManager(t *testing.T, key string, status int, block bool) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		if status != http.StatusOK {
			http.Error(w, `{"error":{"code":403,"message":"Permission denied on secret","status":"PERMISSION_DENIED"}}`, status)
			return
		}
		if r.URL.Path != "/v1/"+testSigningSecret+":access" {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"payload": map[string]any{
			"data":       base64.StdEncoding.EncodeToString([]byte(key)),
			"dataCrc32c": strconv.FormatInt(int64(crc32.Checksum([]byte(key), crc32.MakeTable(crc32.Castagnoli))), 10),
		}})
	}))
	t.Cleanup(func() { close(release); server.Close() })
	return server
}

// signedListener stands in for Envoy's webhook: it accepts a post signed
// with key and refuses any other with 401.
func signedListener(t *testing.T, key string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	accepted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(key))
		_, _ = mac.Write(body)
		if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-GhostWispr-Signature"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		accepted++
		mu.Unlock()
	}))
	t.Cleanup(server.Close)
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return accepted
	}
}

func testEnvoyPublishing(t *testing.T, secretManager *httptest.Server) (*envoyPublishing, *warningsRegistry) {
	t.Helper()
	store, manager := storeSessions(t)
	warnings := newWarningsRegistry()
	return &envoyPublishing{
		store:    store,
		manager:  manager,
		warnings: warnings,
		openSecrets: func(ctx context.Context, _ *config.Config) (*secrets.Reader, error) {
			return secrets.NewReader(ctx, option.WithEndpoint(secretManager.URL), option.WithoutAuthentication(), option.WithHTTPClient(secretManager.Client()))
		},
	}, warnings
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func envoyConfig(listenerURL string) *config.Config {
	return &config.Config{EnvoyEnabled: true, EnvoyWebhookURL: listenerURL, EnvoyWebhookSecretName: testSigningSecret}
}

// TestEnvoyEventsAreSignedWithTheKeyInSecretManager fails if the recorder
// signs with anything but the key Secret Manager holds for it.
func TestEnvoyEventsAreSignedWithTheKeyInSecretManager(t *testing.T) {
	listener, accepted := signedListener(t, "key-in-secret-manager")
	publishing, warnings := testEnvoyPublishing(t, fakeSecretManager(t, "key-in-secret-manager\n", http.StatusOK, false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishing.configure(ctx, envoyConfig(listener.URL), discardLogger)
	if err := publishing.store.CreateSession("session-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := publishing.current.Load().PublishSessionStarted(ctx, "session-1"); err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	if accepted() != 1 {
		t.Fatalf("listener accepted %d events, want 1", accepted())
	}
	if list := warnings.List(); len(list) != 0 {
		t.Fatalf("warnings = %q, want none", list)
	}
}

// TestAnUnreadableEnvoyKeyWarnsAndHoldsNothingUp fails if a signing key the
// recorder cannot read is not shown in /api/status, or if waiting for it
// holds up the settings change or a session's start: recording must not
// depend on Envoy.
func TestAnUnreadableEnvoyKeyWarnsAndHoldsNothingUp(t *testing.T) {
	listener, accepted := signedListener(t, "unused")
	publishing, warnings := testEnvoyPublishing(t, fakeSecretManager(t, "", http.StatusForbidden, false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishing.configure(ctx, envoyConfig(listener.URL), discardLogger)
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := warnings.List()
		if len(list) == 1 && strings.Contains(list[0], testSigningSecret) && strings.Contains(list[0], "not sent") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warnings = %q, want one naming the unreadable signing key", list)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A Secret Manager that does not answer: the settings change and a
	// session's start return at once, and nothing is sent.
	hung, hungWarnings := testEnvoyPublishing(t, fakeSecretManager(t, "", http.StatusOK, true))
	started := time.Now()
	hung.configure(ctx, envoyConfig(listener.URL), discardLogger)
	if err := hung.manager.Message(speechFinal(t, "words while the key read hangs")); err != nil {
		t.Fatalf("Message: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("configuring Envoy and starting a session took %s while the key read hung, want well under a second", elapsed)
	}
	if accepted() != 0 {
		t.Fatalf("listener accepted %d events with no key, want none", accepted())
	}
	if list := hungWarnings.List(); len(list) != 0 {
		t.Fatalf("warnings while the read is still waiting = %q, want none yet", list)
	}

	// Envoy turned off: its warning goes.
	publishing.configure(ctx, &config.Config{}, discardLogger)
	if list := warnings.List(); len(list) != 0 {
		t.Fatalf("warnings after turning Envoy off = %q, want none", list)
	}
}
