package voice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// textGateVoice opens the rig's fake sessions, as a voice that drives its
// own turn, whose events the Supervisor reads only once release closes,
// having closed entered: a voice's text can be read after its audio played.
type textGateVoice struct {
	voice            *fakeVoice
	entered, release chan struct{}
}

func (*textGateVoice) DrivesTurn() bool { return true }

func (v *textGateVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return &textGateSession{Session: s, entered: v.entered, release: v.release}, nil
}

type textGateSession struct {
	Session
	entered, release chan struct{}
}

func (s *textGateSession) Events() <-chan Event {
	close(s.entered)
	<-s.release
	return s.Session.Events()
}

// TestALineReadAfterItsSpeechStillMatchesItsEcho fails if the voice's own
// words, heard back during its first speech and read during its next, stop
// that next speech when the Supervisor read the first line's text only after
// the first speech ended: a match of a word against the lines said before
// its speech ended left that line out, where main finds the echo (Sol's
// review of #133 at e6484db). The controls read the text in time, and read
// other words said at the same moment, which stop the voice.
func TestALineReadAfterItsSpeechStillMatchesItsEcho(t *testing.T) {
	for _, late := range []bool{false, true} {
		for _, own := range []bool{false, true} {
			t.Run(fmt.Sprintf("read after the speech %v, its own words %v", late, own), func(t *testing.T) {
				entered, gate := make(chan struct{}), make(chan struct{})
				letRead := func() {
					select {
					case <-gate:
					default:
						close(gate)
					}
				}
				t.Cleanup(letRead)
				r := newRigWith(t, newFakeConcierge(), func(cfg *SupervisorConfig, r *rig) {
					cfg.Voice = func() (Voice, error) { return &textGateVoice{voice: r.voice, entered: entered, release: gate}, nil }
					cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
				})
				r.heard("Wispr, what is the status?", true, true)
				sess := r.waitSession()
				select {
				case <-entered:
				case <-time.After(testWait):
					t.Fatal("fixture: the Supervisor never asked for the session's events")
				}
				if err := sess.Say(t.Context(), "Release progress.", Answer); err != nil {
					t.Fatal(err)
				}
				waitSaid(t, sess)
				first := waitSpeechAfter(t, r, time.Time{})
				registered := func() bool {
					r.s.mu.Lock()
					defer r.s.mu.Unlock()
					for _, o := range r.s.own {
						if o.words["release"] && o.words["progress"] {
							return true
						}
					}
					return false
				}
				if !late {
					letRead()
					eventually(t, "the first line's text read", registered)
				}
				said := from(first.Add(30 * time.Millisecond))
				time.Sleep(time.Until(said(1)) + 20*time.Millisecond)
				r.speaker.finish(audio.InputVoice)
				r.speaker.FlushSpeech()
				if late {
					if registered() {
						t.Fatal("fixture: the held text was read before the speech ended")
					}
					letRead()
					eventually(t, "the first line's text read late", registered)
				}
				if err := sess.Say(t.Context(), "Ada holds it.", Answer); err != nil {
					t.Fatal(err)
				}
				waitSaid(t, sess)
				second := waitSpeechAfter(t, r, first)
				text := "Hold on."
				if own {
					text = "Release progress."
				}
				r.s.OnTranscript(timedResult(text, 200, true, said))
				if cut := voiceCutAfter(r, second); cut == own {
					t.Fatalf("the voice was cut %v, want %v: the first line's text read late %v, the words its own %v", cut, !own, late, own)
				}
			})
		}
	}
}

// TestTheVoicesWordsStayItsOwnWhileItsSpeechPlays fails if the voice's own
// words, heard back more than ownWordsKept after their line was said while
// that speech still plays, stop it: they were matched only against lines
// said up to ownWordsKept before them, not before their speech began (Sol's
// review of #133 at 4238ad3). The control hears them back at once.
func TestTheVoicesWordsStayItsOwnWhileItsSpeechPlays(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("heard after the horizon %v", late), func(t *testing.T) {
			kept := ownWordsKept
			ownWordsKept = 300 * time.Millisecond
			t.Cleanup(func() { ownWordsKept = kept }) // after the rig has stopped
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what is the status?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, "Release progress.")
			started := waitSpeechAfter(t, r, time.Time{})
			after := 30 * time.Millisecond
			if late {
				after += ownWordsKept
			}
			said := from(started.Add(after))
			time.Sleep(time.Until(said(1)) + 20*time.Millisecond)
			r.s.OnTranscript(timedResult("Release progress.", 200, true, said))
			never(t, "the voice's own words stopped its speech", 200*time.Millisecond, func() bool { return voiceCutAfter(r, started) })
		})
	}
}

