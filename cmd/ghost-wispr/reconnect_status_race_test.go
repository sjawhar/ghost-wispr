package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
)

// stepMic is a scripted microphone that records, at each open, the light's
// operations as the open began and ended (requireRed), and keeps the light
// rig's microphone state.
type stepMic struct {
	*scriptedMic
	r      *lightRig
	opened chan openSteps
}

func (m *stepMic) open(reconnect bool) error {
	begin := m.r.device.snapshot()
	m.r.mic.open.Store(true)
	m.opened <- openSteps{begin: begin, end: m.r.device.snapshot()}
	if reconnect {
		return m.scriptedMic.Reopen()
	}
	return m.scriptedMic.Open()
}

func (m *stepMic) Open() error   { return m.open(false) }
func (m *stepMic) Reopen() error { return m.open(true) }
func (m *stepMic) Close() error {
	m.r.mic.open.Store(false)
	return m.scriptedMic.Close()
}

// heldReconnectStatus is an event store whose store of the mic loop's
// "Attempting to reconnect microphone..." status waits until release closes,
// having closed entered: the status store is a database write, which can be
// slow.
type heldReconnectStatus struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*heldReconnectStatus) HasSession(string) (bool, error) { return true, nil }

func (s *heldReconnectStatus) StoreEvent(_ context.Context, _, payload string) error {
	var event struct{ Component, Message string }
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	if event.Component == "mic" && event.Message == "Attempting to reconnect microphone..." {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return nil
}

// TestAPauseWhileTheReconnectReportsItsStatusLeavesTheOpenToTheResume fails
// if a pause and a resume that both come while the mic loop stores its
// "reconnecting" status, after its restart wait passed with no pause, let the
// reconnect open the microphone: the resume's own open never runs, and its
// "resuming" state stays set until its limit, though audio flows under red
// (Sol's review of #136 at 56e1543, reproduced 50 of 50 on both error paths).
// The resume must own the open, under its own red confirmation, and end its
// state. The control, no pause, reconnects as before.
func TestAPauseWhileTheReconnectReportsItsStatusLeavesTheOpenToTheResume(t *testing.T) {
	for _, failure := range []string{"Input overflowed", "device disconnected"} {
		for _, pause := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pause %v", failure, pause), func(t *testing.T) {
				r := startLightRig(t, false)
				mic := &stepMic{scriptedMic: newScriptedMic(), r: r, opened: make(chan openSteps, 8)}
				mic.streamErrs = []error{errors.New(failure)}
				if err := r.capture.openUnderRed(func() error { r.mic.open.Store(true); return nil }); err != nil {
					t.Fatal(err)
				}
				store := &heldReconnectStatus{entered: make(chan struct{}), release: make(chan struct{})}
				release := sync.OnceFunc(func() { close(store.release) })
				t.Cleanup(release)
				hub := server.NewHub()
				hub.SetEventStore(store)
				recorded := &lockedBuffer{}
				ctx, cancel := context.WithCancel(context.Background())
				done := goDone(func() {
					streamMicWithRetry(ctx, r.capture, mic, recorded, func(time.Duration) {}, nopLogf, hub)
				})
				t.Cleanup(func() {
					cancel()
					close(mic.chunks)
					if !returnedWithin(done, testWait) {
						t.Error("the mic loop did not stop")
					}
				})

				mic.expectEvents(t, "stream", "close") // the error, then the restart wait
				select {
				case <-store.entered: // the reconnect stores its status
				case <-time.After(testWait):
					t.Fatal("the reconnect did not store its status")
				}
				if pause {
					r.capture.Pause()
					if err := r.capture.RequestResume(); err != nil {
						t.Fatal(err)
					}
					waitForHardwareWithin(t, testWait, "the resume to open the gate", func() bool { return !r.capture.isClosed() })
				}
				release()

				want := "reopen"
				if pause {
					want = "open" // the resume's own
				}
				for {
					var got string
					select {
					case got = <-mic.events:
					case <-time.After(testWait):
						t.Fatalf("no device call after the status, want %q", want)
					}
					if got == "close" && pause {
						continue // the reconnect's refusal closes the closed microphone again
					}
					if got != want {
						t.Fatalf("device call %q, want %q", got, want)
					}
					break
				}
				select {
				case steps := <-mic.opened:
					requireRed(t, want, steps)
				case <-time.After(testWait):
					t.Fatal("no open recorded")
				}
				mic.expectEvents(t, "stream")
				mic.capture(t, []byte{1, 2, 3, 4})
				if n := recorded.Len(); n != 4 {
					t.Fatalf("recorded %d bytes under confirmed red, want 4", n)
				}
				waitForHardwareWithin(t, testWait, "the resume's state to end", func() bool { return r.capture.MicHold() == "" })
			})
		}
	}
}
