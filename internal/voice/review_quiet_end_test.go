package voice

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

func TestReviewQuietEndAfterCompletedDriverSpeech(t *testing.T) {
	for _, ending := range []string{"side talk", "holder takes floor"} {
		t.Run(ending, func(t *testing.T) {
			c := newFakeConcierge("It ships tomorrow.")
			r := newRigWith(t, c, func(cfg *SupervisorConfig, r *rig) {
				cfg.Voice = func() (Voice, error) { return drivingVoice{r.voice}, nil }
				cfg.IdleWait, cfg.FollowUpWait = time.Minute, time.Minute
			})
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			sess.mu.Lock()
			sess.endLate = time.Second
			sess.mu.Unlock()
			speakAs(t, sess, "The issue is in review since this morning.")
			r.waitSpeaking()
			r.speaker.finish(audio.InputVoice)
			eventually(t, "the driver's follow-up window", func() bool {
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				return r.s.ex != nil && r.s.ex.followUpTimer != nil
			})
			if ending == "side talk" {
				r.heard(sideTalk, true, true)
				out, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "review-side", Name: AnswerTool, Args: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var result AnswerResult
				if err := json.Unmarshal(out, &result); err != nil || result.Say != "" {
					t.Fatalf("side talk result %s, error %v", out, err)
				}
			} else {
				c.answers <- PendingAnswer{TaskID: "review-holder", SessionID: "sess-1", IssueKey: "ABC-12", Text: "It ships tomorrow.", AskedAt: time.Now()}
				next := r.waitSession()
				if got := waitSaid(t, next); got != "It ships tomorrow." {
					t.Fatalf("holder said %q", got)
				}
			}
			eventually(t, "the old session closed", func() bool { _, _, closed := sess.state(); return closed })
			r.waitEvent("voice_spoken:sess-1")
		})
	}
}

func TestReviewQuietEndHasNoDuplicateOrUnheardSpoken(t *testing.T) {
	for _, speak := range []bool{false, true} {
		t.Run(map[bool]string{false: "never spoke", true: "vendor reported first"}[speak], func(t *testing.T) {
			r := newDrivingRig(t, 300*time.Millisecond)
			r.heard("Wispr, what is happening?", true, true)
			sess := r.waitSession()
			if speak {
				speakAs(t, sess, "The issue is in review since this morning.")
				r.waitSpeaking()
				r.speaker.finish(audio.InputVoice)
				r.waitEvent("voice_spoken:sess-1")
			}
			eventually(t, "the idle session closed", func() bool { _, _, closed := sess.state(); return closed })
			time.Sleep(50 * time.Millisecond)
			count := 0
			for _, event := range r.events.all() {
				if event == "voice_spoken:sess-1" {
					count++
				}
			}
			want := 0
			if speak {
				want = 1
			}
			if count != want {
				t.Fatalf("voice_spoken count %d, want %d; events %v", count, want, r.events.all())
			}
			if !slices.Contains(r.events.all(), "voice_exchange_started:sess-1") {
				t.Fatal("probe saw no exchange")
			}
		})
	}
}
