package session

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

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

// slowEndRecorder is the real recorder whose first EndSession, once the
// recording is closed, waits for release: the time the MP3 encode and the
// session row's write take while the session is still current.
type slowEndRecorder struct {
	*audio.Recorder
	once    sync.Once
	closed  chan struct{}
	release chan struct{}
}

func (s *slowEndRecorder) EndSession(id string) (string, error) {
	path, err := s.Recorder.EndSession(id)
	first := false
	s.once.Do(func() { first = true; close(s.closed) })
	if first {
		<-s.release
	}
	return path, err
}

// endingCapture is a capture whose recorder holds its first session end
// open after the recording closes, until the test releases it.
func endingCapture(t *testing.T) (*capture, *slowEndRecorder) {
	t.Helper()
	return endingCaptureWithTimeout(t, time.Hour)
}

// endingCaptureWithTimeout is endingCapture with a silence timeout.
func endingCaptureWithTimeout(t *testing.T, timeout time.Duration) (*capture, *slowEndRecorder) {
	t.Helper()
	dir := t.TempDir()
	recorder := audio.NewRecorder(dir)
	slow := &slowEndRecorder{Recorder: recorder, closed: make(chan struct{}), release: make(chan struct{})}
	store := newStoreMock()
	manager := NewManager(store, slow, nil, nil, NewDetector(timeout), 0)
	return &capture{t: t, dir: dir, store: store, recorder: recorder, manager: manager, mic: recorder.Writer(io.Discard)}, slow
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
	<-slow.closed                                                              // the first session's recording is closed; its encode runs
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

// nextSession waits up to wait for a current session other than first.
func (c *capture) nextSession(first string, wait time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if next, _ := c.manager.ActiveSession(); next != "" && next != first {
			return next
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("no session after %s opened within %v", first, wait)
	return ""
}

// endsWithin reports whether session id is no longer current within wait.
func (c *capture) endsWithin(id string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if current, _ := c.manager.ActiveSession(); current != id {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestASessionWordsOpenAfterAnEndEndsAfterSilence fails if the session the
// words spoken during a session end open has no silence timer. Their
// utterance end armed the timer during the end; it fired while the end still
// ran (an encode can take a minute), and its end gave up after 5 s, so the
// next session recorded the empty room until someone spoke again.
func TestASessionWordsOpenAfterAnEndEndsAfterSilence(t *testing.T) {
	timeout := 200 * time.Millisecond
	c, slow := endingCaptureWithTimeout(t, timeout)
	c.say(1, 2)
	c.hear(liveResult(t, true, true, "Bye."))
	first, _ := c.manager.ActiveSession()
	if err := c.manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatal(err)
	}
	<-slow.closed // the silence timer ended the first session; its encode runs
	c.say(10)
	c.hear(liveResult(t, true, true, "See", "you."))
	if err := c.manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatal(err)
	}
	// The timer fires during the end, and its end waits 5 s for the end
	// under way, then gives up.
	time.Sleep(6 * time.Second)
	close(slow.release)
	next := c.nextSession(first, 2*time.Second)
	if !c.endsWithin(next, 10*timeout) {
		t.Fatalf("session %s, opened by words spoken while %s ended, is still open after %v of silence with a %v silence timeout", next, first, 10*timeout, timeout)
	}
}

// TestASilenceTimerFromBeforeAnEndDoesNotEndTheNextSession fails if a silence
// timer that fired while a session was ending ends the next session the
// moment it opens: the timer was for silence before that session existed.
func TestASilenceTimerFromBeforeAnEndDoesNotEndTheNextSession(t *testing.T) {
	timeout := time.Second
	c, slow := endingCaptureWithTimeout(t, timeout)
	c.say(1, 2)
	c.hear(liveResult(t, true, true, "Bye."))
	first, _ := c.manager.ActiveSession()
	if err := c.manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatal(err)
	}
	<-slow.closed
	c.say(10)
	c.hear(liveResult(t, true, true, "See", "you."))
	if err := c.manager.UtteranceEnd(&api.UtteranceEndResponse{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(timeout + 500*time.Millisecond) // the timer fires; its end waits for the end under way
	close(slow.release)
	next := c.nextSession(first, 2*time.Second)
	if c.endsWithin(next, timeout/2) {
		t.Fatalf("session %s ended within %v of opening, before its own %v of silence: a timer from before it ended it", next, timeout/2, timeout)
	}
	if !c.endsWithin(next, 5*timeout) {
		t.Fatalf("session %s is still open after %v of silence with a %v silence timeout", next, 5*timeout, timeout)
	}
}
