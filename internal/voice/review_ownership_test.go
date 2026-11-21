package voice

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// reviewSpeechQueue delays delivery, not playback, preserving the output's
// start/end order. Waiting for queued counts observes actual speaker events.
type reviewSpeechQueue struct {
	*fakeSpeaker
	mu      sync.Mutex
	holding bool
	queue   []heldReport
	seen    chan audio.SpeechEvent
}

func (q *reviewSpeechQueue) OnSpeech(fn func(audio.SpeechEvent)) func() {
	return q.fakeSpeaker.OnSpeech(func(e audio.SpeechEvent) {
		if e.Input != audio.InputVoice {
			fn(e)
			return
		}
		q.mu.Lock()
		if q.holding {
			q.queue = append(q.queue, heldReport{fn: fn, e: e})
			q.mu.Unlock()
			q.seen <- e
			return
		}
		q.mu.Unlock()
		fn(e)
	})
}

func (q *reviewSpeechQueue) release() {
	q.mu.Lock()
	for len(q.queue) > 0 {
		item := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()
		item.fn(item.e)
		q.mu.Lock()
	}
	q.holding = false
	q.mu.Unlock()
}

func (q *reviewSpeechQueue) wait(t *testing.T, started bool) {
	t.Helper()
	select {
	case e := <-q.seen:
		if e.Started != started || e.Cancelled {
			t.Fatalf("speaker event %+v, want started=%v and not cancelled", e, started)
		}
	case <-time.After(testWait):
		t.Fatal("no queued voice event")
	}
}

func TestReviewBothSpeechReportsAfterStop(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%v", late), func(t *testing.T) {
			var q *reviewSpeechQueue
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				q = &reviewSpeechQueue{fakeSpeaker: r.speaker, holding: true, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			t.Cleanup(q.release)
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			sess.mu.Lock()
			sess.endLate = time.Second
			sess.mu.Unlock()
			speakAs(t, sess, "The issue is in review since this morning.")
			q.wait(t, true)
			r.speaker.finish(audio.InputVoice)
			q.wait(t, false)
			if !late {
				q.release()
				r.waitEvent("voice_spoken:sess-1")
			}
			r.s.Stop("review stop after completed audio")
			eventually(t, "the exchange closed", func() bool { _, _, closed := sess.state(); return closed })
			q.release()
			if late {
				r.waitEvent("voice_spoken:sess-1")
			}
		})
	}
}

func TestReviewSpeechInGapIsNotNextExchange(t *testing.T) {
	for _, heldStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("held_start=%v", heldStart), func(t *testing.T) {
			var q *reviewSpeechQueue
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				q = &reviewSpeechQueue{fakeSpeaker: r.speaker, holding: heldStart, seen: make(chan audio.SpeechEvent, 16)}
				cfg.Speaker = q
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			t.Cleanup(q.release)
			r.heard("Wispr, what is happening?", true, true)
			first := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			r.s.Stop("end first exchange before gap")
			eventually(t, "first exchange closed", func() bool { _, _, closed := first.state(); return closed })
			var pb Playback
			if err := r.s.WithFloor(t.Context(), func() { pb = r.speaker.Begin(audio.InputVoice, nil) }); err != nil {
				t.Fatal(err)
			}
			if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); err != nil {
				t.Fatal(err)
			}
			if heldStart {
				q.wait(t, true)
			} else {
				r.waitSpeaking()
			}
			r.speaker.finish(audio.InputVoice)
			if heldStart {
				q.wait(t, false)
			} else {
				eventually(t, "gap speech delivered", func() bool { r.s.mu.Lock(); defer r.s.mu.Unlock(); return !r.s.voiceSpeaking })
			}
			r.heard("Wispr, what happens next?", true, true)
			second := r.waitSession()
			r.waitEvent("voice_exchange_started:sess-1")
			q.release()
			r.s.mu.Lock()
			wrong := r.s.ex == nil || r.s.ex.notified[EventSpoken]
			r.s.mu.Unlock()
			if wrong {
				t.Fatalf("gap speech was attributed to the silent second exchange: %v", r.events.all())
			}
			// Positive control: the second exchange's own completed audio reports.
			speakAs(t, second, "The next review begins tomorrow.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			r.waitEvent("voice_spoken:sess-1")
			if n := spokenCount(r); n != 1 {
				t.Fatalf("got %d spoken events, want only the second exchange's", n)
			}
		})
	}
}

func TestReviewHeldEndAcrossTwoExchanges(t *testing.T) {
	var reports *heldReports
	r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
		reports = holdReports(t, cfg, r)
		cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
		cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
	})
	r.heard("Wispr, what is happening?", true, true)
	first := r.waitSession()
	r.waitEvent("voice_exchange_started:sess-1")
	r.s.mu.Lock()
	firstEx := r.s.ex
	r.s.mu.Unlock()
	speakAs(t, first, "The issue is in review since this morning.")
	r.waitSpeaking()
	r.speaker.finish(audio.InputVoice)
	waitFor(t, reports.held, "the first exchange's completed speech report")
	r.s.Stop("first exchange ends before report")
	eventually(t, "first exchange closed", func() bool { _, _, closed := first.state(); return closed })
	r.heard("Wispr, what happens next?", true, true)
	second := r.waitSession()
	r.waitEvent("voice_exchange_started:sess-1")
	r.s.mu.Lock()
	secondEx := r.s.ex
	r.s.mu.Unlock()
	speakAs(t, second, "The next review begins tomorrow.")
	r.speaker.finish(audio.InputVoice)
	// The second speech ends while the first speech's report remains held.
	eventually(t, "both later voice reports queued", func() bool {
		reports.mu.Lock()
		defer reports.mu.Unlock()
		n := 0
		for _, item := range reports.queue {
			if item.e.Input == audio.InputVoice {
				n++
			}
		}
		return n >= 3
	})
	reports.release()
	r.s.mu.Lock()
	firstSpoken, secondSpoken := firstEx.notified[EventSpoken], secondEx.notified[EventSpoken]
	r.s.mu.Unlock()
	if !firstSpoken || !secondSpoken {
		t.Fatalf("first spoken=%v, second spoken=%v", firstSpoken, secondSpoken)
	}
	eventually(t, "two distinct exchange reports", func() bool { return spokenCount(r) == 2 })
}
