package voice

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestALaterLineDoesNotHideAnEarlierTalkOver fails if words said over the
// voice's first speech, read only during its next, are taken as its echo
// because the next line has those words: a line said after the speech the
// words were heard in ended cannot have been heard in it (Sol's review of
// #133 at 4238ad3). The control's next line shares no word with them; both
// must stop the voice.
func TestALaterLineDoesNotHideAnEarlierTalkOver(t *testing.T) {
	for _, later := range []string{"Ada holds it.", "Hold on while I check."} {
		t.Run(later, func(t *testing.T) {
			r := newLongFollowUpDrivingRig(t)
			r.heard("Wispr, what is the status?", true, true)
			sess := r.waitSession()
			speakAs(t, sess, "The issue is in review.")
			first := waitSpeechAfter(t, r, time.Time{})
			said := from(first.Add(30 * time.Millisecond))
			time.Sleep(time.Until(said(1)) + 20*time.Millisecond)
			r.speaker.finish(audio.InputVoice)
			r.speaker.FlushSpeech()
			speakAs(t, sess, later)
			second := waitSpeechAfter(t, r, first)
			if !said(1).Before(second) {
				t.Fatal("control: the words were said after the next speech began")
			}
			r.s.OnTranscript(timedResult("Hold on.", 200, true, said))
			eventually(t, "the earlier talk-over to stop the voice", func() bool { return voiceCutAfter(r, second) })
		})
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
