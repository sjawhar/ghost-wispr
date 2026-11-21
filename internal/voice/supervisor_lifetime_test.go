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

// The tests here hold each effect of the voice to the lifetime of what it is
// for: an exchange to its meeting, a hand's words to its membership while a
// write waits for room, a hand sound to the hands it is for, and the
// never-mind barrier to answers alone.

// TestAnExchangeEndsWithItsMeeting fails if an exchange keeps its voice open,
// or holds the floor, once the recorder ends its meeting (the meeting
// context, which the recorder cancels in the same step that clears its
// session), with no session-ended hook; a live meeting's exchange is the
// control.
func TestAnExchangeEndsWithItsMeeting(t *testing.T) {
	meeting, end := context.WithCancel(context.Background())
	defer end()
	c := newFakeConcierge("It is in review.")
	c.hold = true
	r := newRigWith(t, c, func(cfg *SupervisorConfig, _ *rig) {
		cfg.Meeting = func(id string) context.Context {
			if id != "sess-1" {
				t.Errorf("the exchange asked for the meeting of %q, want sess-1", id)
			}
			return meeting
		}
	})
	sess := r.ask("what is left?")
	never(t, "control: a live meeting's exchange ended", 200*time.Millisecond, func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex == nil
	})
	end()
	eventually(t, "the exchange ended with its meeting", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex == nil
	})
	eventually(t, "the voice closed", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.closed
	})
}

// waitingPlayback holds each Write until released, and commits the frame only
// if the context it was given is live then, as audio.Output's queue does
// while it waits for room.
type waitingPlayback struct {
	entered   chan struct{}
	release   chan struct{}
	committed atomic.Bool
}

func (p *waitingPlayback) Write(ctx context.Context, _ []byte, _ audio.AudioFormat) error {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.committed.Store(true)
	return nil
}

func (p *waitingPlayback) Drain(ctx context.Context) error { return ctx.Err() }

// TestAHandsFrameWaitingForRoomEndsWithItsMembership fails if a frame of a
// hand's words, written while the speaker's queue has no room, is committed
// once the membership ended during the wait; or if the writer's own
// cancellation no longer stops it, or a live frame is refused.
func TestAHandsFrameWaitingForRoomEndsWithItsMembership(t *testing.T) {
	for _, end := range []string{"nothing", "the membership", "the writer"} {
		t.Run(end, func(t *testing.T) {
			membership, endMembership := context.WithCancel(context.Background())
			defer endMembership()
			writer, endWriter := context.WithCancel(context.Background())
			defer endWriter()
			under := &waitingPlayback{entered: make(chan struct{}), release: make(chan struct{})}
			p := sayPlayback{Playback: under, ctx: membership}
			done := make(chan error, 1)
			go func() { done <- p.Write(writer, []byte{1, 0}, ChimeFormat) }()
			<-under.entered
			switch end {
			case "the membership":
				endMembership()
			case "the writer":
				endWriter()
			}
			close(under.release)
			err := <-done
			switch {
			case end == "nothing" && (err != nil || !under.committed.Load()):
				t.Fatalf("control: a live frame was refused: %v", err)
			case end != "nothing" && under.committed.Load():
				t.Fatalf("a frame was committed after %s ended while it waited for room", end)
			}
		})
	}
}

// TestAnEndedHandIsRefusedBeforeTheLight fails if RaiseHand takes a hand
// whose membership already ended, or turns the waiting light on for it; a
// live hand is the control.
func TestAnEndedHandIsRefusedBeforeTheLight(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "a live hand", true: "an ended hand"}[ended], func(t *testing.T) {
			r, light, _ := newHandsWaitingRig(t)
			membership, end := context.WithCancel(context.Background())
			defer end()
			if ended {
				end()
			}
			h := testHand("h1", "ABC-12", time.Minute)
			h.Context, h.Current = membership, func() bool { return membership.Err() == nil }
			err := r.s.RaiseHand(h)
			on := slices.Contains(light.callsCopy(), "on")
			if !ended && (err != nil || !on) {
				t.Fatalf("control: a live hand was refused or lit nothing: %v, %q", err, light.callsCopy())
			}
			if ended && (err == nil || on) {
				t.Fatalf("an ended hand was taken (%v) or lit the waiting light (%q)", err, light.callsCopy())
			}
		})
	}
}

