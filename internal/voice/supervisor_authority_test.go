package voice

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// The tests here hold each effect to the authority it acts for, read at the
// moment of the effect and with no lock: an exchange to the recorder's
// meeting once it has one, a notification sound to the answers or hands it
// sounds for, and a hand's spoken outcome to its membership.

// endedContext is a context that is done from the start.
func endedContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestAWakeBeforeTheFirstFinalWaitsForItsMeeting fails if a wake heard before
// the recorder has a session (Deepgram's first final opens it) is bound to the
// ended meeting of no session and ends at once, as "Wispr, ..." spoken as a
// meeting's first words would; or if the exchange, once it takes the session
// the final opened, does not end the moment the recorder ends that meeting.
func TestAWakeBeforeTheFirstFinalWaitsForItsMeeting(t *testing.T) {
	meeting, end := context.WithCancel(context.Background())
	defer end()
	c := newFakeConcierge("It is in review.")
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
		r.session = ""
		cfg.Meeting = func(id string) context.Context {
			if id == "sess-1" {
				return meeting
			}
			return endedContext()
		}
	})
	r.heard("Wispr, what is left on the release?", false, false)
	exchange := func() *exchange {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex
	}
	never(t, "a wake heard before the first final ended at once", 200*time.Millisecond, func() bool {
		ex := exchange()
		return ex == nil || ex.ended || ex.ctx.Err() != nil
	})
	r.mu.Lock()
	r.session = "sess-1" // the first final opened the recording session
	r.mu.Unlock()
	r.heard("Wispr, what is left on the release?", true, true)
	r.waitTurn()
	ex := exchange()
	if ex == nil || ex.ctx.Err() != nil {
		t.Fatal("control: the exchange did not survive to its turn")
	}
	end()
	if ex.ctx.Err() == nil {
		t.Fatal("the exchange's context was live after the recorder ended the meeting it took")
	}
	eventually(t, "the exchange ended with its meeting", func() bool { return exchange() == nil })
}

// TestTheRecorderEndStopsAVoiceFrameWaitingForRoom fails if a frame of an
// exchange's speech, written while the speaker's queue has no room, is
// committed once the recorder ended the meeting during the wait, while the
// Supervisor's lock is held (so no callback that needs it can run); a live
// meeting's frame is the control.
func TestTheRecorderEndStopsAVoiceFrameWaitingForRoom(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "the meeting records on", true: "the recorder ends the meeting"}[ended], func(t *testing.T) {
			meeting, end := context.WithCancel(context.Background())
			defer end()
			under := &waitingPlayback{entered: make(chan struct{}), release: make(chan struct{})}
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
				cfg.Meeting = func(string) context.Context { return meeting }
				cfg.Speaker = &voiceBlockingSpeaker{fakeSpeaker: r.speaker, voice: under}
			})
			r.heard("Wispr, tell me.", false, false)
			pb := r.waitSession().x.Out.Begin()
			done := make(chan error, 1)
			go func() { done <- pb.Write(context.Background(), []byte{1, 0}, ChimeFormat) }()
			<-under.entered
			r.s.mu.Lock()
			if ended {
				end()
			}
			close(under.release)
			err := <-done
			r.s.mu.Unlock()
			switch {
			case !ended && (err != nil || !under.committed.Load()):
				t.Fatalf("control: a live meeting's frame was refused: %v", err)
			case ended && under.committed.Load():
				t.Fatal("a frame was committed after the recorder ended its meeting while it waited for room")
			}
		})
	}
}

// voiceBlockingSpeaker is the fake speaker with every voice playback replaced
// by one playback that waits for room.
type voiceBlockingSpeaker struct {
	*fakeSpeaker
	voice Playback
}

func (s *voiceBlockingSpeaker) Begin(input string) Playback {
	if input == audio.InputVoice {
		return s.voice
	}
	return s.fakeSpeaker.Begin(input)
}

