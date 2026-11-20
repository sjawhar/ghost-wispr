package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// flakyDeepgram is the #65 round 3 review's peer: it answers the first
// websocket handshake with HTTP 503 and accepts the next, counting the audio
// frames it reads.
func flakyDeepgram(t *testing.T) (host string, handshakes, frames *atomic.Int64) {
	t.Helper()
	handshakes, frames = &atomic.Int64{}, &atomic.Int64{}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handshakes.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			kind, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				frames.Add(1)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws://" + srv.Listener.Addr().String(), handshakes, frames
}

// TestLiveTranscriptionConnectsAfATransientFirstDialFailure fails if one
// failed first dial to Deepgram turns live transcription off, as it did when
// the app dialled once: sessions start only from live transcripts, so the unit
// then saved nothing until a restart. Audio written meanwhile must reach the
// socket, and the status warning must go once it connects.
func TestLiveTranscriptionConnectsAfterATransientFirstDialFailure(t *testing.T) {
	host, handshakes, frames := flakyDeepgram(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warnings := newWarningsRegistry()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 200 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond},
		transcriptCallback{}, warnings, nil)
	t.Cleanup(func() { _ = rc.Close() })
	if rc.IsConnected() {
		t.Fatal("live transcription says connected after its first dial got HTTP 503")
	}
	if got := warnings.List(); !slices.ContainsFunc(got, func(w string) bool { return strings.Contains(w, "Deepgram is not connected") }) {
		t.Fatalf("warnings after a failed first dial = %q, want one saying Deepgram is not connected", got)
	}
	if _, err := rc.Write(make([]byte, 8000)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (!rc.IsConnected() || frames.Load() == 0) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rc.IsConnected() || frames.Load() == 0 {
		t.Fatalf("after one HTTP 503: connected=%t, handshakes=%d, audio frames at the peer=%d; want it connected and the audio sent", rc.IsConnected(), handshakes.Load(), frames.Load())
	}
	waitForHardware(t, "the Deepgram warning to clear", func() bool { return len(warnings.List()) == 0 })
}

// switchableDeepgram is a Deepgram live endpoint whose answer the test sets:
// accept and read, accept and close at once, or refuse with HTTP 503. It
// keeps the query of every handshake it is asked.
type switchableDeepgram struct {
	mode    atomic.Int32
	mu      sync.Mutex
	conns   []*websocket.Conn
	queries []url.Values
}

const (
	peerAccepts int32 = iota
	peerDropsAtOnce
)

func startSwitchableDeepgram(t *testing.T) (*switchableDeepgram, string) {
	t.Helper()
	peer := &switchableDeepgram{}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.mu.Lock()
		peer.queries = append(peer.queries, r.URL.Query())
		peer.mu.Unlock()
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if peer.mode.Load() == peerDropsAtOnce {
			_ = c.Close()
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
	return peer, "ws://" + srv.Listener.Addr().String()
}

// dropAll closes every connection the peer holds.
func (p *switchableDeepgram) dropAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// held is how many connections the peer holds.
func (p *switchableDeepgram) held() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// handshakes is the query of every handshake the peer was asked, in order.
func (p *switchableDeepgram) handshakes() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.queries)
}

func notConnectedWarning(warnings *warningsRegistry) bool {
	return slices.ContainsFunc(warnings.List(), func(w string) bool { return strings.Contains(w, "Deepgram is not connected") })
}

