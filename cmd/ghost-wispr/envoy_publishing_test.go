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
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/secrets"
)

const testSigningSecret = "projects/example-project/secrets/example-signing-secret/versions/latest"

// secretManagerDenial is Secret Manager's answer to an identity that has no
// Secret Accessor role on the secret.
const secretManagerDenial = `{"error":{"code":403,"message":"Permission denied on secret","status":"PERMISSION_DENIED"}}`

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
			http.Error(w, secretManagerDenial, status)
			return
		}
		writeSecretVersion(w, r, key)
	}))
	t.Cleanup(func() { close(release); server.Close() })
	return server
}

// secretManagerToGrant answers Secret Manager's access call for
// testSigningSecret with 403 until grant is called and with key after it, as
// Secret Manager does when the unit's identity is given Secret Accessor on
// the secret. reads counts the access calls.
func secretManagerToGrant(t *testing.T, key string) (server *httptest.Server, grant func(), reads func() int32) {
	t.Helper()
	var granted atomic.Bool
	var calls atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !granted.Load() {
			http.Error(w, secretManagerDenial, http.StatusForbidden)
			return
		}
		writeSecretVersion(w, r, key)
	}))
	t.Cleanup(server.Close)
	return server, func() { granted.Store(true) }, calls.Load
}

// writeSecretVersion answers Secret Manager's access call for
// testSigningSecret with key, and any other path with 404.
func writeSecretVersion(w http.ResponseWriter, r *http.Request, key string) {
	if r.URL.Path != "/v1/"+testSigningSecret+":access" {
		http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"payload": map[string]any{
		"data":       base64.StdEncoding.EncodeToString([]byte(key)),
		"dataCrc32c": strconv.FormatInt(int64(crc32.Checksum([]byte(key), crc32.MakeTable(crc32.Castagnoli))), 10),
	}})
}

// envoyListener stands in for Envoy's webhook: as Envoy does
// (legion packages/envoy/internal/webhook/ghostwispr.go), it refuses a post
// without a delivery id or an event type with 400, and one not signed with key
// with 401. Like Envoy, which publishes a Ghost Wispr event under its
// X-GhostWispr-Delivery id and stores a repeat of it once (legion
// packages/envoy/internal/integration/webhook_redelivery_test.go), it counts
// each delivery once: Ghost Wispr sends an event at least once, and every post
// of one event carries the same delivery id.
type envoyListener struct {
	*httptest.Server
	mu         sync.Mutex
	posted     int
	deliveries map[string]bool
	hold, held chan struct{} // see holdFirstPost
}

func signedListener(t *testing.T, key string) *envoyListener {
	t.Helper()
	l := &envoyListener{deliveries: map[string]bool{}}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivery := strings.TrimSpace(r.Header.Get("X-GhostWispr-Delivery"))
		if delivery == "" || strings.TrimSpace(r.Header.Get("X-GhostWispr-Event")) == "" {
			http.Error(w, "missing ghostwispr headers", http.StatusBadRequest)
			return
		}
		mac := hmac.New(sha256.New, []byte(key))
		_, _ = mac.Write(body)
		if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-GhostWispr-Signature"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		l.mu.Lock()
		l.posted++
		first := l.posted == 1
		l.deliveries[delivery] = true
		hold, held := l.hold, l.held
		l.mu.Unlock()
		if first && hold != nil {
			close(held)
			<-hold
		}
	}))
	t.Cleanup(l.Close)
	return l
}

// accepted is the number of events the listener stored: one per delivery id.
func (l *envoyListener) accepted() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.deliveries)
}

// posts is the number of signed posts the listener answered.
func (l *envoyListener) posts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.posted
}

// holdFirstPost makes the listener hold the first signed post, unanswered,
// until release runs. arrived is closed once that post has come. Call it
// before anything posts.
func (l *envoyListener) holdFirstPost(t *testing.T) (arrived <-chan struct{}, release func()) {
	t.Helper()
	l.mu.Lock()
	l.hold, l.held = make(chan struct{}), make(chan struct{})
	hold, held := l.hold, l.held
	l.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release) // before the server's Close, which waits for the held post
	return held, release
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
	listener := signedListener(t, "key-in-secret-manager")
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
	if listener.accepted() != 1 {
		t.Fatalf("listener accepted %d events, want 1", listener.accepted())
	}
	if list := warnings.List(); len(list) != 0 {
		t.Fatalf("warnings = %q, want none", list)
	}
}

