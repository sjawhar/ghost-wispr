package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// fixedKey is a live-transcription key that never changes, for a test
// whose Deepgram takes any key.
func fixedKey(key string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return key, nil }
}

// keyedListen is Deepgram's /v1/listen socket as a test sees it: it keeps
// the Authorization header of each handshake and the sockets it holds, and
// drops them all on dropAll.
type keyedListen struct {
	mu    sync.Mutex
	auths []string
	conns []*websocket.Conn
}

func startKeyedListen(t *testing.T) (*keyedListen, string) {
	t.Helper()
	peer := &keyedListen{}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.mu.Lock()
		peer.auths = append(peer.auths, r.Header.Get("Authorization"))
		peer.mu.Unlock()
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		peer.mu.Lock()
		peer.conns = append(peer.conns, c)
		peer.mu.Unlock()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return peer, srv.URL
}

func (p *keyedListen) handshakes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.auths)
}

func (p *keyedListen) dropAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// TestAReconnectAfterAKeySaveUsesTheSavedKey fails if live transcription,
// once a Deepgram key is saved on the settings page (PATCH /api/config
// provider_keys, with main's config-change callback), reconnects with the
// key the unit started with, as GPT-6.1 Sol's review of 4e85fcf found: its
// reconnects took the start's key, so a corrected key read Works on the page
// while live transcription kept being refused until a restart.
func TestAReconnectAfterAKeySaveUsesTheSavedKey(t *testing.T) {
	const keyEnv = "EXAMPLE_LISTEN_KEY"
	peer, url := startKeyedListen(t)
	t.Setenv(keyEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "providers:\n  deepgram: {api: deepgram, url: %s, auth: {kind: api_key, env: %s}}\ntranscription: {model: deepgram/nova-3}\n", url, keyEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(keyEnv+"=dg-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgStore, _, err := loadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	unitProviders.follow(cfgStore)
	t.Cleanup(func() { unitProviders.follow(nil) })
	onConfigChange(cfgStore, func(cfg *config.Config) { unitProviders.of(cfg) })
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}

	cfg := cfgStore.Get()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rc := newLiveTranscription(ctx, liveTranscriptionCredential(cfgStore), liveTranscriptionClientOptions(&cfg), liveTranscriptionOptions(&cfg, 16000, nil),
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{}, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	waitUntil(t, "the first dial to connect", rc.IsConnected)
	if got := peer.handshakes(); len(got) != 1 || got[0] != "token dg-1" {
		t.Fatalf("the control, the first dial: handshakes %q; want one, with the key the unit started with", got)
	}

	patchConfig(t, h, `{"provider_keys":{"deepgram":"dg-2"}}`)
	// The microphone's audio, as the send queue writes it, so the client
	// sees the dropped socket and reconnects.
	stop := make(chan struct{})
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				_, _ = rc.Write(make([]byte, 3200))
			}
		}
	}()
	defer func() { close(stop); <-writing }()
	peer.dropAll()
	waitUntil(t, "a reconnect after the key save", func() bool { return len(peer.handshakes()) >= 2 && rc.IsConnected() })
	got := peer.handshakes()
	if last := got[len(got)-1]; last != "token dg-2" {
		t.Fatalf("the reconnect after the key save sent %q (every handshake: %q); want the saved key, token dg-2", last, got)
	}
}
