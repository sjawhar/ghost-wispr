package main

import (
	"context"
	"math"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestTheVoiceGetsWhenEachWordWasCaptured fails if the words of a live result
// reach the voice without the time their audio was captured, as the live
// stream times it, or with a time that moves with the result's arrival. The
// voice's barge-in counts only words said after its speech started, and
// Deepgram can send the question's own result seconds late (smart formatting
// holds a final that ends in a spoken number), after the voice began to
// answer it.
func TestTheVoiceGetsWhenEachWordWasCaptured(t *testing.T) {
	peer, host := startListeningDeepgram(t)
	heard := &heardTranscripts{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rc := newLiveTranscription(ctx, fixedKey("test-key"),
		&interfaces.ClientOptions{Host: host},
		constOpts(&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}),
		transcribe.ResilientConfig{SampleRate: 16000, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{voice: heard, stamp: func() (int, bool) { return 1, true }}, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	waitUntil(t, "a socket", func() bool { return rc.IsConnected() && peer.latest() != nil })
	socket := peer.latest()
	// The microphone's buffers reach the live client through the send queue,
	// as startMicMonitoring wires them.
	mic := transcribe.NewAsyncSender(ctx, rc, deepgramSendChunks, nopLogf)
	var lo, hi [2]time.Time // when each second's buffer was handed on
	for i, label := range []uint16{10, 11} {
		lo[i] = time.Now()
		_, _ = mic.Write(reconnectSecond(label))
		hi[i] = time.Now()
		time.Sleep(100 * time.Millisecond)
	}
	waitUntil(t, "both seconds at the socket", func() bool { return socket.streamSeconds() >= 2 })
	time.Sleep(300 * time.Millisecond) // the result comes late
	first, _ := socket.heardAt(10)
	socket.result(t, true, true, first, 2, first, "What's", "new?") // a word at the start of each second
	// A word at 1.001 s, which JSON decodes to a little under 1.001: it is
	// timed at 1.001 s, not truncated a sample early.
	socket.result(t, true, true, first+1.001, 0.5, first+1.001, "Again.")

	waitUntil(t, "the results at the voice", func() bool {
		heard.mu.Lock()
		defer heard.mu.Unlock()
		return len(heard.got) == 2
	})
	heard.mu.Lock()
	defer heard.mu.Unlock()
	words := heard.got[0].Words
	if len(words) != 2 {
		t.Fatalf("the voice got words %+v, want 2", words)
	}
	exact := time.Duration(math.Round((first + 1.001) * float64(time.Second)))
	if want, ok := rc.CapturedAt(exact); !ok || len(heard.got[1].Words) != 1 || !heard.got[1].Words[0].At.Equal(want) {
		t.Errorf("the word at %.3f s captured at %+v, want %v", first+1.001, heard.got[1].Words, want.Format(time.StampMicro))
	}
	// Each word starts its second's buffer: 1 s of audio before the time
	// that buffer was captured.
	for i := range words {
		if at := words[i].At; at.Before(lo[i].Add(-time.Second)) || at.After(hi[i].Add(-time.Second)) {
			t.Errorf("word %q captured at %v, want from %v to %v: 1 s before its buffer was captured", words[i].Text, at.Format(time.StampMicro), lo[i].Add(-time.Second).Format(time.StampMicro), hi[i].Add(-time.Second).Format(time.StampMicro))
		}
	}
}
