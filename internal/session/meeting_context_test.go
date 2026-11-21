package session

import (
	"context"
	"testing"
	"time"
)

// TestAMeetingContextEndsInTheStepThatClearsItsSession fails if a recording
// session's meeting context outlives the step that clears the session: it
// must be done when the end returns, while the session-ended hook still
// waits, for an automatic and a manual session alike; and it must be done at
// once for an id that is not the current session.
func TestAMeetingContextEndsInTheStepThatClearsItsSession(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	manager.SetSessionEndedHook(func(string) { <-release }) // the hook never returns during the test
	if ctx := manager.MeetingContext(""); ctx.Err() == nil {
		t.Fatal("the meeting context of no session is live")
	}

	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("start session: %v", err)
	}
	id := manager.currentSession()
	ctx := manager.MeetingContext(id)
	if ctx.Err() != nil {
		t.Fatal("control: the current session's meeting context is done")
	}
	if other := manager.MeetingContext(id + "0"); other.Err() == nil {
		t.Fatal("the meeting context of a session that is not current is live")
	}
	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the meeting context outlived the end of its session: it waits for the session-ended hook")
	}

	manual, err := manager.ManualStartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("manual start: %v", err)
	}
	manualCtx := manager.MeetingContext(manual)
	if manualCtx.Err() != nil {
		t.Fatal("control: the manual session's meeting context is done")
	}
	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("end the manual session: %v", err)
	}
	if manualCtx.Err() == nil {
		t.Fatal("the manual session's meeting context outlived its end")
	}
}

// contextRecorder is takenRecorder that reads, as each recording starts, the
// meeting context of the id it is asked to start.
type contextRecorder struct {
	takenRecorder
	manager *Manager
	seen    map[string]context.Context
}

func (r *contextRecorder) StartSession(sessionID string) error {
	r.seen[sessionID] = r.manager.MeetingContext(sessionID)
	return r.takenRecorder.StartSession(sessionID)
}

// TestAMeetingContextOfATakenIDEnds fails if the meeting context of an id a
// session start claimed, and gave up because a recording had it, stays live
// once the start moved to the next id; or if the next id's is done.
func TestAMeetingContextOfATakenIDEnds(t *testing.T) {
	store := openIDStore(t)
	started := time.Date(2026, 11, 6, 14, 9, 30, 0, time.UTC)
	const onDisk, free = "20261106140930", "20261106140931"
	recorder := &contextRecorder{takenRecorder: takenRecorder{taken: map[string]bool{onDisk: true}}, seen: map[string]context.Context{}}
	manager := NewManager(store, recorder, nil, nil, NewDetector(time.Hour), 0)
	recorder.manager = manager
	if err := manager.ensureSessionStarted(started); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if id, _ := manager.ActiveSession(); id != free {
		t.Fatalf("control: the session started as %q, want %q", id, free)
	}
	taken, ok := recorder.seen[onDisk]
	if !ok || recorder.seen[free] == nil {
		t.Fatalf("control: the recorder saw the starts of %v, want %s and %s", recorder.seen, onDisk, free)
	}
	if taken.Err() == nil {
		t.Fatalf("the meeting context of %s, which the start gave up, is live", onDisk)
	}
	if err := manager.MeetingContext(free).Err(); err != nil {
		t.Fatalf("the meeting context of %s, the session that started, is done: %v", free, err)
	}
}

// TestLeavingRecordingEndsTheMeetingAtOnce fails if the meeting context of
// the recording session outlives the moment capture pauses (PauseCapture, as
// every pause, light hold and close runs before the gate closes), and waits
// for the session's end, which follows only once Deepgram's last words are
// in. The next session after the resume gets a live meeting context: the
// control.
func TestLeavingRecordingEndsTheMeetingAtOnce(t *testing.T) {
	store := newStoreMock()
	manager := NewManager(store, nil, nil, nil, NewDetector(time.Hour), 0)
	if err := manager.ensureSessionStarted(time.Now().UTC()); err != nil {
		t.Fatalf("start session: %v", err)
	}
	id := manager.currentSession()
	meeting := manager.MeetingContext(id)
	if meeting.Err() != nil {
		t.Fatal("control: the recording session's meeting context is done")
	}
	manager.PauseCapture()
	if meeting.Err() == nil {
		t.Fatal("the meeting context was live when capture paused; only the session's later end would end it")
	}
	if again := manager.MeetingContext(id); again.Err() == nil {
		t.Fatal("the session that is ending gives a live meeting context while capture is paused")
	}
	if current, _ := manager.ActiveSession(); current != id {
		t.Fatalf("control: the pause cleared the session %q itself; its end does that", id)
	}
	if err := manager.endCurrentSession(context.Background()); err != nil {
		t.Fatalf("end session: %v", err)
	}
	manager.ResumeCapture()
	if err := manager.ensureSessionStarted(time.Now().UTC().Add(2 * time.Second)); err != nil {
		t.Fatalf("start the next session: %v", err)
	}
	if err := manager.MeetingContext(manager.currentSession()).Err(); err != nil {
		t.Fatalf("control: the session after the resume has a done meeting context: %v", err)
	}
}
