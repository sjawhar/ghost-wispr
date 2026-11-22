package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestAStoppedGateOpeningSavesNothing fails if the gate's opening after a
// resume, cut off by the stop while it waits for the pause to settle or reads
// the light, goes on to refuse the resume, which saves the pause state: the
// stop must end it (the join), and the stop must return within its deadline.
// The second case is an opening nothing joins (no background, as before
// #138): its refusal comes after main closed the store, and the store refuses
// the write itself. From review 6005005351 of #138.
func TestAStoppedGateOpeningSavesNothing(t *testing.T) {
	for _, joined := range []bool{true, false} {
		name := "the stop cuts the opening off"
		if !joined {
			name = "an opening nothing joins: the closed store refuses its write"
		}
		t.Run(name, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "gate.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			recorder := audio.NewRecorder(t.TempDir())
			manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
			capture := newSessionCaptureGate(context.Background(), manager, recorder, func() deepgramFinalizer { return nil }, time.Hour, testEndRetry, nopLogf)
			if joined {
				capture.background = manager
			}
			opening := make(chan struct{})
			capture.beforeGateOpen = func() error {
				close(opening)
				time.Sleep(200 * time.Millisecond) // reading the light back
				return errors.New("the recording light does not show red")
			}
			refused := make(chan error, 1)
			capture.onResumeRefused = func(error) {
				refused <- store.SetAppState(storage.AppStateCapturePaused, "true")
			}
			capture.Pause()
			if err := capture.RequestResume(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-opening:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the resume did not reach the light check")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := manager.StopBackground(ctx); err != nil {
				t.Fatalf("the stop did not end the opening within its deadline: %v", err)
			}
			if !closeStore(ctx, store) {
				t.Fatal("closeStore left the store open")
			}
			select {
			case err := <-refused:
				if joined {
					t.Errorf("the opening the stop cut off refused the resume after the stop (its write: %v)", err)
				} else if !errors.Is(err, stopping.ErrStopped) {
					t.Errorf("the unjoined opening's write after the store closed = %v, want stopping.ErrStopped", err)
				}
			case <-time.After(time.Second):
				if !joined {
					t.Error("control: the unjoined opening did not refuse the resume")
				}
			}
		})
	}
}

// heldAppendStore holds the first AppendSegment, which a session's end makes
// as it flushes its buffered words, until release closes, and records what
// the store answered.
type heldAppendStore struct {
	*storage.SQLiteStore
	entered, release chan struct{}
	wrote            chan error
	held             bool
}

//nolint:gocritic // Store's append API passes one segment by value.
func (s *heldAppendStore) AppendSegment(sessionID string, seg transcribe.Segment) error {
	if s.held {
		return s.SQLiteStore.AppendSegment(sessionID, seg)
	}
	s.held = true
	close(s.entered)
	<-s.release
	err := s.SQLiteStore.AppendSegment(sessionID, seg)
	s.wrote <- err
	return err
}

// bufferedFinal is Deepgram's final result for text that does not end the
// speech: its words wait in the session's buffer.
func bufferedFinal(t *testing.T, text string) *api.MessageResponse {
	t.Helper()
	raw := fmt.Sprintf(`{"is_final": true, "speech_final": false, "channel": {"alternatives": [{
		"transcript": %q, "words": [{"speaker": 0, "punctuated_word": %q, "start": 0, "end": 1}]}]}}`, text, text)
	var msg api.MessageResponse
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("build message: %v", err)
	}
	return &msg
}