// TestAnOldEchoWithItsSpeechStartQueuedIsTheVoicesOwn fails if the voice's
// own words, heard back during a speech whose start event was still queued
// when a later line was said after ownWordsKept, stop its next speech or
// become the asker's turn: the later line forgot the earlier line's words,
// as the exchange had no span for them yet (Sol's review of #133 at
// 4238ad3). The control reads other words said at the same moment, which
// stop it.
func TestAnOldEchoWithItsSpeechStartQueuedIsTheVoicesOwn(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprintf("its own words %v", own), func(t *testing.T) {
			kept := ownWordsKept
			ownWordsKept = 300 * time.Millisecond
			t.Cleanup(func() { ownWordsKept = kept }) // after the rig has stopped
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what is the status?", true, true)
			sess := r.waitSession()
			release := r.speaker.holdEvents()
			defer release()
			const line = "Release progress."
			r.s.mu.Lock()
			r.s.addOwnLocked(line, time.Now()) // as GPT-Live's text of a line does
			r.s.mu.Unlock()
			if err := sess.x.Out.Begin().Write(t.Context(), []byte{1, 0}, ChimeFormat); err != nil {
				t.Fatal(err)
			}
			first := queuedStart(t, r, time.Time{})
			said := from(first.Add(30 * time.Millisecond))
			time.Sleep(time.Until(said(1)) + 20*time.Millisecond)
			r.speaker.finish(audio.InputVoice)
			time.Sleep(ownWordsKept + 100*time.Millisecond)
			r.s.mu.Lock()
			r.s.addOwnLocked("Ada holds it.", time.Now()) // the next line, the first start still queued
			r.s.mu.Unlock()
			if err := sess.x.Out.Begin().Write(t.Context(), []byte{1, 0}, ChimeFormat); err != nil {
				t.Fatal(err)
			}
			second := queuedStart(t, r, first)
			text := line
			if !own {
				text = "Hold on."
			}
			r.s.OnTranscript(timedResult(text, 200, true, said))
			cut := voiceCutAfter(r, second)
			if !own {
				if !cut {
					t.Fatal("control: words said over the earlier speech did not stop the next one")
				}
				return
			}
			if cut {
				t.Fatal("the voice's old echo stopped its next speech")
			}
			r.s.mu.Lock()
			turn := r.s.turnOf(r.s.ex)
			r.s.mu.Unlock()
			if turn != "what is the status?" {
				t.Fatalf("the echo made the turn %q", turn)
			}
		})
	}
}

// queuedStart returns the time of the fake speaker's queued start of the
// voice after after, failing t if none is queued.
func queuedStart(t *testing.T, r *rig, after time.Time) time.Time {
	t.Helper()
	r.speaker.mu.Lock()
	defer r.speaker.mu.Unlock()
	for _, e := range r.speaker.queued {
		if e.Input == audio.InputVoice && e.Started && e.At.After(after) {
			return e.At
		}
	}
	t.Fatal("fixture: no speech start of the voice is queued")
	return time.Time{}
}

// TestAWordRetimedInsideTheCarriedWordIsNotASecondWord fails if a word
// whose capture starts inside the carried word, the same 40 ms of audio
// retimed by 10 ms, pairs with it as a second word: only a word starting at
// or after the carried word's end is another (Sol's review of #133 at
// 4238ad3 found only an identical start tested). The controls start at the
// carried word's end and 200 ms later, and pair.
func TestAWordRetimedInsideTheCarriedWordIsNotASecondWord(t *testing.T) {
	for _, tc := range []struct {
		after   time.Duration
		text    string
		wantCut bool
	}{
		{10 * time.Millisecond, "It.", false},
		{40 * time.Millisecond, "on.", true},
		{200 * time.Millisecond, "on.", true},
	} {
		t.Run(fmt.Sprintf("+%v", tc.after), func(t *testing.T) {
			base := time.Now().Add(-time.Second)
			ex := &exchange{played: []span{{from: base.Add(-time.Second)}}}
			first := &Transcript{Text: "It.", IsFinal: true, GateOpen: true, At: time.Now(), Words: []Word{{Text: "It.", Start: 200, End: 200.04, At: base}}}
			cut, carried := bargesIn(first, nil, ex, nil)
			if cut || carried == nil || carried.endAt.Sub(carried.at) != 40*time.Millisecond {
				t.Fatalf("control: the first word cut %v, carried %+v", cut, carried)
			}
			start := 200 + tc.after.Seconds()
			second := &Transcript{Text: tc.text, GateOpen: true, At: time.Now(), Words: []Word{{Text: tc.text, Start: start, End: start + 0.04, At: base.Add(tc.after)}}}
			if cut, _ := bargesIn(second, nil, ex, carried); cut != tc.wantCut {
				t.Fatalf("a word at +%v cut %v, want %v: the carried word ends at +40ms", tc.after, cut, tc.wantCut)
			}
		})
	}
}