// TestDeepgramWarningFollowsLaterDisconnects fails if the "Deepgram is not
// connected" warning is shown only before the first dial: a socket that
// Deepgram drops later, or that dies right after a reconnect opens it, must
// show it until a socket is connected again, and then clear it.
func TestDeepgramWarningFollowsLaterDisconnects(t *testing.T) {
	peer, host := startSwitchableDeepgram(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warnings := newWarningsRegistry()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{}, warnings, nil)
	t.Cleanup(func() { _ = rc.Close() })
	// The first dial runs in the background.
	waitForHardwareWithin(t, 3*time.Second, "the first dial to connect and clear the warning", func() bool {
		return rc.IsConnected() && !notConnectedWarning(warnings)
	})

	// The microphone's audio, as the send queue writes it.
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

	// Deepgram drops the socket, and every reconnect's socket at once.
	peer.mode.Store(peerDropsAtOnce)
	peer.dropAll()
	waitForHardwareWithin(t, 3*time.Second, "the Deepgram warning after the socket is dropped", func() bool {
		return notConnectedWarning(warnings)
	})

	// Deepgram takes sockets again.
	peer.mode.Store(peerAccepts)
	waitForHardwareWithin(t, 5*time.Second, "Deepgram connected again with no warning", func() bool {
		return rc.IsConnected() && !notConnectedWarning(warnings)
	})
	time.Sleep(300 * time.Millisecond)
	if !rc.IsConnected() || notConnectedWarning(warnings) {
		t.Fatalf("300 ms after reconnecting: connected=%t, warnings %q; want connected and no warning", rc.IsConnected(), warnings.List())
	}
}

// hubDeepgramStatus is the "deepgram" status the hub hands a web client that
// connects now: the last one it broadcast.
func hubDeepgramStatus(hub *server.Hub) string {
	for _, event := range hub.SnapshotComponentStatuses() {
		if event.Component == "deepgram" {
			return event.Status
		}
	}
	return ""
}

// TestTheHubsDeepgramStatusEndsAtTheClientsState fails if the "deepgram"
// status the web clients hold is not the client's own state once it settles,
// after the first dial and after each reconnect. On the office Pi on
// 2026-11-20 the hub sent connected and then draining, so the page and the
// acceptance driver read "draining" for as long as Deepgram stayed connected.
func TestTheHubsDeepgramStatusEndsAtTheClientsState(t *testing.T) {
	peer, host := startSwitchableDeepgram(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := server.NewHub()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{}, newWarningsRegistry(), hub)
	t.Cleanup(func() { _ = rc.Close() })

	settled := func(what string) {
		t.Helper()
		waitForHardwareWithin(t, 5*time.Second, what, func() bool {
			return rc.IsConnected() && hubDeepgramStatus(hub) == "connected"
		})
		time.Sleep(100 * time.Millisecond)
		if got := hubDeepgramStatus(hub); !rc.IsConnected() || got != "connected" {
			t.Fatalf("%s: 100 ms later the client is connected=%t and the hub's deepgram status is %q, want connected", what, rc.IsConnected(), got)
		}
	}
	settled("the first dial: client connected and the hub's status connected")
	for round := 1; round <= 5; round++ {
		peer.dropAll()
		waitForHardwareWithin(t, 3*time.Second, "the client to notice the drop", func() bool {
			_, _ = rc.Write(make([]byte, 3200))
			return !rc.IsConnected()
		})
		settled("a reconnect: client connected and the hub's status connected")
	}
}

// blockingStatusStore is an event store that holds two Deepgram statuses
// while it stores them: the first "reconnecting", until release closes, so
// the hub's status callback runs behind the client's changes, as a slow disk
// makes it; and the first "connected", until the test ends, so a connected
// announced late stays visible to the test instead of passing in microseconds.
type blockingStatusStore struct {
	reconnecting, connected chan struct{}
	release, end            chan struct{}
	onceReconnecting        *sync.Once
	onceConnected           *sync.Once
}

func (blockingStatusStore) HasSession(string) (bool, error) { return true, nil }

func (s blockingStatusStore) StoreEvent(_ context.Context, _, payload string) error {
	var event struct{ Component, Status string }
	_ = json.Unmarshal([]byte(payload), &event)
	switch {
	case event.Component != "deepgram":
	case event.Status == "reconnecting":
		s.onceReconnecting.Do(func() { close(s.reconnecting); <-s.release })
	case event.Status == "connected":
		s.onceConnected.Do(func() { close(s.connected); <-s.end })
	}
	return nil
}

// stuckStatusStore holds every Deepgram status it is asked to store: until
// ctx ends if honours, else until end closes. entered closes at the first.
type stuckStatusStore struct {
	entered, end, cancelled chan struct{}
	honours                 bool
	once                    *sync.Once
}

