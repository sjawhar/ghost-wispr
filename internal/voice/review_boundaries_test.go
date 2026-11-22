package voice

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Sol's round-5 boundary probes of #143, kept as tests. They checked which
// exchange the round-5 rule, by floor spans, gave a speech whose start came
// at each side of a span's edge, a span's 30 s retention, and a start 45 s
// old. A speech is now its playback's owner's (audio.SpeechEvent.Owner): the
// same speeches carry their owner, and the time and the floor's holder must
// change nothing. They drive the production onSpeech on a bare Supervisor.

// boundarySupervisor is a bare Supervisor that onSpeech can run on.
func boundarySupervisor(t *testing.T) *Supervisor {
	return &Supervisor{notes: make(chan [2]string, 8), logf: t.Logf, cfg: SupervisorConfig{IdleWait: time.Hour, FollowUpWait: time.Hour, followUpAfter: time.AfterFunc}}
}

// stopTimers stops the timers a driving exchange's speech arms.
func stopTimers(exs ...*exchange) {
	for _, ex := range exs {
		if ex.idle != nil {
			ex.idle.Stop()
		}
		if ex.followUpTimer != nil {
			ex.followUpTimer.Stop()
		}
	}
}

func TestReviewFloorSpanBoundaries(t *testing.T) {
	now := time.Now()
	makeEx := func(id string, ended bool) *exchange {
		return &exchange{x: Exchange{SessionID: id}, drives: true, ended: ended, notified: map[string]bool{}}
	}
	for _, tc := range []struct {
		name   string
		offset time.Duration
		open   bool
		want   string // whose playback started the speech; "" for none
	}{
		{"before first", -time.Nanosecond, false, ""},
		{"first from", 0, false, "a"},
		{"before shared boundary", time.Second - time.Nanosecond, false, "a"},
		{"shared boundary", time.Second, false, "b"},
		{"last to", 2 * time.Second, false, ""},
		{"open from", 2 * time.Second, true, "c"},
		{"open later", 3 * time.Second, true, "c"},
		// The owner decides even against the floor and the time.
		{"an ended exchange's speech while another holds the floor", 3 * time.Second, true, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, c := makeEx("a", true), makeEx("b", true), makeEx("c", false)
			byID := map[string]*exchange{"a": a, "b": b, "c": c}
			s := boundarySupervisor(t)
			if tc.open {
				s.ex = c
			}
			defer stopTimers(a, b, c)
			var owner any
			if ex := byID[tc.want]; ex != nil {
				owner = ex
			}
			at := now.Add(-5 * time.Second).Add(tc.offset)
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: at, Owner: owner})
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, At: at.Add(time.Nanosecond), Played: time.Nanosecond, Owner: owner})
			for _, ex := range []*exchange{a, b, c} {
				if ex.notified[EventSpoken] != (ex.x.SessionID == tc.want) {
					t.Fatalf("exchange %s spoken=%v, want owner %q", ex.x.SessionID, ex.notified[EventSpoken], tc.want)
				}
			}
		})
	}
}

func TestReviewFloorThirtySecondRetention(t *testing.T) {
	now := time.Now()
	for _, age := range []time.Duration{ownWordsKept - time.Nanosecond, ownWordsKept, ownWordsKept + time.Nanosecond, 10 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			a := &exchange{x: Exchange{SessionID: "a"}, drives: true, ended: true, notified: map[string]bool{}}
			b := &exchange{x: Exchange{SessionID: "b"}, drives: true, ended: true, notified: map[string]bool{}}
			s := boundarySupervisor(t)
			defer stopTimers(a, b)
			at := now.Add(-age)
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: at, Owner: a})
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, At: at.Add(time.Microsecond), Played: time.Microsecond, Owner: a})
			if !a.notified[EventSpoken] || b.notified[EventSpoken] {
				t.Fatalf("age=%v a spoken=%v b spoken=%v, want true, false", age, a.notified[EventSpoken], b.notified[EventSpoken])
			}
		})
	}
}

func TestReviewOldStartOnOpenOrRecentlyClosedSpan(t *testing.T) {
	for _, open := range []bool{false, true} {
		t.Run(map[bool]string{false: "recently closed", true: "still open"}[open], func(t *testing.T) {
			now := time.Now()
			ex := &exchange{x: Exchange{SessionID: "long"}, drives: true, ended: !open, notified: map[string]bool{}}
			s := boundarySupervisor(t)
			defer stopTimers(ex)
			if open {
				s.ex = ex
			}
			at := now.Add(-45 * time.Second)
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: at, Owner: ex})
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, At: at.Add(time.Second), Played: time.Second, Owner: ex})
			if !ex.notified[EventSpoken] {
				t.Fatal("old start was not reported spoken")
			}
		})
	}
}

// TestALateStartOfAnEndedExchangeTimesNothing fails if the speaker's late
// report that an ended exchange's speech started times or logs its first
// audio: those are a running exchange's, and a log made after the exchange,
// and the test that ran it, ended races the test's end (GitHub CI at fa3273a,
// TestADeleteForgetsTheRecentTranscript). The control's exchange still runs,
// and its first audio is logged.
func TestALateStartOfAnEndedExchangeTimesNothing(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "control: the exchange runs", true: "the exchange ended"}[ended], func(t *testing.T) {
			var logged []string
			s := boundarySupervisor(t)
			s.logf = func(format string, args ...any) { logged = append(logged, format) }
			ex := &exchange{x: Exchange{SessionID: "a"}, drives: true, ended: ended, notified: map[string]bool{}, turnAt: time.Now().Add(-time.Second)}
			defer stopTimers(ex)
			s.onSpeech(&audio.SpeechEvent{Input: audio.InputVoice, Started: true, At: time.Now(), Owner: ex})
			if got := slices.Contains(logged, "voice: first spoken audio %v after the turn ended"); got == ended || ex.firstOutDone == ended {
				t.Fatalf("first audio logged: %v, timed: %v; want both %v", got, ex.firstOutDone, !ended)
			}
		})
	}
}