// TestTheLastHandsEndCancelsItsSound fails if a hand sound that has not begun
// plays once the last hand it was for came down; a hand still up is the
// control.
func TestTheLastHandsEndCancelsItsSound(t *testing.T) {
	for _, lowered := range []bool{false, true} {
		t.Run(map[bool]string{false: "the hand still up", true: "the last hand lowered"}[lowered], func(t *testing.T) {
			r, light, _ := newHandsWaitingRig(t)
			release := r.speaker.holdBegins(audio.InputChime)
			var once sync.Once
			unblock := func() { once.Do(release) }
			defer unblock()
			h := testHand("h1", "ABC-12", time.Minute)
			if err := r.s.RaiseHand(h); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the hand sound under way", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.chimeCancel != nil && r.s.chimeHands
			})
			if lowered {
				r.s.LowerHands(h.Membership)
				if light.lit() {
					t.Fatal("control: the waiting light stayed on")
				}
			}
			unblock()
			deadline := time.Now().Add(250 * time.Millisecond)
			for len(r.speaker.soundsPlayed()) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			played := slices.Contains(r.speaker.soundsPlayed(), "hand")
			r.speaker.finish(audio.InputChime)
			if !lowered && !played {
				t.Fatal("control: the hand sound did not play")
			}
			if lowered && played {
				t.Fatal("the hand sound played after the last hand came down")
			}
		})
	}
}

// TestNeverMindCutsTheQueuedAnswerSoundBehindAPlannedHandSound fails if a
// never mind that drops the last answer leaves that answer's sound, still
// playing from the speaker, in its queue because a hand waits to sound after
// it; the hand's sound and its waiting light must survive.
func TestNeverMindCutsTheQueuedAnswerSoundBehindAPlannedHandSound(t *testing.T) {
	c := newFakeConcierge()
	r, light := newWaitingRig(t, c)
	r.speaker.holdDrain(audio.InputChime)
	holdAnswer(t, r, light, "t1", "It ships tomorrow.")
	eventually(t, "the answer sound queued", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.chimeDraining && !r.s.chimeHands
	})
	r.speaker.mu.Lock()
	answerGen := r.speaker.chimes[0].gen
	r.speaker.mu.Unlock()
	if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
		t.Fatal(err)
	}
	r.heard("Wispr, never mind.", true, true)
	eventually(t, "only the hand waiting", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return len(r.s.pending) == 1 && r.s.pending[0].hand != nil
	})
	r.speaker.mu.Lock()
	answerLive := r.speaker.gen[audio.InputChime] == answerGen
	r.speaker.mu.Unlock()
	eventually(t, "control: the hand sound", func() bool { return slices.Contains(r.speaker.soundsPlayed(), "hand") })
	if !light.lit() {
		t.Fatal("control: the hand's waiting light went off")
	}
	r.speaker.finish(audio.InputChime)
	if answerLive {
		t.Fatal("never mind left the dropped answer's sound in the speaker's queue")
	}
}

// TestAPendingNeverMindLetsAHandSound fails if a hand raised while a never
// mind waits for the Concierge gets no sound until the Concierge answers, or
// if letting it sound also lets a held answer sound.
func TestAPendingNeverMindLetsAHandSound(t *testing.T) {
	for _, answerToo := range []bool{false, true} {
		t.Run(map[bool]string{false: "a hand", true: "a hand and a held answer"}[answerToo], func(t *testing.T) {
			c := newFakeConcierge()
			c.working = 1
			c.neverMindHold = make(chan struct{})
			var once sync.Once
			answer := func() { once.Do(func() { close(c.neverMindHold) }) }
			defer answer()
			r, light := newWaitingRig(t, c)
			r.heard("Wispr, never mind.", true, true)
			eventually(t, "the never mind under way", func() bool { return len(c.neverMindCalls()) == 1 })
			if answerToo {
				c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow."}
				eventually(t, "the answer held", func() bool {
					r.s.mu.Lock()
					defer r.s.mu.Unlock()
					return len(r.s.pending) > 0
				})
			}
			if err := r.s.RaiseHand(testHand("h1", "ABC-12", time.Minute)); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the hand sound while the never mind waits", func() bool { return slices.Contains(r.speaker.soundsPlayed(), "hand") })
			if !light.lit() {
				t.Fatal("control: the hand's waiting light is off")
			}
			if slices.Contains(r.speaker.soundsPlayed(), "answer") {
				t.Fatal("an answer sounded while the never mind waited")
			}
		})
	}
}
