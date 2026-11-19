package session

import (
	"context"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestMessageKeptReturnsNothingWhileCaptureIsPaused fails if, while capture
// is paused, MessageKept returns a result whose words no live transcript after
// the pause keeps: no session may start while paused, so they go at most to
// the session the pause ends, and the voice, which reads MessageKept, must not
// act on them.
func TestMessageKeptReturnsNothingWhileCaptureIsPaused(t *testing.T) {
	t.Run("a final after the pause ended the session", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2, 3, 4)
		c.hear(timedResult(t, true, true, 0, "Good", "morning."))
		first, _ := c.manager.ActiveSession()
		c.manager.PauseCapture()
		if err := c.manager.ForceEndSession(context.Background()); err != nil {
			t.Fatalf("the pause's end: %v", err)
		}
		kept, err := c.manager.MessageKept(timedResult(t, true, true, 2*time.Second, "Wispr,", "start", "a", "timer."))
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := c.manager.ActiveSession(); id != "" {
			t.Fatalf("session %s opened while paused", id)
		}
		if kept != nil {
			t.Fatalf("while paused, MessageKept returned %q, which no live transcript keeps (the ended session holds %q)", kept.Channel.Alternatives[0].Transcript, c.textOf(first))
		}
	})
	t.Run("an interim", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2, 3, 4)
		c.hear(timedResult(t, true, true, 0, "Good", "morning."))
		c.manager.PauseCapture()
		if err := c.manager.ForceEndSession(context.Background()); err != nil {
			t.Fatalf("the pause's end: %v", err)
		}
		kept, err := c.manager.MessageKept(timedResult(t, false, false, 2*time.Second, "Wispr,", "start"))
		if err != nil {
			t.Fatal(err)
		}
		if kept != nil {
			t.Fatalf("while paused, MessageKept returned the interim %q", kept.Channel.Alternatives[0].Transcript)
		}
	})
	t.Run("a final with no session ever open", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2, 3)
		c.manager.PauseCapture()
		kept, err := c.manager.MessageKept(timedResult(t, true, true, 0, "Wispr,", "what", "is", "new?"))
		if err != nil {
			t.Fatal(err)
		}
		c.store.mu.Lock()
		sessions := len(c.store.sessions)
		c.store.mu.Unlock()
		if sessions != 0 {
			t.Fatalf("%d sessions were made while paused", sessions)
		}
		if kept != nil {
			t.Fatalf("while paused, MessageKept returned %q; the live transcript kept none of it", kept.Channel.Alternatives[0].Transcript)
		}
	})
	t.Run("control: the same final while capturing", func(t *testing.T) {
		c := newCapture(t)
		c.manager.SetLiveStream(clockStream{c})
		c.say(1, 2, 3)
		kept, err := c.manager.MessageKept(timedResult(t, true, true, 0, "Wispr,", "what", "is", "new?"))
		if err != nil || kept == nil {
			t.Fatalf("MessageKept = %v, %v; want the result", kept, err)
		}
		if got, want := c.currentText(), kept.Channel.Alternatives[0].Transcript; got != want {
			t.Fatalf("the live transcript is %q; MessageKept returned %q", got, want)
		}
	})
}

// interimHub records each interim the manager broadcasts once after is set,
// as "session: text".
type interimHub struct {
	hubMock
	after atomic.Bool
	seen  sync.Mutex
	shown []string
}

func (h *interimHub) BroadcastLiveTranscriptInterim(sessionID string, speaker int, text string, startTime float64) {
	h.hubMock.BroadcastLiveTranscriptInterim(sessionID, speaker, text, startTime)
	if !h.after.Load() {
		return
	}
	h.seen.Lock()
	defer h.seen.Unlock()
	h.shown = append(h.shown, sessionID+": "+text)
}

func (h *interimHub) interimsAfter() []string {
	h.seen.Lock()
	defer h.seen.Unlock()
	return slices.Clone(h.shown)
}

// resumingRecorder is the real recorder that, the first time the manager asks
// it to hold audio, pauses and resumes capture as the capture gate does, then
// starts the next session, as a person's manual start just after the resume
// would: the manager has placed the result's words by then, and has neither
// shown nor buffered them.
type resumingRecorder struct {
	*audio.Recorder
	t       *testing.T
	manager *Manager
	hub     *interimHub
	once    sync.Once
	next    atomic.Value // the session started after the resume
}

func (r *resumingRecorder) HoldPreroll(ago time.Duration) {
	r.once.Do(func() {
		r.manager.PauseCapture()
		r.DiscardPreroll()
		r.manager.ResumeCapture()
		r.hub.after.Store(true)
		id, err := r.manager.ManualStartSession(context.Background(), "")
		if err != nil {
			r.t.Errorf("the session after the resume did not start: %v", err)
			return
		}
		r.next.Store(id)
	})
	r.Recorder.HoldPreroll(ago)
}

