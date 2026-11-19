package session

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestAnEmptyFinalInsideTheFirstUtteranceKeepsTheHold fails if a final
// result with no words, Deepgram's answer to a pause inside the first
// utterance, releases the hold while that utterance's words wait to open a
// session: the recording would lose them again.
func TestAnEmptyFinalInsideTheFirstUtteranceKeepsTheHold(t *testing.T) {
	c := newCapture(t)
	c.say(1, 2)
	c.say(10)
	c.hear(liveResult(t, true, false, "Good", "morning,"))
	c.say(11, 12, 13, 14, 15, 16)
	c.hear(liveResult(t, true, false)) // a pause inside the utterance
	c.say(17, 18)
	c.hear(liveResult(t, true, true, "everyone."))

	got := c.recordedSeconds()
	utterance := []uint16{10, 11, 12, 13, 14, 15, 16, 17, 18}
	if len(got) < len(utterance) || !slices.Equal(got[len(got)-len(utterance):], utterance) {
		t.Fatalf("the recording holds seconds %v; want it to end with %v", got, utterance)
	}
}

// countingRecorder counts the holds the manager asks of the real recorder.
type countingRecorder struct {
	*audio.Recorder
	holds atomic.Int32
}

func (r *countingRecorder) HoldPreroll(ago time.Duration) {
	r.holds.Add(1)
	r.Recorder.HoldPreroll(ago)
}

// TestNoHoldWhileCaptureIsPaused fails if a result Deepgram sends during a
// pause, for audio from before it, holds audio for the next session.
func TestNoHoldWhileCaptureIsPaused(t *testing.T) {
	rec := &countingRecorder{Recorder: audio.NewRecorder(t.TempDir())}
	m := NewManager(newStoreMock(), rec, nil, nil, NewDetector(time.Hour), 0)
	_, _ = rec.Writer(io.Discard).Write(make([]byte, 32000))
	m.PauseCapture()
	if err := m.Message(liveResult(t, false, false, "late", "words")); err != nil {
		t.Fatal(err)
	}
	if n := rec.holds.Load(); n != 0 {
		t.Fatalf("the manager asked for %d holds while paused, want none", n)
	}
}