// TestAHandSoundEndsWithItsLastMembership fails if a hand sound, chosen and
// waiting to begin, plays a frame once the membership of the hand it is for
// ended, before anything lowered the hand (the Supervisor's lock is held, so
// no cleanup can run); a live membership's sound is the control.
func TestAHandSoundEndsWithItsLastMembership(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "the membership lives", true: "the membership ended"}[ended], func(t *testing.T) {
			r, _, _ := newHandsWaitingRig(t)
			release := r.speaker.holdBegins(audio.InputChime)
			var once sync.Once
			unblock := func() { once.Do(release) }
			defer unblock()
			membership, end := context.WithCancel(context.Background())
			defer end()
			h := testHand("h1", "ABC-12", time.Minute)
			h.Context = membership
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the hand sound chosen", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.chimeCancel != nil && r.s.chimeHands
			})
			r.s.mu.Lock()
			if ended {
				end()
			}
			unblock()
			deadline := time.Now().Add(150 * time.Millisecond)
			for r.speaker.written(audio.InputChime) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			played := slices.Contains(r.speaker.soundsPlayed(), "hand")
			r.s.mu.Unlock()
			switch {
			case !ended && !played:
				t.Fatal("control: a live hand's sound played nothing")
			case ended && played:
				t.Fatal("the hand sound played after the membership of its only hand ended")
			}
		})
	}
}

// TestASessionsEndCutsItsAnswerSound fails if an answer sound, chosen and
// waiting to begin, plays once SessionEnded dropped the answer it was for.
func TestASessionsEndCutsItsAnswerSound(t *testing.T) {
	r, light := newWaitingRig(t, newFakeConcierge())
	release := r.speaker.holdBegins(audio.InputChime)
	var once sync.Once
	unblock := func() { once.Do(release) }
	defer unblock()
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound chosen", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeCancel != nil && !r.s.chimeHands
	})
	r.s.SessionEnded("sess-1")
	unblock()
	never(t, "the answer sound played after its answer's session ended", 200*time.Millisecond, func() bool {
		return slices.Contains(r.speaker.soundsPlayed(), "answer")
	})
}

// TestTheLastHandsEndCutsItsQueuedSound fails if a hand sound whose frames are
// all queued stays in the speaker's queue once its last hand came down; a hand
// still up keeps it.
func TestTheLastHandsEndCutsItsQueuedSound(t *testing.T) {
	for _, lowered := range []bool{false, true} {
		t.Run(map[bool]string{false: "the hand still up", true: "the last hand lowered"}[lowered], func(t *testing.T) {
			r, _, _ := newHandsWaitingRig(t)
			h := testHand("h1", "ABC-12", time.Minute)
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the hand sound queued", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.chimeCancel == nil && r.speaker.written(audio.InputChime) > 0
			})
			r.speaker.mu.Lock()
			gen := r.speaker.chimes[0].gen
			r.speaker.mu.Unlock()
			if lowered {
				r.s.LowerHands(h.Membership)
			}
			r.speaker.mu.Lock()
			queued := r.speaker.gen[audio.InputChime] == gen
			r.speaker.mu.Unlock()
			switch {
			case !lowered && !queued:
				t.Fatal("control: a hand still up lost its queued sound")
			case lowered && queued:
				t.Fatal("the hand sound stayed in the speaker's queue after its last hand came down")
			}
		})
	}
}

// TestNeverMindHoldsAnAnswerSoundAlreadyChosen fails if an answer sound chosen
// before "Wispr, never mind", and waiting to begin, plays while the never
// mind still waits for the Concierge; the answer it may drop must not sound
// first. A hand raised meanwhile still sounds.
func TestNeverMindHoldsAnAnswerSoundAlreadyChosen(t *testing.T) {
	c := newFakeConcierge()
	c.working = 1
	c.neverMindHold = make(chan struct{})
	var decided sync.Once
	decide := func() { decided.Do(func() { close(c.neverMindHold) }) }
	defer decide()
	r, light := newWaitingRig(t, c)
	release := r.speaker.holdBegins(audio.InputChime)
	var once sync.Once
	unblock := func() { once.Do(release) }
	defer unblock()
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound chosen", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeCancel != nil && !r.s.chimeHands
	})
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "the never mind asked the Concierge", func() bool { return len(c.neverMindCalls()) == 1 })
	if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
		t.Fatal(err)
	}
	unblock()
	eventually(t, "control: the hand sound while the never mind waits", func() bool {
		return slices.Contains(r.speaker.soundsPlayed(), "hand")
	})
	r.s.mu.Lock()
	waiting := r.s.neverMinds > 0
	r.s.mu.Unlock()
	if !waiting {
		t.Fatal("UNKNOWN: the never mind was decided before the check")
	}
	if slices.Contains(r.speaker.soundsPlayed(), "answer") {
		t.Fatal("an answer sound chosen before the never mind played while it waited")
	}
}

