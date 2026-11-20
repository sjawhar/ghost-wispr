package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestWordsFromBeforeAPauseAfterALongOutageDoNotReachTheNextSession fails if
// words spoken before a capture pause reach the live transcript of the
// session that words spoken after the resume open. After a long outage
// Deepgram works through the audio the live client buffered and lags the
// microphone, so the pause's Finalize finalizes only what Deepgram had
// reached, and its results for the words just before the pause come after
// the resume: they opened the next session, or went into it. The pause and
// the resume run as the capture gate runs them (settleCapturePause, then
// DiscardPreroll and ResumeCapture).
func TestWordsFromBeforeAPauseAfterALongOutageDoNotReachTheNextSession(t *testing.T) {
	peer, host := startListeningDeepgram(t)
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recorder := audio.NewRecorder(t.TempDir())
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rc := newLiveTranscription(ctx, "test-key",
		&interfaces.ClientOptions{Host: host},
		&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
		transcribe.ResilientConfig{SampleRate: 16000, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
		transcriptCallback{manager: manager}, newWarningsRegistry(), nil)
	t.Cleanup(func() { _ = rc.Close() })
	t.Cleanup(func() { _ = manager.ForceEndSession(context.Background()) })
	mic := recorder.Writer(rc)
	say := func(labels ...uint16) {
		t.Helper()
		for _, label := range labels {
			if _, err := mic.Write(reconnectSecond(label)); err != nil {
				t.Fatalf("capture second %d: %v", label, err)
			}
		}
	}

	waitUntil(t, "the first socket", func() bool { return rc.IsConnected() && peer.latest() != nil })
	first := peer.latest()
	say(1, 2)
	waitUntil(t, "the first socket to get its audio", func() bool { return first.streamSeconds() >= 2 })

	// A 40 s outage: the live client buffers the room's audio, and sends it
	// to the next socket once Deepgram is back.
	peer.refusing.Store(true)
	peer.dropAll()
	waitUntil(t, "the live client to see the socket gone", func() bool {
		say(3)
		return !rc.IsConnected()
	})
	for range 40 {
		say(4)
	}
	peer.refusing.Store(false)
	waitUntil(t, "a second socket", func() bool { return peer.latest() != first })
	second := peer.latest()

	say(10, 11, 12) // "Orange tractors climb.", spoken before the pause
	waitUntil(t, "the words before the pause to reach the second socket", func() bool {
		_, ok := second.heardAt(12)
		return ok
	})
	manager.PauseCapture()
	settleCapturePause(ctx, manager, rc, time.Second, testEndRetry, nopLogf)
	recorder.DiscardPreroll()
	manager.ResumeCapture()
	say(20, 21, 22, 23) // "Seven quiet violins play.", spoken after the resume
	waitUntil(t, "the words after the resume to reach the second socket", func() bool {
		_, ok := second.heardAt(23)
		return ok
	})

	// Deepgram, lagging, sends its results for both, timed in the second
	// socket's stream.
	before, _ := second.heardAt(10)
	after, _ := second.heardAt(20)
	second.result(t, true, true, before, 3, before, "Orange", "tractors", "climb.")
	second.result(t, true, true, after, 4, after, "Seven", "quiet", "violins", "play.")

	var text string
	waitUntil(t, "the words after the resume in a session", func() bool {
		id, _ := manager.ActiveSession()
		if id == "" {
			return false
		}
		segments, err := store.GetSegments(id)
		if err != nil {
			return false
		}
		var parts []string
		for _, segment := range segments {
			parts = append(parts, segment.Text)
		}
		text = strings.Join(parts, " ")
		return strings.Contains(text, "Seven")
	})
	if text != "Seven quiet violins play." {
		t.Fatalf("the session opened after the resume has the live transcript %q; want %q, nothing from before the pause", text, "Seven quiet violins play.")
	}
}
