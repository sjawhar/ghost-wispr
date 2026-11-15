package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deepgramWarningKey is the "deepgram" status warning's key.
const deepgramWarningKey = "deepgram"

// deepgramNotConnected is the "deepgram" status warning.
const deepgramNotConnected = "Deepgram is not connected; live transcription keeps retrying, and no session starts by itself until it connects"

// newLiveTranscription makes the resilient Deepgram client a microphone start
// sends its audio to, and dials the first socket. A failed dial does not turn
// live transcription off: the client buffers audio and redials with backoff
// (transcribe.ResilientClient.Connect). The "deepgram" status warning says
// Deepgram is not connected whenever the client is not, before the first
// dial and after every later disconnect, and goes once a socket is connected
// and its buffered audio sent. Sessions start only from live transcripts, so
// without this one failed dial left the unit saving nothing until a restart.
// reportConnected broadcasts a first dial that worked; at boot the start-up
// status reports it instead.
func newLiveTranscription(ctx context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, config transcribe.ResilientConfig, manager session.LifecycleManager, warnings *warningsRegistry, hub *server.Hub, reportConnected bool) *transcribe.ResilientClient {
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
	// Set before the dial, so a reconnect that connects first clears it.
	warnings.Set(deepgramWarningKey, deepgramNotConnected)
	if err := rc.Connect(); err != nil {
		log.Printf("warning: deepgram did not connect, retrying in the background: %v", err)
		return rc
	}
	syncWarning()
	if reportConnected && hub != nil {
		hub.BroadcastComponentStatus("deepgram", storage.ComponentStatusConnected, "Deepgram connected")
	}
	return rc
}