func (stuckStatusStore) HasSession(string) (bool, error) { return true, nil }

func (s stuckStatusStore) StoreEvent(ctx context.Context, _, payload string) error {
	var event struct{ Component string }
	_ = json.Unmarshal([]byte(payload), &event)
	if event.Component != "deepgram" {
		return nil
	}
	s.once.Do(func() { close(s.entered) })
	if s.honours {
		select {
		case <-ctx.Done():
			close(s.cancelled)
			return ctx.Err()
		case <-s.end:
			return nil
		}
	}
	<-s.end
	return nil
}

// TestTheWarningChangesWithTheClientsStateAtOnce fails if the "Deepgram is
// not connected" warning waits for the status callback, which can run behind
// the client: a change heard of late left the warning wrong for as long as
// the callback was behind. The callback here is stuck in the hub's store for
// the whole test; the warning must still clear the moment the client reads
// connected, and come back the moment it reads not connected.
func TestTheWarningChangesWithTheClientsStateAtOnce(t *testing.T) {
	peer, host := startSwitchableDeepgram(t)
	store := stuckStatusStore{entered: make(chan struct{}), end: make(chan struct{}), cancelled: make(chan struct{}), once: &sync.Once{}}
	hub := server.NewHub()
	hub.SetEventStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warnings := newWarningsRegistry()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Hour, MaxReconnectBackoff: time.Hour},
		transcriptCallback{}, warnings, hub)
	t.Cleanup(func() { close(store.end); _ = rc.Close() })

	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the status callback never reached the store")
	}
	waitForHardwareWithin(t, 3*time.Second, "the client to connect", rc.IsConnected)
	if notConnectedWarning(warnings) {
		t.Fatal("the client reads connected, and the warning still says it is not: it waited for the stuck status callback")
	}
	peer.dropAll()
	for deadline := time.Now().Add(3 * time.Second); rc.IsConnected(); {
		if time.Now().After(deadline) {
			t.Fatal("the client did not notice the drop")
		}
		_, _ = rc.Write(make([]byte, 3200))
	}
	if !notConnectedWarning(warnings) {
		t.Fatal("the client reads not connected, and the warning is not shown: it waited for the stuck status callback")
	}
}

// TestCloseCancelsAStatusStoreItWaitsOn fails if Close leaves the status
// callback in the hub's store: the callback must pass its context down to the
// store, so a close ends a store that waits, at once, rather than leave it
// running after Close's bounded wait.
func TestCloseCancelsAStatusStoreItWaitsOn(t *testing.T) {
	_, host := startSwitchableDeepgram(t)
	store := stuckStatusStore{entered: make(chan struct{}), end: make(chan struct{}), cancelled: make(chan struct{}), honours: true, once: &sync.Once{}}
	defer close(store.end)
	hub := server.NewHub()
	hub.SetEventStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Hour, MaxReconnectBackoff: time.Hour},
		transcriptCallback{}, newWarningsRegistry(), hub)
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the status callback never reached the store")
	}
	start := time.Now()
	_ = rc.Close()
	took := time.Since(start)
	select {
	case <-store.cancelled:
	default:
		t.Fatalf("Close returned after %v and the store it waited on was never cancelled", took)
	}
	if took >= 500*time.Millisecond {
		t.Fatalf("Close took %v: it waited on the store instead of cancelling it", took)
	}
}

// firstStoreSignals is the real store, telling the test when its first store
// starts and when it returns.
type firstStoreSignals struct {
	*storage.SQLiteStore
	entered, returned chan struct{}
	once              *sync.Once
}

func (s firstStoreSignals) StoreEvent(ctx context.Context, eventType, payload string) error {
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if first {
		defer close(s.returned)
	}
	return s.SQLiteStore.StoreEvent(ctx, eventType, payload)
}