// TestNoHoldWhileASessionIsOpen fails if words heard during a session, whose
// audio is in its recording, hold audio for the next session.
func TestNoHoldWhileASessionIsOpen(t *testing.T) {
	rec := &countingRecorder{Recorder: audio.NewRecorder(t.TempDir())}
	m := NewManager(newStoreMock(), rec, nil, nil, NewDetector(time.Hour), 0)
	if _, err := m.ManualStartSession(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Message(liveResult(t, false, false, "in", "session")); err != nil {
		t.Fatal(err)
	}
	if n := rec.holds.Load(); n != 0 {
		t.Fatalf("the manager asked for %d holds during a session, want none", n)
	}
}

// slowEndRecorder is the real recorder whose EndSession, once the recording
// is closed, waits for release: the time the MP3 encode and the session row's
// write take while the session is still current.
type slowEndRecorder struct {
	*audio.Recorder
	closed  chan struct{}
	release chan struct{}
}

func (s *slowEndRecorder) EndSession(id string) (string, error) {
	path, err := s.Recorder.EndSession(id)
	close(s.closed)
	<-s.release
	return path, err
}

// endingCapture is a capture whose recorder holds each session end open
// after its recording closes, until the test releases it.
func endingCapture(t *testing.T) (*capture, *slowEndRecorder) {
	t.Helper()
	var slow *slowEndRecorder
	c := newCaptureOn(t, func(r *audio.Recorder) Recorder {
		slow = &slowEndRecorder{Recorder: r, closed: make(chan struct{}), release: make(chan struct{})}
		return slow
	})
	return c, slow
}

// TestWordsSpokenWhileASessionEndsOpenTheNextSession fails if words spoken
// after a session's recording closed, while its end (the MP3 encode and the
// database write, up to a minute or more) still runs, go into the ending
// session's live transcript, and so into no recording: the ending session's
// refined transcript drops them, and the next session's recording opened 5 s
// before its own first words. They open the next session, whose recording
// holds them from the first.
func TestWordsSpokenWhileASessionEndsOpenTheNextSession(t *testing.T) {
	c, slow := endingCapture(t)
	c.say(1, 2)
	c.hear(liveResult(t, true, true, "Bye."))
	first, _ := c.manager.ActiveSession()
	c.say(3, 4)
	ended := make(chan error, 1)
	go func() { ended <- c.manager.endCurrentSession(context.Background()) }() // the silence timer
	<-slow.closed // the first session's recording is closed; its encode runs
	c.say(10)
	c.hear(liveResult(t, false, false, "Good", "morning,"))
	c.say(11, 12, 13, 14, 15, 16, 17)
	c.hear(liveResult(t, true, true, "Good", "morning,", "everyone.", "This", "is", "the", "next", "meeting."))
	close(slow.release)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	next, _ := c.manager.ActiveSession()
	if next == "" || next == first {
		t.Fatalf("after the first session ended, the current session is %q; want a next one opened by the words that waited", next)
	}
	c.say(18, 19)
	c.hear(liveResult(t, true, true, "Next."))

	if got := c.textOf(first); !slices.Equal(got, []string{"Bye."}) {
		t.Fatalf("the ending session's live transcript is %q; want only %q, the words its recording holds", got, "Bye.")
	}
	if got := strings.Join(c.textOf(next), " "); !strings.HasPrefix(got, "Good morning,") {
		t.Fatalf("the next session's live transcript is %q; want it to begin %q", got, "Good morning,")
	}
	if got, want := c.secondsOf(next), []uint16{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}; !slices.Equal(got, want) {
		t.Fatalf("the next session's recording holds seconds %v; want %v, from its first words", got, want)
	}
}

// TestWordsCapturedBeforeTheRecordingClosedStayWithTheEndingSession fails if
// words Deepgram hears while a session ends, but that were spoken before its
// recording closed, leave that session: their audio is in its recording, not
// in the next one's. Words spoken after the close open the next session.
func TestWordsCapturedBeforeTheRecordingClosedStayWithTheEndingSession(t *testing.T) {
	c, slow := endingCapture(t)
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2)
	c.hear(timedResult(t, true, true, 0, "Bye."))
	first, _ := c.manager.ActiveSession()
	c.say(3) // "Last.", 2 s into the stream
	ended := make(chan error, 1)
	go func() { ended <- c.manager.endCurrentSession(context.Background()) }()
	<-slow.closed
	c.say(10, 11) // "Good morning,", 3 s into the stream
	c.hear(timedResult(t, true, false, 2*time.Second, "Last."))
	c.hear(timedResult(t, true, true, 3*time.Second, "Good", "morning,"))
	close(slow.release)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	next, _ := c.manager.ActiveSession()

	if got := c.textOf(first); !slices.Equal(got, []string{"Bye.", "Last."}) {
		t.Fatalf("the ending session's live transcript is %q; want %q: its recording holds both", got, []string{"Bye.", "Last."})
	}
	if next == "" || next == first {
		t.Fatalf("the current session is %q; want a next one, opened by the words spoken after the close", next)
	}
	if got := strings.Join(c.textOf(next), " "); got != "Good morning," {
		t.Fatalf("the next session's live transcript is %q; want %q", got, "Good morning,")
	}
	if got, want := c.secondsOf(next), []uint16{10, 11}; !slices.Equal(got, want) {
		t.Fatalf("the next session's recording holds seconds %v; want %v: nothing of the ending session's", got, want)
	}
}

// TestASessionStartsWhenItsRecordingDoes fails if a session's start time is
// not the time its recording starts: a refined segment's time is the
// session's start plus the segment's offset into the recording, so a
// recording that opens with audio from before the session started stamped
// every refined segment late by that much, up to the whole hold.
func TestASessionStartsWhenItsRecordingDoes(t *testing.T) {
	c := newCapture(t)
	c.say(1, 2)
	c.say(10)
	c.hear(liveResult(t, false, false, "Good", "morning,"))
	c.say(11, 12, 13, 14, 15, 16, 17, 18)
	before := time.Now().UTC()
	c.hear(liveResult(t, true, true, "Good", "morning,", "everyone."))
	after := time.Now().UTC()
	id, _ := c.manager.ActiveSession()
	lead := time.Duration(len(c.secondsOf(id))) * time.Second

	c.store.mu.Lock()
	started := c.store.sessions[id]
	c.store.mu.Unlock()
	if started.Before(before.Add(-lead-time.Second)) || started.After(after.Add(-lead+time.Second)) {
		t.Fatalf("the session started at %s; want its recording's start, %v before it began (%s)", started.Format(time.RFC3339Nano), lead, before.Format(time.RFC3339Nano))
	}
}
