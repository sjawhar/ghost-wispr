package voice

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's twenty-third review of #191 at 2066336, and Main's rules after it:
// whether GPT-Live asked is decided on its completed speech, the whole
// transcript at the speech's end, never on a fragment; a later speech that
// does not end in a question takes the ask-back away; it lapses recentKept
// after the question; and the room's recent words a voice reads through
// Exchange.Room are the retained store as it is now, aged.

// voiceAskRig is a driving voice's exchange opened on a question, with a
// follow-up window long enough for the test to speak in it.
func voiceAskRig(t *testing.T) (*rig, *fakeSession) {
	t.Helper()
	r := newRigWith(t, newFakeConcierge("fact"), func(cfg *SupervisorConfig, r *rig) {
		v := drivingVoice{r.voice}
		cfg.Voice = func() (Voice, error) { return v, nil }
		cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
	})
	r.heard("Wispr, why is the recorder blocked?", true, true)
	sess := r.waitSession()
	return r, sess
}

// voiceSpeaks has the voice say one speech whose transcript arrives in
// deltas, and returns once the Supervisor has taken in the speaker's report
// of its end (the follow-up window it opens). With cut, the speaker ends the
// speech cancelled, before its end, as a cut does.
func voiceSpeaks(t *testing.T, r *rig, sess *fakeSession, cut bool, deltas ...string) {
	t.Helper()
	for _, d := range deltas {
		sess.send(EventAgentText, d)
	}
	want := strings.Join(deltas, "")
	eventually(t, "the voice's transcript reached the Supervisor", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex != nil && strings.HasSuffix(r.s.ex.voiceLine, want)
	})
	prev := speechStart(r)
	speakAs(t, sess, "") // the speech's audio; its words came above
	waitSpeechAfter(t, r, prev)
	r.s.mu.Lock()
	gen := r.s.ex.followUpGen
	r.s.mu.Unlock()
	if cut {
		r.speaker.Cancel(audio.InputVoice)
	} else {
		r.speaker.finish(audio.InputVoice)
	}
	eventually(t, "the speech's end reached the Supervisor", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.ex.followUpGen > gen && r.s.ex.followUpTimer != nil
	})
}

func voiceAsked(r *rig) (askedBack, voiceAsked bool) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.ex.askedBack, r.s.ex.voiceAsked
}

// TestTheVoicesQuestionIsJudgedOnTheCompletedSpeech: the same words asked
// in one delta or two give one answer, from the whole speech. At 2066336 a
// question mark in an earlier delta ("Is it ready?", " It is ready.") left the
// ask-back set, so a bare key after it reached the model (Sol's
// TestSol191R23AskingBoundary/question_then_final_statement); a later
// statement in another speech did not take it away either; and a question cut
// off before its speech ended, which the room never heard whole, still set it.
func TestTheVoicesQuestionIsJudgedOnTheCompletedSpeech(t *testing.T) {
	for _, tc := range []struct {
		name     string
		speeches [][]string
		cut      bool // the last speech is cut before its end
		want     bool
	}{
		{"control: a question", [][]string{{"What is its key?"}}, false, true},
		{"control: a question in two deltas", [][]string{{"What is", " its key?"}}, false, true},
		{"a statement after a question, one delta", [][]string{{"Is it ready? It is ready."}}, false, false},
		{"a statement after a question, two deltas", [][]string{{"Is it ready?", " It is ready."}}, false, false},
		{"a later statement", [][]string{{"What is its key?"}, {"The recorder is on."}}, false, false},
		{"a question cut before its speech ended", [][]string{{"What is its key?"}}, true, false},
		{"control: a plain statement", [][]string{{"The recorder is on."}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sess := voiceAskRig(t)
			for i, deltas := range tc.speeches {
				voiceSpeaks(t, r, sess, tc.cut && i == len(tc.speeches)-1, deltas...)
			}
			if askedBack, asked := voiceAsked(r); askedBack != tc.want || asked != tc.want {
				t.Errorf("after %q the ask-back is %v (the voice's own %v), want %v", tc.speeches, askedBack, asked, tc.want)
			}
		})
	}
}

// TestTheVoicesQuestionLapsesAfterRecentKept: a reply recentKept after
// GPT-Live's question is no reply to it, as nothing older than recentKept is
// any model's history either. At 2066336 it never lapsed (Sol's
// TestSol191R23QuestionStateAfterTwoMinutes, at the production two minutes).
// Control: a reply within the time is a reply.
func TestTheVoicesQuestionLapsesAfterRecentKept(t *testing.T) {
	kept := recentKept
	recentKept = 300 * time.Millisecond
	t.Cleanup(func() { recentKept = kept })
	for _, late := range []bool{false, true} {
		name := "control: a reply in time"
		if late {
			name = "a reply after recentKept"
		}
		t.Run(name, func(t *testing.T) {
			r, sess := voiceAskRig(t)
			voiceSpeaks(t, r, sess, false, "What is its key?")
			if late {
				time.Sleep(recentKept + 100*time.Millisecond)
			}
			r.heard("ABC-12", true, true)
			if askedBack, _ := voiceAsked(r); askedBack == late {
				t.Errorf("a reply %v after the question: ask-back %v, want %v", map[bool]string{false: "at once", true: "after recentKept"}[late], askedBack, !late)
			}
		})
	}
}

// TestARoomReadSeesTheRetainedWordsAsTheyAreNow (Sol's
// TestSolR23RoomHistoryTracksNewFinals and TestSolR23RoomHistoryAgesOnRead,
// which fail at 2066336 and its parent): Exchange.Room read the copy the
// exchange opened with, so a final retained since was in the request but not
// the history, and an entry older than recentKept stayed. It reads the
// retained store as it is now, as the answer tool does.
func TestARoomReadSeesTheRetainedWordsAsTheyAreNow(t *testing.T) {
	kept := recentKept
	recentKept = 400 * time.Millisecond
	t.Cleanup(func() { recentKept = kept })
	has := func(recent []TurnText, text string) bool {
		return slices.ContainsFunc(recent, func(l TurnText) bool { return strings.Contains(l.Text, text) })
	}
	r, sess := voiceAskRig(t)
	if !has(sess.x.Room().Recent, "recorder blocked") {
		t.Fatalf("control: the opening is not in the room's recent words: %+v", sess.x.Room().Recent)
	}
	r.heard("and the observer changed the deployment window", true, false)
	room := sess.x.Room()
	if !strings.Contains(room.Request, "deployment window") {
		t.Fatalf("control: the new final is not in the request: %q", room.Request)
	}
	if !has(room.Recent, "deployment window") {
		t.Errorf("a final retained since the exchange opened is missing from Room.Recent: %+v", room.Recent)
	}
	time.Sleep(recentKept + 100*time.Millisecond)
	if aged := sess.x.Room().Recent; has(aged, "recorder blocked") {
		t.Errorf("an entry older than recentKept stayed in Room.Recent: %+v", aged)
	}
}