// TestAPausesSettlementIsWorkTheStopWaitsFor fails if the stop, as main runs
// it, returns while a pause's settlement still runs: its session's end
// flushes the buffered words to the store (AppendSegment), which must land
// before the stop returns (the join). When the flush outlasts the stop's
// deadline, main closes the store, and the store refuses the late flush
// itself (stopping.ErrStopped). The stop's own end waits for that flush and
// so does not end the session before its words are stored: the session stays
// active, for the next start to end (storage's RecoverStaleSessions). From
// review 6005005351 of #138.
func TestAPausesSettlementIsWorkTheStopWaitsFor(t *testing.T) {
	for _, withinDeadline := range []bool{true, false} {
		name := "the flush ends within the stop's deadline"
		if !withinDeadline {
			name = "the flush outlasts the deadline: the closed store refuses it"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settle.db")
			db := openResumeStore(t, path)
			store := &heldAppendStore{SQLiteStore: db, entered: make(chan struct{}), release: make(chan struct{}), wrote: make(chan error, 1)}
			recorder := audio.NewRecorder(t.TempDir())
			manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
			// As main's, the gate's context ends with the app (here, the test),
			// which stops a pause's end that keeps failing on the closed store.
			capture := newSessionCaptureGate(t.Context(), manager, recorder, func() deepgramFinalizer { return nil }, 10*time.Millisecond, testEndRetry, nopLogf)
			capture.background = manager
			id, err := manager.ManualStartSession(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.MessageKept(bufferedFinal(t, "words that wait in the buffer")); err != nil {
				t.Fatal(err)
			}
			capture.Pause()
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the pause's settlement did not flush the buffered words")
			}
			deadline := 400 * time.Millisecond
			if withinDeadline {
				// The flush is released at 100 ms; the deadline leaves a
				// loaded machine room to finish the stop after it.
				deadline = 2 * time.Second
				time.AfterFunc(100*time.Millisecond, func() { close(store.release) })
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			recoverAtStart(db).stop(ctx, manager, lifecyclePublishWait)
			if withinDeadline != (ctx.Err() == nil) {
				t.Fatalf("the stop's deadline expired: %v, want %t", ctx.Err(), !withinDeadline)
			}
			if !closeStore(ctx, db) {
				t.Fatal("closeStore left the store open")
			}
			if !withinDeadline {
				close(store.release)
			}
			select {
			case err := <-store.wrote:
				if withinDeadline && err != nil {
					t.Errorf("the flush the stop waited for = %v, want it written", err)
				}
				if !withinDeadline && !errors.Is(err, stopping.ErrStopped) {
					t.Errorf("the flush after the store closed = %v, want stopping.ErrStopped", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the settlement's flush did not end")
			}
			reopened := openResumeStore(t, path)
			segments, err := reopened.GetSegments(id)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(segments) == 1; got != withinDeadline {
				t.Errorf("%d segments saved, want the buffered words saved: %t", len(segments), withinDeadline)
			}
			sess, err := reopened.GetSession(id)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{true: storage.SessionEnded, false: storage.SessionActive}[withinDeadline]; sess.Status != want {
				t.Errorf("the session is %s after the stop, want %s", sess.Status, want)
			}
		})
	}
}

// heldReadLight is a light whose read back, once block is set, waits until
// release closes: an opening's red check is then under way, holding red.
type heldReadLight struct {
	*pluggableLight
	block            atomic.Bool
	once             sync.Once
	entered, release chan struct{}
}

func (l *heldReadLight) ReadRGB() (r, g, b uint8, err error) {
	if l.block.Load() {
		l.once.Do(func() { close(l.entered) })
		<-l.release
	}
	return l.pluggableLight.ReadRGB()
}

// TestAnOpeningTheStopOvertakesOpensNothing fails if a resume's gate opening
// opens the gate once the stop has begun, though its red check, under way at
// the stop, then reads red: the stop is the session manager's (the opening's
// context ends) or the gate's own (captureGate.Stop, as main's shutdown
// begins), during the check or during beforeResume. It also fails if an
// opening the stop overtook during its check runs beforeResume, or if an
// opening that opens nothing keeps the red hold its check took, so the light
// stays red with no microphone. The control, with no stop, opens the gate
// and ends the hold. From review 6005610488 of #138.
func TestAnOpeningTheStopOvertakesOpensNothing(t *testing.T) {
	for _, stop := range []string{"", "the manager's stop during the red check", "the gate's stop during the red check", "the gate's stop during beforeResume"} {
		name := stop
		if stop == "" {
			name = "control: no stop"
		}
		t.Run(name, func(t *testing.T) {
			store := openResumeStore(t, filepath.Join(t.TempDir(), "light.db"))
			manager := session.NewManager(store, audio.NewRecorder(t.TempDir()), nil, nil, session.NewDetector(time.Hour), 0)
			settling, releaseSettle := make(chan struct{}), make(chan struct{})
			resuming, releaseResume := make(chan struct{}), make(chan struct{})
			var resumed atomic.Bool
			capture := newCaptureGate(nil, func() { close(settling); <-releaseSettle }, func() {
				resumed.Store(true)
				if stop == "the gate's stop during beforeResume" {
					close(resuming)
					<-releaseResume
				}
			})
			capture.background = manager
			light := &heldReadLight{pluggableLight: &pluggableLight{present: true}, entered: make(chan struct{}), release: make(chan struct{})}
			hardware := &hardwareControl{light: &reconnectingLight{light: light, warnings: newWarningsRegistry()}}
			controls := newPersistentControls(store, capture.Pause, capture, nil)
			wirePauseControls(&server.ControlHooks{}, controls, capture, hardware, newWarningsRegistry())
			if err := controls.Pause(); err != nil {
				t.Fatal(err)
			}
			awaitClosed(t, settling, "the pause's settlement")
			if err := controls.Resume(); err != nil {
				t.Fatal(err)
			}
			light.block.Store(true)
			close(releaseSettle)
			awaitClosed(t, light.entered, "the opening's red check")
			switch stop {
			case "the manager's stop during the red check":
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				if err := manager.StopBackground(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("control: the stop = %v, want its deadline while the red check is held", err)
				}
			case "the gate's stop during the red check":
				capture.Stop()
			}
			close(light.release) // the check reads red
			if stop == "the gate's stop during beforeResume" {
				awaitClosed(t, resuming, "beforeResume")
				capture.Stop()
				close(releaseResume)
			}
			if stop == "" {
				// StopBackground cancels the opening it joins: the control
				// lets it open first.
				waitUntil(t, "the control's gate opening", func() bool { return !capture.isClosed() })
			}
			if err := manager.StopBackground(context.Background()); err != nil { // joins the opening
				t.Fatal(err)
			}
			if opened := !capture.isClosed(); opened != (stop == "") {
				t.Errorf("the gate opened: %t, want %t", opened, stop == "")
			}
			if overtakenInCheck := strings.HasSuffix(stop, "during the red check"); overtakenInCheck && resumed.Load() {
				t.Error("beforeResume ran for an opening the stop overtook during its red check")
			}
			if hardware.holdsRed() {
				t.Error("the opening's red hold outlived it")
			}
		})
	}
}

// TestAShutdownKeepsAnAcceptedResumesSavedState fails if a shutdown that
// begins while an accepted resume's gate opening waits for the pause to
// settle makes the opening refuse the resume: the refusal saves the paused
// state the unit came from over the accepted resume's, though its cause, the
// light's stop at shutdown ("recording light stopped"), is the shutdown's
// and not the light's. The saved state must stay the accepted resume's, so
// the next start restores recording, and confirms the light red before its
// microphone opens. It also fails if the gate opens once the app has
// stopped. The control, with no shutdown, opens the gate. From review
// 6005610488 of #138.
func TestAShutdownKeepsAnAcceptedResumesSavedState(t *testing.T) {
	for _, from := range []micState{micClosed, micListening} {
		for _, shutdown := range []string{"", "the app stops", "the app and the light stop"} {
			name := shutdown
			if shutdown == "" {
				name = "control: no shutdown"
			}
			t.Run(from.String()+"/"+name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "choice.db")
				store := openResumeStore(t, path)
				recorder := audio.NewRecorder(t.TempDir())
				manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
				appCtx, stopApp := newAppContext()
				defer stopApp()
				capture := newSessionCaptureGate(appCtx, manager, recorder, func() deepgramFinalizer { return nil }, 10*time.Millisecond, testEndRetry, nopLogf)
				settling, releaseSettle := make(chan struct{}), make(chan struct{})
				settle := capture.settle
				capture.settle = func() {
					close(settling)
					<-releaseSettle
					settle()
				}
				openings := &countedBackground{backgroundWork: manager}
				capture.background = openings
				hardware := &hardwareControl{light: &reconnectingLight{light: &pluggableLight{present: true}, warnings: newWarningsRegistry()}}
				controls := newPersistentControls(store, capture.Pause, capture, nil)
				if from == micListening {
					controls.useListener(capture, func() {}, func() bool { return true }, func() {})
				}
				wirePauseControls(&server.ControlHooks{}, controls, capture, hardware, newWarningsRegistry())
				var refusals atomic.Int32
				refuse := capture.onResumeRefused
				capture.onResumeRefused = func(err error) {
					refusals.Add(1)
					refuse(err)
				}
				if err := controls.Pause(); err != nil {
					t.Fatal(err)
				}
				awaitClosed(t, settling, "the pause's settlement")
				if capture.micState() != from {
					t.Fatalf("control: paused into %v, want %v", capture.micState(), from)
				}
				if err := controls.Resume(); err != nil {
					t.Fatal(err)
				}
				if value, _, err := store.GetAppState(storage.AppStateCapturePaused); err != nil || value != savedRecording {
					t.Fatalf("control: the accepted resume saved %q (%v), want %q", value, err, savedRecording)
				}
				switch shutdown {
				case "the app stops":
					stopApp()
				case "the app and the light stop":
					stopApp()
					hardware.Stop()
				}
				close(releaseSettle)
				// The opening ends before the manager's stop, which would
				// cancel it: the app's stop alone must keep it from opening
				// or refusing.
				waitUntil(t, "the gate's opening to end", openings.allEnded)
				if err := manager.StopBackground(context.Background()); err != nil { // joins the opening
					t.Fatal(err)
				}
				if opened := !capture.isClosed(); opened != (shutdown == "") {
					t.Errorf("the gate opened: %t, want %t", opened, shutdown == "")
				}
				if n := refusals.Load(); n != 0 {
					t.Errorf("the opening refused the accepted resume %d times", n)
				}
				value, _, err := store.GetAppState(storage.AppStateCapturePaused)
				if err != nil {
					t.Fatal(err)
				}
				if !closeStore(context.Background(), store) {
					t.Fatal("closeStore left the store open")
				}
				restored, err := restorePause(openResumeStore(t, path), newCaptureGate(nil, nil, nil))
				if err != nil || restored != micRecording {
					t.Errorf("after the shutdown the saved state is %q, and the next start restores %v (%v), want recording", value, restored, err)
				}
			})
		}
	}
}

// countedBackground is background work that counts the work it starts with
// Go, and the work that has ended.
type countedBackground struct {
	backgroundWork
	started, ended atomic.Int32
}

func (b *countedBackground) Go(f func(ctx context.Context)) {
	b.started.Add(1)
	b.backgroundWork.Go(func(ctx context.Context) {
		defer b.ended.Add(1)
		f(ctx)
	})
}

// allEnded reports whether work was started with Go, and all of it ended.
func (b *countedBackground) allEnded() bool {
	n := b.started.Load()
	return n > 0 && b.ended.Load() == n
}

// awaitClosed waits for ch to close, and fails the test if it does not
// within five seconds.
func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("control: %s did not come", what)
	}
}
