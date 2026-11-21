package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// gatedManager is the real session manager whose first result with words
// waits at gate before the manager takes it, which fixes the moment the
// socket's reader is inside the callback.
type gatedManager struct {
	*session.Manager
	entered chan struct{}
	gate    chan struct{}
	waited  bool
}

func (g *gatedManager) Message(mr *api.MessageResponse) error {
	_, err := g.MessageKept(mr)
	return err
}

// MessageKept is the path the transcript callback takes when the manager
// reports what its transcript keeps (keptMessager), so the gate sits here.
func (g *gatedManager) MessageKept(mr *api.MessageResponse) (*api.MessageResponse, error) {
	if !g.waited && len(mr.Channel.Alternatives) > 0 && mr.Channel.Alternatives[0].Transcript != "" {
		g.waited = true
		close(g.entered)
		<-g.gate
	}
	return g.Manager.MessageKept(mr)
}

// TestClosingLiveTranscriptionWhileItHandsOnAResultReturns fails if closing
// the live client (at shutdown, or the test-mode Deepgram fault) while its
// socket's reader hands a result with words to the session manager hangs:
// the client held its lock while the socket's close waited for the reader,
// and the reader waited for that lock to time the words (CapturedSince).
func TestClosingLiveTranscriptionWhileItHandsOnAResultReturns(t *testing.T) {
	peer, host := startListeningDeepgram(t)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recorder := audio.NewRecorder(t.TempDir())
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	gated := &gatedManager{Manager: manager, entered: make(chan struct{}), gate: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{SampleRate: 16000, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{manager: gated}, newWarningsRegistry(), nil)
	waitUntil(t, "the first socket", func() bool { return rc.IsConnected() && peer.latest() != nil })
	mic := recorder.Writer(rc)
	_, _ = mic.Write(reconnectSecond(1))
	_, _ = mic.Write(reconnectSecond(2))
	peer.latest().result(t, false, false, 0, 2, 0.5, "Good")
	<-gated.entered

	closed := make(chan struct{})
	go func() {
		_ = rc.Close()
		close(closed)
	}()
	time.Sleep(200 * time.Millisecond) // Close is under way while the reader is in the callback
	close(gated.gate)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the live client has not returned after 5 s: it waits for the socket's reader, which waits for the client")
	}
}
