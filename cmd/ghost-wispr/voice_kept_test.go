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

// plainManager is the real session manager with MessageKept hidden, as it
// was before #84: the control that the test below can see a late result reach
// the voice.
type plainManager struct{ session.LifecycleManager }

// TestALateWakeFromBeforeAPauseDoesNotReachTheVoice fails if a wake said
// before a capture pause, whose result Deepgram sends only after the resume
// (the pause settled when its wait for the Finalize reply ran out), reaches
// the voice through the real session manager; the words said after the
// resume must reach it. The control, a manager with no MessageKept, lets the
// late wake through, so the check can fail.
func TestALateWakeFromBeforeAPauseDoesNotReachTheVoice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hideKept bool
	}{
		{"the real session manager", false},
		{"control: the manager without MessageKept", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer, host := startListeningDeepgram(t)
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			recorder := audio.NewRecorder(t.TempDir())
			manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
			heard := &heardTranscripts{}
			callback := transcriptCallback{manager: manager, voice: heard, stamp: func() (int, bool) { return 1, true }}
			if tc.hideKept {
				callback.manager = plainManager{manager}
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			rc := newLiveTranscription(ctx, "test-key",
				&interfaces.ClientOptions{Host: host},
				&interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1},
				transcribe.ResilientConfig{SampleRate: 16000, InitialReconnectDelay: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond},
				callback, newWarningsRegistry(), nil)
			if tc.hideKept {
				manager.SetLiveStream(rc) // as for the real manager, which the callback sets
			}
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

			waitUntil(t, "a socket", func() bool { return rc.IsConnected() && peer.latest() != nil })
			socket := peer.latest()
			say(10, 11, 12, 13) // "Wispr, what is happening?", said before the pause
			waitUntil(t, "the words before the pause to reach the socket", func() bool {
				_, ok := socket.heardAt(13)
				return ok
			})
			manager.PauseCapture()
			settleCapturePause(ctx, manager, rc, 200*time.Millisecond, testEndRetry, nopLogf)
			recorder.DiscardPreroll()
			manager.ResumeCapture()
			say(20, 21, 22, 23) // "Seven quiet violins play.", said after the resume
			waitUntil(t, "the words after the resume to reach the socket", func() bool {
				_, ok := socket.heardAt(23)
				return ok
			})

			// Deepgram sends its result for the wake only now, after the resume.
			before, _ := socket.heardAt(10)
			after, _ := socket.heardAt(20)
			socket.result(t, true, true, before, 4, before, "Wispr,", "what", "is", "happening?")
			socket.result(t, true, true, after, 4, after, "Seven", "quiet", "violins", "play.")

			reached := func(word string) bool {
				heard.mu.Lock()
				defer heard.mu.Unlock()
				for _, got := range heard.got {
					if strings.Contains(got.Text, word) {
						return true
					}
				}
				return false
			}
			waitUntil(t, "the words after the resume at the voice", func() bool { return reached("Seven") })
			if tc.hideKept {
				if !reached("Wispr") {
					t.Fatal("the control's voice did not read the late wake, so the check cannot fail")
				}
				return
			}
			if reached("Wispr") {
				t.Fatal("the late wake from before the pause reached the voice after the resume")
			}
		})
	}
}
