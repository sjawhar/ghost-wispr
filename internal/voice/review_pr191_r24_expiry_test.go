// Sol's twenty-fourth review of #191 at 12c293e
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6084899163):
// Sol's probes, kept as regression tests, unchanged: the exact two-minute expiry.

package voice

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestSol191R24ExactVoiceAskExpiry(t *testing.T) {
	for _, age := range []time.Duration{2*time.Minute - time.Nanosecond, 2 * time.Minute, 2*time.Minute + time.Nanosecond} {
		t.Run(age.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if recentKept != 2*time.Minute {
					t.Fatalf("not the production retention: %v", recentKept)
				}
				s := &Supervisor{}
				ex := &exchange{askedBack: true, voiceAsked: true, voiceAskedAbout: "why is the recorder blocked?", voiceAskedAt: time.Now()}
				time.Sleep(age)
				if got := time.Since(ex.voiceAskedAt); got != age {
					t.Fatalf("clock control: got %v want %v", got, age)
				}
				s.voiceAskExpiredLocked(ex)
				want := age < 2*time.Minute
				if ex.askedBack != want || ex.voiceAsked != want {
					t.Errorf("age=%v askedBack=%v voiceAsked=%v want=%v", age, ex.askedBack, ex.voiceAsked, want)
				}
				if !want && (ex.voiceAskedAbout != "" || !ex.voiceAskedAt.IsZero()) {
					t.Errorf("expired ownership survived: about=%q at=%v", ex.voiceAskedAbout, ex.voiceAskedAt)
				}
			})
		})
	}
}

func TestSol191R24ConciergeAskIsNotVoiceExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Supervisor{}
		ex := &exchange{askedBack: true, voiceAsked: false}
		time.Sleep(2 * time.Minute)
		s.voiceAskExpiredLocked(ex)
		if !ex.askedBack {
			t.Fatal("voice expiry cleared the Concierge's own question")
		}
	})
}
