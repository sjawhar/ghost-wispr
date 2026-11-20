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
// broadcast on hub. callback hands each result to the session manager and
// the voice. The client is the manager's live stream (SetLiveStream) when the
// manager takes one, so a session's recording starts before the first words
// Deepgram hears, even when it hears them late after a reconnect.
func newLiveTranscription(ctx context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, config transcribe.ResilientConfig, callback transcriptCallback, warnings *warningsRegistry, hub *server.Hub) *transcribe.ResilientClient {
	// The client hands each change of its state to this callback in order,
	// but a change can be heard of after newer ones happened: the callback
	// may run behind. The warning and the hub's "deepgram" status show the
	// state as it is, so each call applies the client's current state, and
	// only if it is newer than the one last applied (by its Seq): a change
	// heard of late never clears the warning of a newer outage, nor tells the
	// pages connected during one.
	var (
		rc        *transcribe.ResilientClient
		appliedMu sync.Mutex
		applied   uint64
	)
	rc = transcribe.NewResilientClient(ctx, nil, config, log.Printf, func(context.Context, transcribe.StateChange) {
		appliedMu.Lock()
		defer appliedMu.Unlock()
		current := rc.CurrentState()
		if current.Seq <= applied {
			return
		}
		applied = current.Seq
		if current.State == transcribe.StateConnected {
			warnings.Clear(deepgramWarningKey)
		} else {
			warnings.Set(deepgramWarningKey, deepgramNotConnected)
		}
		if hub != nil {
			hub.BroadcastComponentStatusSeq("deepgram", current.State.String(), fmt.Sprintf("Deepgram %s", current.State.String()), current.Seq)
		}
	})
	rc.Factory = makeDeepgramClientFactory(ctx, apiKey, cOptions, tOptions, callback, deepgramWriteTimeout)
	if manager, ok := callback.manager.(liveManager); ok && manager != nil {
		manager.SetLiveStream(rc)
	}
	// Set before the dial, so a dial that connects first clears it.
	warnings.Set(deepgramWarningKey, deepgramNotConnected)
	rc.ConnectInBackground()
	return rc
}
