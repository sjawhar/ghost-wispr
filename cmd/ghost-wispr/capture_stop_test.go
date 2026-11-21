package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
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
// itself (stopping.ErrStopped). From review 6005005351 of #138.
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
			capture := newSessionCaptureGate(context.Background(), manager, recorder, func() deepgramFinalizer { return nil }, 10*time.Millisecond, testEndRetry, nopLogf)
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
		})
	}
}
