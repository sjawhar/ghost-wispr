package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// liveTranscriptionConfig is the resilient Deepgram client's configuration
// for a microphone opened at sampleRate: with deepgram_buffer_size unset, its
// buffer holds transcribe.DefaultBufferSeconds at that rate.
func liveTranscriptionConfig(cfg *config.Config, sampleRate int) transcribe.ResilientConfig {
	return transcribe.ResilientConfig{
		BufferSize:            cfg.DeepgramBufferSize,
		SampleRate:            sampleRate,
		InitialReconnectDelay: cfg.ParsedDeepgramReconnectInitialDelay(),
		MaxReconnectBackoff:   cfg.ParsedDeepgramReconnectMaxBackoff(),
	}
}

// deepgramWarningKey is the "deepgram" status warning's key.
const deepgramWarningKey = "deepgram"

// deepgramNotConnected is the "deepgram" status warning.
const deepgramNotConnected = "Deepgram is not connected; live transcription keeps retrying, and no session starts by itself until it connects"

// liveManager is the session manager live transcription feeds: it takes the
// results (session.LifecycleManager), and the client is its live stream.
type liveManager interface {
	session.LifecycleManager
	SetLiveStream(stream session.LiveStream)
}

// newLiveTranscription makes the resilient Deepgram client a microphone start
// sends its audio to, and dials the first socket in the background: it returns
// at once, because the start holds the recording light red until it returns,
// and a dial can take the 15 s handshake limit. Until a socket is connected
// the client buffers the audio and redials with backoff
// (transcribe.ResilientClient.ConnectInBackground), as after any disconnect.
// The "deepgram" status warning says Deepgram is not connected whenever the
// client is not, before the first dial and after every later disconnect, and
// goes once a socket is connected and its buffered audio sent. Sessions start
// only from live transcripts, so without this one failed dial left the unit
// saving nothing until a restart. Each change of the client's state is
// broadcast on hub. The client is manager's live stream (SetLiveStream), so a
// session's recording starts before the first words Deepgram hears, even when
// it hears them late after a reconnect.
func newLiveTranscription(ctx context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, config transcribe.ResilientConfig, manager liveManager, warnings *warningsRegistry, hub *server.Hub) *transcribe.ResilientClient {
	var rc *transcribe.ResilientClient
	// syncWarning sets the warning from the client's state as it is now. The
	// client reports each change on a goroutine of its own, so reports can
	// arrive out of order; reading the state under one lock still leaves the
	// warning matching the last state.
	var warningMu sync.Mutex
	syncWarning := func() {
		warningMu.Lock()
		defer warningMu.Unlock()
		if rc.IsConnected() {
			warnings.Clear(deepgramWarningKey)
		} else {
			warnings.Set(deepgramWarningKey, deepgramNotConnected)
		}
	}
	rc = transcribe.NewResilientClient(ctx, nil, config, log.Printf, func(state transcribe.ConnectionState) {
		syncWarning()
		if hub != nil {
			hub.BroadcastComponentStatus("deepgram", state.String(), fmt.Sprintf("Deepgram %s", state.String()))
		}
	})
	rc.Factory = makeDeepgramClientFactory(ctx, apiKey, cOptions, tOptions, transcriptCallback{manager: manager}, deepgramWriteTimeout)
	if manager != nil {
		manager.SetLiveStream(rc)
	}
	// Set before the dial, so a dial that connects first clears it.
	warnings.Set(deepgramWarningKey, deepgramNotConnected)
	rc.ConnectInBackground()
	return rc
}
