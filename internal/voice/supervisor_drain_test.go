package voice

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// The tests here hold a sound to what the speaker still plays of it: a
// notification belongs to its answers or hands until its last frame has
// played, however late the speaker plays it, and an error sound for a
// meeting plays only while the recorder holds that meeting.

// chimeFrames is how many 20 ms frames a sound is written in.
func chimeFrames(pcm []byte) int { return (len(pcm) + 959) / 960 }

// TestANotificationKeepsItsOwnersUntilItDrains fails if a hand sound whose
// frames are all queued, and still play from the speaker (its tail held up
// past any fixed allowance), is forgotten as played: the last hand coming
// down must cut it. A hand still up keeps it: the control.
func TestANotificationKeepsItsOwnersUntilItDrains(t *testing.T) {
	for _, lowered := range []bool{false, true} {
		t.Run(map[bool]string{false: "the hand still up", true: "the last hand lowered"}[lowered], func(t *testing.T) {
			r, _, _ := newHandsWaitingRig(t)
			r.speaker.holdDrain(audio.InputChime)
			h := testHand("h1", "ABC-12", time.Minute)
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatal(err)
			}
			eventually(t, "every frame of the hand sound queued", func() bool {
				return r.speaker.written(audio.InputChime) >= chimeFrames(HandChime())
			})
			r.speaker.mu.Lock()
			gen := r.speaker.gen[audio.InputChime]
			r.speaker.mu.Unlock()
			time.Sleep(500 * time.Millisecond) // the speaker still plays it
			if lowered {
				r.s.LowerHands(h.Membership)
			}
			r.speaker.mu.Lock()
			cut := r.speaker.gen[audio.InputChime] != gen
			r.speaker.mu.Unlock()
			switch {
			case !lowered && cut:
				t.Fatal("control: a hand still up lost the sound the speaker still plays")
			case lowered && !cut:
				t.Fatal("the hand sound the speaker still played was left playing after its last hand came down")
			}
		})
	}
}

// TestASoundWaitsForTheOneStillPlaying fails if a hand raised while another
// hand's sound still plays from the speaker is counted as sounded by it, or
// gets a sound of its own begun on top of it. Its own sound plays once the
// first has played out.
func TestASoundWaitsForTheOneStillPlaying(t *testing.T) {
	r, _, _ := newHandsWaitingRig(t)
	r.speaker.holdDrain(audio.InputChime)
	if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "every frame of the first hand sound queued", func() bool {
		return r.speaker.written(audio.InputChime) >= chimeFrames(HandChime())
	})
	second := testHand("h2", "XYZ-9", time.Minute)
	if err := r.s.RaiseHand(second); err != nil {
		t.Fatal(err)
	}
	never(t, "a second sound begun while the first still played", 300*time.Millisecond, func() bool {
		names, _ := r.speaker.chimePlaybacks()
		return len(names) > 1
	})
	r.s.mu.Lock()
	i := slices.IndexFunc(r.s.pending, func(p *pendingAnswer) bool { return p.hand == second })
	chimed := i >= 0 && r.s.pending[i].chimed
	r.s.mu.Unlock()
	if chimed {
		t.Fatal("the second hand was counted as sounded by a sound that had finished writing before it was raised")
	}
	r.speaker.finish(audio.InputChime)
	eventually(t, "the second hand's own sound once the first played out", func() bool {
		names, _ := r.speaker.chimePlaybacks()
		return len(names) == 2 && names[1] == "hand"
	})
}

// TestAMeetingErrorSoundReadsItsMeeting fails if an error sound for a
// meeting, held up before its first frame, plays once the recorder ended
// that meeting. A live meeting's error sound plays: the control.
func TestAMeetingErrorSoundReadsItsMeeting(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "the meeting records on", true: "the recorder ended the meeting"}[ended], func(t *testing.T) {
			meeting, end := context.WithCancel(context.Background())
			defer end()
			r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, _ *rig) {
				cfg.Meeting = func(string) context.Context { return meeting }
			})
			release := r.speaker.holdBegins(audio.InputChime)
			r.s.mu.Lock()
			r.s.errorLocked("sess-1")
			r.s.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			if ended {
				end()
			}
			release()
			if !ended {
				eventually(t, "control: the live meeting's error sound", func() bool { return r.speaker.written(audio.InputChime) > 0 })
				return
			}
			never(t, "the error sound played after the recorder ended its meeting", 200*time.Millisecond, func() bool {
				return r.speaker.written(audio.InputChime) > 0
			})
		})
	}
}
