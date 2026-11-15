package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"

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
	rc := newLiveTranscription(ctx, "test-key",
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 200 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond},
		nil, warnings, nil, false)
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
// accept and read, accept and close at once, or refuse with HTTP 503.
type switchableDeepgram struct {
	mode  atomic.Int32
	mu    sync.Mutex
	conns []*websocket.Conn
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
	rc := newLiveTranscription(ctx, "test-key",
		&interfaces.ClientOptions{Host: host, EnableKeepAlive: true},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{BufferSize: 1 << 16, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		nil, warnings, nil, false)
	t.Cleanup(func() { _ = rc.Close() })
	if !rc.IsConnected() || notConnectedWarning(warnings) {
		t.Fatalf("after a first dial that worked: connected=%t, warnings %q", rc.IsConnected(), warnings.List())
	}

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