// TestASweepDuringAPublishSendsTheEventOnce fails if a session_started that a
// sweep posts while the direct publish of it is still posting reaches Envoy as
// two events. The event stays pending until its post returns, so a sweep in
// between, such as the one configure starts, posts it again. The control
// checks that the sweep did post it a second time.
func TestASweepDuringAPublishSendsTheEventOnce(t *testing.T) {
	listener := signedListener(t, "key-in-secret-manager")
	arrived, release := listener.holdFirstPost(t)
	publishing, _ := testEnvoyPublishing(t, fakeSecretManager(t, "key-in-secret-manager\n", http.StatusOK, false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishing.configure(ctx, envoyConfig(listener.URL), discardLogger)
	if err := publishing.store.CreateSession("session-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() { published <- publishing.current.Load().PublishSessionStarted(ctx, "session-1") }()
	select {
	case <-arrived:
	case <-time.After(testWait):
		t.Fatal("control: no session_started post came")
	}
	publishing.sweep(ctx) // the event is still pending: its first post has not returned
	release()
	if err := <-published; err != nil {
		t.Fatalf("PublishSessionStarted: %v", err)
	}
	if posts := listener.posts(); posts < 2 {
		t.Fatalf("control: the session_started was posted %d times, want a second post from the sweep", posts)
	}
	if got := listener.accepted(); got != 1 {
		t.Fatalf("listener accepted %d events, want 1", got)
	}
}

// TestTheEnvoyFakeRefusesAPostWithoutGhostWisprHeaders fails if the fake
// listener stores a signed post that lacks a delivery id or an event type,
// which Envoy refuses with 400: the fake would count an empty delivery as an
// event, so the Envoy tests would pass for a recorder that sent none. The
// control is the same signed post with both headers.
func TestTheEnvoyFakeRefusesAPostWithoutGhostWisprHeaders(t *testing.T) {
	const key = "key-in-secret-manager"
	body := []byte(`{"event_type":"session_started","payload":{"session_id":"session-1"}}`)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	for _, tc := range []struct {
		name, delivery, event string
		want                  int
	}{
		{name: "control, both headers", delivery: "session-1.session_started", event: "session.started", want: http.StatusOK},
		{name: "no delivery id", delivery: " ", event: "session.started", want: http.StatusBadRequest},
		{name: "no event type", delivery: "session-1.session_started", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := signedListener(t, key)
			request, err := http.NewRequest(http.MethodPost, listener.URL, strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-GhostWispr-Delivery", tc.delivery)
			request.Header.Set("X-GhostWispr-Event", tc.event)
			request.Header.Set("X-GhostWispr-Signature", signature)
			response, err := listener.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.want)
			}
			if wantStored := map[bool]int{true: 1, false: 0}[tc.want == http.StatusOK]; listener.accepted() != wantStored {
				t.Fatalf("listener stored %d events, want %d", listener.accepted(), wantStored)
			}
		})
	}
}

// TestEnvoyWithNoSigningSecretNameSendsNothing fails if a config that names no
// signing key secret builds a publisher: it would ask Secret Manager for a
// secret with no name. Envoy stays off, as with no webhook URL, and the
// startup warning names the missing setting.
func TestEnvoyWithNoSigningSecretNameSendsNothing(t *testing.T) {
	listener := signedListener(t, "unused")
	publishing, _ := testEnvoyPublishing(t, fakeSecretManager(t, "unused", http.StatusOK, false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := envoyConfig(listener.URL)
	cfg.EnvoyWebhookSecretName = ""
	publishing.configure(ctx, cfg, discardLogger)
	if publishing.current.Load() != nil {
		t.Fatal("a config with no envoy_webhook_secret_name built an Envoy publisher")
	}
}

// TestAnUnreadableEnvoyKeyWarnsAndHoldsNothingUp fails if a signing key the
// recorder cannot read is not shown in /api/status, or if waiting for it
// holds up the settings change or a session's start: recording must not
// depend on Envoy.
func TestAnUnreadableEnvoyKeyWarnsAndHoldsNothingUp(t *testing.T) {
	listener := signedListener(t, "unused")
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
	if listener.accepted() != 0 {
		t.Fatalf("listener accepted %d events with no key, want none", listener.accepted())
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

// TestAGrantedEnvoyKeyClearsItsWarningWithinOneSweep fails if a signing key
// that could not be read is read again only when an event is due: a unit with
// nothing to send would keep the /api/status warning after Secret Accessor is
// granted. Once the key reads, a sweep makes no Secret Manager call.
func TestAGrantedEnvoyKeyClearsItsWarningWithinOneSweep(t *testing.T) {
	listener := signedListener(t, "granted-key")
	secretManager, grant, reads := secretManagerToGrant(t, "granted-key")
	publishing, warnings := testEnvoyPublishing(t, secretManager)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishing.configure(ctx, envoyConfig(listener.URL), discardLogger)
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := warnings.List()
		if len(list) == 1 && strings.Contains(list[0], testSigningSecret) && strings.Contains(list[0], "403") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warnings = %q, want one naming the 403", list)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := reads(); got != 1 {
		t.Fatalf("Secret Manager reads at start = %d, want 1", got)
	}

	// Nothing is due: no session waits for an event, so no send reads the key.
	pendingLifecycle, err := publishing.store.PendingEnvoyLifecycleEvents()
	if err != nil {
		t.Fatal(err)
	}
	pendingSummaries, err := publishing.store.GetSessionsNeedingEnvoyPublish()
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingLifecycle) != 0 || len(pendingSummaries) != 0 {
		t.Fatalf("pending lifecycle events = %v, summaries = %v, want none", pendingLifecycle, pendingSummaries)
	}

	grant()
	publishing.sweep(ctx)
	if list := warnings.List(); len(list) != 0 {
		t.Fatalf("warnings one sweep after the grant = %q, want none", list)
	}
	if got := reads(); got != 2 {
		t.Fatalf("Secret Manager reads after the grant's sweep = %d, want 2", got)
	}
	publishing.sweep(ctx)
	if got := reads(); got != 2 {
		t.Fatalf("Secret Manager reads after a sweep with the key read = %d, want still 2", got)
	}
	if listener.accepted() != 0 {
		t.Fatalf("listener accepted %d events with none due, want none", listener.accepted())
	}
}
