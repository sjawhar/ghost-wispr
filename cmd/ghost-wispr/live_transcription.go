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
func newLiveTranscription(ctx context.Context, apiKey string, cOptions *interfaces.ClientOptions, optsFor func() *interfaces.LiveTranscriptionOptions, config transcribe.ResilientConfig, callback transcriptCallback, warnings *warningsRegistry, hub *server.Hub) *transcribe.ResilientClient {
	// The warning changes with the client's state, under the client's lock
	// at the change itself (OnStateChangeLocked): it is local and quick, so
	// it is never behind. The hub's "deepgram" status is broadcast from the
	// state callback, which can run behind the client; each call broadcasts
	// the client's current state, and only one newer than the last it
	// broadcast (by Seq), so the pages never get an older state after a newer
	// one and end on the latest. The callback's context ends at Close, which
	// ends a store the broadcast waits on.
	var (
		rc        *transcribe.ResilientClient
		onChange  transcribe.ConnectionCallback
		appliedMu sync.Mutex
		applied   uint64
	)
	if hub != nil {
		onChange = func(changeCtx context.Context, _ transcribe.StateChange) {
			appliedMu.Lock()
			defer appliedMu.Unlock()
			current := rc.CurrentState()
			if current.Seq <= applied {
				return
			}
			applied = current.Seq
			hub.BroadcastComponentStatusSeq(changeCtx, "deepgram", current.State.String(), fmt.Sprintf("Deepgram %s", current.State.String()), current.Seq)
		}
	}
	rc = transcribe.NewResilientClient(ctx, nil, config, log.Printf, onChange)
	rc.OnStateChangeLocked = func(change transcribe.StateChange) {
		if change.State == transcribe.StateConnected {
			warnings.Clear(deepgramWarningKey)
		} else {
			warnings.Set(deepgramWarningKey, deepgramNotConnected)
		}
	}
	// The voice times each word by when its audio was captured.
	callback.heardAt = rc.CapturedAt
	rc.Factory = makeDeepgramClientFactory(ctx, apiKey, cOptions, optsFor, callback, deepgramWriteTimeout)
	if manager, ok := callback.manager.(liveManager); ok && manager != nil {
		manager.SetLiveStream(rc)
	}
	// Set before the dial: the client reads connected until its first dial
	// starts, with no socket yet.
	warnings.Set(deepgramWarningKey, deepgramNotConnected)
	rc.ConnectInBackground()
	return rc
}