func resumingCapture(t *testing.T) (*capture, *resumingRecorder, *interimHub) {
	t.Helper()
	dir := t.TempDir()
	recorder := audio.NewRecorder(dir)
	hub := &interimHub{}
	resuming := &resumingRecorder{Recorder: recorder, t: t, hub: hub}
	store := newStoreMock()
	manager := NewManager(store, resuming, nil, hub, NewDetector(time.Hour), 0)
	resuming.manager = manager
	c := &capture{t: t, dir: dir, store: store, recorder: recorder, manager: manager, mic: recorder.Writer(io.Discard)}
	manager.SetLiveStream(clockStream{c})
	return c, resuming, hub
}

// TestAResultThatRacesAResumeIsNeitherShownNorReturned fails if a result
// whose words are all from before a pause, which the manager takes while
// capture pauses and resumes, is shown on the page after the resume or
// returned by MessageKept: an interim went out unchecked, under the session
// opened after the resume.
func TestAResultThatRacesAResumeIsNeitherShownNorReturned(t *testing.T) {
	for _, final := range []bool{false, true} {
		name := "an interim"
		if final {
			name = "control: a final"
		}
		t.Run(name, func(t *testing.T) {
			c, resuming, hub := resumingCapture(t)
			c.say(1, 2, 3) // "Orange tractors climb.", 0-3 s into the stream, before the pause
			kept, err := c.manager.MessageKept(timedResult(t, final, final, 0, "Orange", "tractors", "climb."))
			if err != nil {
				t.Fatal(err)
			}
			next, _ := resuming.next.Load().(string)
			if next == "" {
				t.Fatal("the pause, the resume and the next session did not run while the manager took the result")
			}
			if got := hub.interimsAfter(); len(got) != 0 {
				t.Errorf("after the resume the page was shown %q (session: text); every word is from before the pause", got)
			}
			if got := c.textOf(next); len(got) != 0 {
				t.Errorf("session %s, opened after the resume, has the live transcript %q", next, got)
			}
			if kept != nil {
				t.Errorf("MessageKept returned %q after the resume; every word is from before the pause", kept.Channel.Alternatives[0].Transcript)
			}
		})
	}
}

// TestAResultWithoutWordTimesThatStartsBeforeAResumeIsDropped fails if a
// final with no word times whose audio starts before a pause and ends after
// the resume reaches the session the resume's words open: without word times
// its words cannot be split at the pause, so the whole result is dropped.
func TestAResultWithoutWordTimesThatStartsBeforeAResumeIsDropped(t *testing.T) {
	c := newCapture(t)
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2, 3) // "Orange tractors climb.", 0-3 s into the stream
	c.pauseAndResume()
	c.say(10, 11, 12, 13, 14) // "Seven quiet violins play.", 3-8 s
	c.hear(buildMsg(t, `{"type":"Results","is_final":true,"speech_final":true,"start":2,"duration":6,"channel":{"alternatives":[{"transcript":"climb. Seven quiet violins play.","words":[]}]}}`))

	if id, _ := c.manager.ActiveSession(); id != "" {
		t.Fatalf("session %s opened after the resume with the live transcript %q; %q is from before the pause", id, c.currentText(), "climb.")
	}
}

// TestNoInterimIsShownWhileCaptureIsPaused fails if the manager shows the
// page words from before a pause while capture is paused: the page kept the
// last interim through the resume, as words of the meeting after it.
func TestNoInterimIsShownWhileCaptureIsPaused(t *testing.T) {
	for _, paused := range []bool{true, false} {
		name := "paused"
		if !paused {
			name = "control: capturing"
		}
		t.Run(name, func(t *testing.T) {
			hub := &hubMock{}
			m := NewManager(newStoreMock(), audio.NewRecorder(t.TempDir()), nil, hub, NewDetector(time.Hour), 0)
			if paused {
				m.PauseCapture()
			}
			if _, err := m.MessageKept(liveResult(t, false, false, "Orange", "tractors")); err != nil {
				t.Fatal(err)
			}
			if _, err := m.MessageKept(liveResult(t, true, false, "Orange", "tractors", "climb.")); err != nil {
				t.Fatal(err)
			}
			hub.mu.Lock()
			n := hub.interimCount
			hub.mu.Unlock()
			if want := map[bool]int{true: 0, false: 2}[paused]; n != want {
				t.Fatalf("the manager showed %d interims, want %d", n, want)
			}
		})
	}
}
