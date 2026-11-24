package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// errSpeechCut is how a voice's speech ended before its end with no stop: as
// voice A's does when its engine or the Output ends it after Say returned.
var errSpeechCut = errors.New("the speaker ended the playback")

// cutSpeechVoice opens the rig's fake sessions as voice A opens its own: Say
// returns once its speech is queued, and WaitSpoken reports the speech's end,
// here spokenErr. With reported set, the session also reports the failure
// (EventErr) before WaitSpoken returns, as voice A does for a write the
// Output refused.
type cutSpeechVoice struct {
	r         *rig
	spokenErr error
	reported  bool
}

func (v cutSpeechVoice) Open(ctx context.Context, x *Exchange) (Session, error) {
	s, err := v.r.voice.Open(ctx, x)
	if err != nil {
		return nil, err
	}
	return cutSpeechSession{fakeSession: s.(*fakeSession), v: v}, nil
}

type cutSpeechSession struct {
	*fakeSession
	v cutSpeechVoice
}

func (s cutSpeechSession) WaitSpoken(context.Context) error {
	if s.v.reported {
		s.events <- Event{Kind: EventErr, Err: s.v.spokenErr}
		time.Sleep(100 * time.Millisecond) // the Supervisor reads the report first
	}
	return s.v.spokenErr
}

// TestAnAnswerCutWithNoStopIsMetWithTheErrorSound (R10; the office Pi on
// 2026-11-24): voice A's Say returns once its text's first frame is queued,
// and a text its engine or the Output ends after that ends the speech with an
// error (WaitSpoken). It fails if the Supervisor ends such an exchange with
// no log line and no error sound, as the Pi did ("exchange ended (done)"),
// or plays the error sound twice when the voice reported the failure too; or
// if an answer that played to its end gets the error sound or loses its
// follow-up window.
func TestAnAnswerCutWithNoStopIsMetWithTheErrorSound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spokenErr error
		reported  bool
		sounds    int
	}{
		{name: "control: the answer played to its end", sounds: 0},
		{name: "the speech was cut", spokenErr: errSpeechCut, sounds: 1},
		{name: "the speech was cut and the voice reported it", spokenErr: errSpeechCut, reported: true, sounds: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var logged []string
			r := newRigWith(t, newFakeConcierge("The voice issue is in review."), func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) {
					return cutSpeechVoice{r: r, spokenErr: tc.spokenErr, reported: tc.reported}, nil
				}
				cfg.Logf = func(format string, args ...any) {
					line := fmt.Sprintf(format, args...)
					t.Log(line)
					mu.Lock()
					logged = append(logged, line)
					mu.Unlock()
				}
			})
			sess := r.ask("what is happening with the voice issue?")
			eventually(t, "the exchange ended", func() bool { _, _, closed := sess.state(); return closed })
			time.Sleep(100 * time.Millisecond) // an error sound scheduled late would come now
			sounds := 0
			for _, event := range r.timingEvents() {
				if event == "error_sound" {
					sounds++
				}
			}
			if sounds != tc.sounds {
				t.Errorf("the error sound played %d times, want %d", sounds, tc.sounds)
			}
			mu.Lock()
			defer mu.Unlock()
			said, followUp := false, false
			for _, line := range logged {
				said = said || strings.HasPrefix(line, "voice: the answer did not play to its end: "+errSpeechCut.Error())
				followUp = followUp || line == "voice: exchange ended (no follow-up)"
			}
			if cut := tc.spokenErr != nil; said != cut {
				t.Errorf("logged that the answer did not play to its end: %v, want %v; log %q", said, cut, logged)
			}
			if wantFollowUp := tc.spokenErr == nil; followUp != wantFollowUp {
				t.Errorf("the exchange waited for a follow-up: %v, want %v; log %q", followUp, wantFollowUp, logged)
			}
		})
	}
}
