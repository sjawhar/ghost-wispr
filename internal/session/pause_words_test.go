package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// pauseAndResume pauses and resumes capture as the capture gate does: the
// pause stops sessions starting, and the resume drops the audio the recorder
// kept, then lets sessions start again.
func (c *capture) pauseAndResume() {
	c.t.Helper()
	c.manager.PauseCapture()
	c.recorder.DiscardPreroll()
	c.manager.ResumeCapture()
}

// currentText returns the live transcript of the current session, which the
// test needs to exist.
func (c *capture) currentText() string {
	c.t.Helper()
	id, _ := c.manager.ActiveSession()
	if id == "" {
		c.t.Fatal("no session is open")
	}
	return strings.Join(c.textOf(id), " ")
}

// TestWordsFromBeforeAPauseDoNotReachTheNextSession fails if words captured
// before a capture pause, that Deepgram finalizes only after the resume, go
// into the live transcript of the session the resume's first words open. A
// pause must keep the words from before it out of every later session: the
// person who paused asked for that, and a session deleted before the pause
// must not have its last words come back in the next one. Deepgram finalizes
// late after an outage, and can put the last word before the pause in the
// same result as the first words after the resume.
func TestWordsFromBeforeAPauseDoNotReachTheNextSession(t *testing.T) {
	c := newCapture(t)
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2, 3) // "Orange tractors climb the muddy road.", 0-3 s into the stream
	c.pauseAndResume()
	c.say(10, 11, 12) // "Seven quiet violins.", 3-6 s into the stream
	c.hear(timedResult(t, true, false, 0, "Orange", "tractors", "climb"))
	c.hear(timedResult(t, true, true, 2*time.Second, "road.", "Seven", "quiet", "violins."))

	if got, want := c.currentText(), "Seven quiet violins."; got != want {
		t.Fatalf("the session opened after the resume has the live transcript %q; want %q, nothing from before the pause", got, want)
	}
}

// TestAResultFromBeforeAPauseWithNoWordTimesIsDropped fails if a final result
// Deepgram gives no word times, for audio from before a pause, reaches the
// session the words after the resume open: it is placed by the audio its
// result covers.
func TestAResultFromBeforeAPauseWithNoWordTimesIsDropped(t *testing.T) {
	c := newCapture(t)
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2, 3)
	c.pauseAndResume()
	c.say(10, 11)
	c.hear(buildMsg(t, `{"type":"Results","is_final":true,"speech_final":false,"start":0,"duration":3,"channel":{"alternatives":[{"transcript":"Orange tractors.","words":[]}]}}`))
	c.hear(timedResult(t, true, true, 3*time.Second, "Seven", "violins."))

	if got, want := c.currentText(), "Seven violins."; got != want {
		t.Fatalf("the session opened after the resume has the live transcript %q; want %q", got, want)
	}
}

// racingRecorder is the real recorder that pauses and resumes capture the
// first time the manager asks it to hold audio: the manager has placed the
// result's words by then, and has not yet buffered them.
type racingRecorder struct {
	*audio.Recorder
	manager *Manager
	once    sync.Once
}

func (r *racingRecorder) HoldPreroll(ago time.Duration) {
	r.once.Do(func() {
		r.manager.PauseCapture()
		r.Recorder.DiscardPreroll()
		r.manager.ResumeCapture()
	})
	r.Recorder.HoldPreroll(ago)
}

// TestWordsOfAResultThatRacesAResumeAreDropped fails if the words of a result
// the manager takes while capture pauses and resumes reach the session the
// resume's first words open: the manager placed them against the mark of an
// earlier resume, and the resume had already emptied the buffer they then
// went into. Their audio was all captured before the resume, so before the
// pause.
func TestWordsOfAResultThatRacesAResumeAreDropped(t *testing.T) {
	var racing *racingRecorder
	c := newCaptureOn(t, func(r *audio.Recorder) Recorder {
		racing = &racingRecorder{Recorder: r}
		return racing
	})
	racing.manager = c.manager
	c.manager.SetLiveStream(clockStream{c})
	c.say(1, 2, 3)
	c.hear(timedResult(t, true, false, 0, "Orange", "tractors", "climb."))
	c.say(10, 11)
	c.hear(timedResult(t, true, true, 3*time.Second, "Seven", "violins."))

	if got, want := c.currentText(), "Seven violins."; got != want {
		t.Fatalf("the session opened after the resume has the live transcript %q; want %q", got, want)
	}
}
