package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