// TestCloseIsBoundedWhileAStoreWaitsOnAnotherConnectionsWriteLock fails if
// Close, or the status store it leaves running, outlasts its bound while
// another connection (the sqlite3 command, a backup) holds the real
// database's write lock. SQLite's busy handler does not heed the driver's
// interrupt, so the cancel does not end that store: Close returns within 1 s
// (stateCallbackStopWait), and the store ends, stored or failed, within
// busy_timeout (5 s) of its start, with the lock held throughout.
func TestCloseIsBoundedWhileAStoreWaitsOnAnotherConnectionsWriteLock(t *testing.T) {
	var storeStart time.Time
	_, host := startSwitchableDeepgram(t)
	path := filepath.Join(t.TempDir(), "busy.db")
	locker, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Close() }()
	writer, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	tx, err := locker.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO mcp_events(event_type, payload) VALUES('lock', '{}')`); err != nil {
		t.Fatal(err)
	}
	store := firstStoreSignals{SQLiteStore: writer, entered: make(chan struct{}), returned: make(chan struct{}), once: &sync.Once{}}
	hub := server.NewHub()
	hub.SetEventStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Hour, MaxReconnectBackoff: time.Hour},
		transcriptCallback{}, newWarningsRegistry(), hub)
	select {
	case <-store.entered:
		storeStart = time.Now()
	case <-time.After(3 * time.Second):
		t.Fatal("the status callback never reached the store")
	}
	select {
	case <-store.returned:
		t.Fatal("the store did not wait for the other connection's write lock")
	case <-time.After(100 * time.Millisecond):
	}
	start := time.Now()
	_ = rc.Close()
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("Close took %v while a store waited on the write lock; its bound is 1 s", took)
	}
	select {
	case <-store.returned:
		t.Logf("the store ended %v after it started", time.Since(storeStart))
	case <-time.After(time.Until(storeStart.Add(5*time.Second + time.Second))):
		t.Fatalf("the store still waited %v after it started, past busy_timeout (5 s)", time.Since(storeStart))
	}
}

// TestALateConnectedNeverHidesACurrentOutage fails if a change heard of late
// clears the "Deepgram is not connected" warning, or tells the pages
// connected, while Deepgram is down now. The status callback is held on the
// first dial's "reconnecting" while the socket connects and is dropped; once
// it runs again, it hears of "draining" and "connected" after the outage
// began. In #109's first version they cleared the warning and told the pages
// connected; on main the warning stayed but the pages were told connected.
func TestALateConnectedNeverHidesACurrentOutage(t *testing.T) {
	peer, host := startSwitchableDeepgram(t)
	store := blockingStatusStore{
		reconnecting: make(chan struct{}), connected: make(chan struct{}),
		release: make(chan struct{}), end: make(chan struct{}),
		onceReconnecting: &sync.Once{}, onceConnected: &sync.Once{},
	}
	var released sync.Once
	free := func() { released.Do(func() { close(store.release) }) }
	hub := server.NewHub()
	hub.SetEventStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warnings := newWarningsRegistry()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		// No reconnect during the test: the outage lasts.
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: time.Hour, MaxReconnectBackoff: time.Hour},
		transcriptCallback{}, warnings, hub)
	t.Cleanup(func() { free(); close(store.end); _ = rc.Close() })

	select {
	case <-store.reconnecting:
	case <-time.After(3 * time.Second):
		t.Fatal("the first dial's reconnecting status was not stored")
	}
	waitForHardwareWithin(t, 3*time.Second, "the socket to connect while the status callback is held", func() bool {
		return rc.IsConnected() && peer.held() == 1
	})
	peer.dropAll()
	waitForHardwareWithin(t, 3*time.Second, "the outage", func() bool {
		_, _ = rc.Write(make([]byte, 3200))
		return !rc.IsConnected()
	})
	free()

	// The held callback and every change behind it run now. A "connected"
	// announced from here on is one heard of late: the client stays down.
	select {
	case <-store.connected:
		t.Fatalf("during the outage (connected=%t) the pages were told Deepgram is connected; the warning is shown: %t",
			rc.IsConnected(), notConnectedWarning(warnings))
	case <-time.After(500 * time.Millisecond):
	}
	if rc.IsConnected() {
		t.Fatal("the client reconnected, though its reconnect delay is an hour")
	}
	if !notConnectedWarning(warnings) {
		t.Fatalf("during the outage the warning is gone (hub status %q): a change heard of late cleared it", hubDeepgramStatus(hub))
	}
	if status := hubDeepgramStatus(hub); status == "connected" || status == "" {
		t.Fatalf("during the outage the hub's deepgram status is %q", status)
	}
}

// TestEveryLiveSocketOptsOutOfDeepgramModelImprovement fails if a live
// socket, the first or one a reconnect dials, asks Deepgram without
// mip_opt_out=true. Without it, Deepgram's Model Improvement Program may keep
// the office audio and its transcripts after processing, for training, and a
// deleted segment would live on at Deepgram. It dials through
// newLiveTranscription, as main does.
func TestEveryLiveSocketOptsOutOfDeepgramModelImprovement(t *testing.T) {
	peer, host := startSwitchableDeepgram(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{}, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	// The peer keeps a connection only after its handshake reply, which can
	// reach the client first: wait for both, so the drop closes it.
	waitForHardwareWithin(t, 3*time.Second, "the first socket to connect", func() bool {
		return rc.IsConnected() && peer.held() == 1
	})

	// Deepgram drops the socket: the next write fails, and the client dials again.
	peer.dropAll()
	waitForHardwareWithin(t, 5*time.Second, "a reconnect's socket", func() bool {
		_, _ = rc.Write(make([]byte, 3200))
		return len(peer.handshakes()) >= 2 && rc.IsConnected()
	})
	for i, query := range peer.handshakes() {
		if got := query["mip_opt_out"]; !slices.Equal(got, []string{"true"}) {
			t.Fatalf("socket %d asked Deepgram with mip_opt_out %q (query %v), want [\"true\"]", i+1, got, query)
		}
	}
}

// deadSocket is a Deepgram socket the peer has closed.
type deadSocket struct{}

func (deadSocket) Write([]byte) (int, error) { return 0, errDeepgramSocketClosed }
func (deadSocket) Close() error              { return nil }

// liveSocket is a Deepgram socket that takes every write.
type liveSocket struct{}

func (liveSocket) Write(p []byte) (int, error) { return len(p), nil }
func (liveSocket) Close() error                { return nil }

// TestDeepgramOpenDuringAReconnectStartsNoSecondReconnect fails if a socket's
// Open, which the app-owned socket reports while it dials, marks the
// resilient client connected: during a reconnect that is before the new
// socket replaces the dead one, so the microphone's next write goes to the
// dead socket and starts a second reconnect beside the first, one of whose
// sockets is then never closed. It drives the Open callback newLiveTranscription
// gives its sockets, with the microphone's write in that window.
func TestDeepgramOpenDuringAReconnectStartsNoSecondReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dials atomic.Int32
	var rc *transcribe.ResilientClient
	callback := transcriptCallback{}
	factory := func(context.Context) (io.WriteCloser, error) {
		if dials.Add(1) == 1 {
			// As dialDeepgramLive reports the open socket before it returns,
			// with the microphone's next buffer in that window.
			_ = callback.Open(&api.OpenResponse{})
			_, _ = rc.Write([]byte("mic audio"))
		}
		return liveSocket{}, nil
	}
	rc = transcribe.NewResilientClient(ctx, factory, transcribe.ResilientConfig{BufferSize: 1 << 12, InitialReconnectDelay: 10 * time.Millisecond, MaxReconnectBackoff: 10 * time.Millisecond}, nil, nil)
	t.Cleanup(func() { _ = rc.Close() })
	rc.Client = deadSocket{}
	_, _ = rc.Write([]byte("audio the dead socket refuses")) // starts the reconnect

	waitForHardware(t, "the reconnect", rc.IsConnected)
	time.Sleep(200 * time.Millisecond)
	if got := dials.Load(); got != 1 {
		t.Fatalf("Deepgram dials = %d, want 1: the Open during the reconnect started a second reconnect", got)
	}
}
