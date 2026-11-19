package tts

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestBeforePlayHoldsTheSpeechUntilTheFloorIsFree fails if POST
// /api/tts/speak plays while a voice exchange holds the floor: the request
// waits in its queue, then plays once BeforePlay lets it.
func TestBeforePlayHoldsTheSpeechUntilTheFloorIsFree(t *testing.T) {
	provider := &mockProvider{
		audio:  []byte{0x01, 0x00, 0x02, 0x00},
		format: audio.AudioFormat{SampleRate: 16000, Channels: 1, Encoding: audio.EncodingPCM16},
	}
	playEntered := make(chan struct{}, 1)
	releasePlay := make(chan struct{})
	close(releasePlay)
	speaker := newOutputOn(t, &blockingOutputStream{entered: playEntered, release: releasePlay})
	floor := make(chan struct{})
	waited := make(chan struct{}, 1)
	o := NewOrchestrator(provider, speaker, OrchestratorOpts{
		MaxRequestsPerMin: 100,
		BeforePlay: func(ctx context.Context) error {
			waited <- struct{}{}
			select {
			case <-floor:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	defer o.Stop()

	if _, err := o.Speak("an announcement", "", ""); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("the request never asked for the floor")
	}
	select {
	case <-playEntered:
		t.Fatal("the request played while an exchange held the floor")
	case <-time.After(100 * time.Millisecond):
	}
	close(floor)
	select {
	case <-playEntered:
	case <-time.After(time.Second):
		t.Fatal("the request did not play once the floor was free")
	}
}