// TestACutNotificationSoundsAgain fails if a notification sound cut before it
// played out (by an exchange taking the floor, whether its producer still ran
// or had queued its last frame) is taken as heard: its hand, still up, must
// sound again at the next gap.
func TestACutNotificationSoundsAgain(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "cut before its first frame", true: "cut once queued"}[queued], func(t *testing.T) {
			r, _, _ := newHandsWaitingRig(t)
			var release func()
			if !queued {
				release = r.speaker.holdBegins(audio.InputChime)
			}
			if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the hand sound chosen or queued", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				if queued {
					return r.s.chimeCancel == nil && r.speaker.written(audio.InputChime) > 0
				}
				return r.s.chimeCancel != nil && r.s.chimeHands
			})
			// A wake takes the floor and cuts the chime input; the exchange ends
			// with no turn, so the floor is free again.
			r.s.mu.Lock()
			ex := &exchange{ctx: context.Background(), cancel: func() {}, opened: make(chan struct{}), notified: map[string]bool{}, times: &exchangeTimes{}}
			r.s.takeFloorLocked(ex)
			r.s.endLocked(ex, false, "test")
			chimed := r.s.pending[0].chimed
			r.s.mu.Unlock()
			if release != nil {
				release()
			}
			if chimed {
				t.Fatal("the hand whose sound was cut is still marked as sounded")
			}
			eventually(t, "the hand sound again", func() bool {
				names, frames := r.speaker.chimePlaybacks()
				n := 0
				for i, name := range names {
					if name == "hand" && frames[i] > 0 {
						n++
					}
				}
				return n >= map[bool]int{false: 1, true: 2}[queued]
			})
		})
	}
}

// flagMembership is a membership context whose end only its Err reports: its
// Done never closes, so nothing waiting on it learns of the end, as a callback
// that has not run yet would not.
type flagMembership struct {
	context.Context
	ended atomic.Bool
}

func (m *flagMembership) Err() error {
	if m.ended.Load() {
		return context.Canceled
	}
	return nil
}

// TestAHandsCompletionReadsItsMembership fails if a hand whose membership ended
// while its words played out is reported spoken, or put back with the error
// sound, because the completion read only its exchange: an ended membership's
// hand is withdrawn, whether the speech then drained or failed.
func TestAHandsCompletionReadsItsMembership(t *testing.T) {
	for _, drained := range []bool{true, false} {
		t.Run(map[bool]string{true: "the speech drained", false: "the speech was cut"}[drained], func(t *testing.T) {
			r, outcomes := newHandsRig(t)
			membership := &flagMembership{Context: context.Background()}
			h := testHand("h1", "ABC-12", time.Minute)
			h.Context = membership
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the hand sound", func() bool { return slices.Contains(r.speaker.soundsPlayed(), "hand") })
			r.heard("Wispr, go ahead.", true, true)
			waitSaid(t, r.waitSession())
			time.Sleep(50 * time.Millisecond) // the completion waits for the speech to play out
			membership.ended.Store(true)
			if drained {
				r.speaker.finish(audio.InputVoice)
			} else {
				r.speaker.Cancel(audio.InputVoice)
			}
			eventually(t, "the hand's outcome", func() bool { return len(outcomes.all()) > 0 })
			if got := outcomes.all(); !slices.Equal(got, []string{"h1:withdrawn"}) {
				t.Fatalf("outcomes %q, want the hand of the ended membership withdrawn", got)
			}
			if slices.Contains(r.speaker.soundsPlayed(), "error") {
				t.Fatal("the ended membership's completion played the error sound")
			}
		})
	}
}
