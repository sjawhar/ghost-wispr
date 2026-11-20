package voice

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestADriverVoiceGetsTheAskerTurnAtTheWake fails if a voice that drives its
// own turn opens with no AskerTurn, or its answer tool asks the Concierge an
// empty question when the vendor delegates before its own transcript lands.
func TestADriverVoiceGetsTheAskerTurnAtTheWake(t *testing.T) {
	r := newDrivingRig(t, time.Minute)
	r.heard("Wispr, what is happening with the voice issue?", true, true)
	sess := r.waitSession()
	if got := sess.x.AskerTurn; got != "what is happening with the voice issue?" {
		t.Fatalf("AskerTurn at open = %q, want the words after the wake phrase", got)
	}
	if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":""}`)}); err != nil {
		t.Fatalf("answer tool: %v", err)
	}
	if got := r.waitTurn(); got != "what is happening with the voice issue?" {
		t.Fatalf("the answer tool asked %q, want the asker's turn", got)
	}
}

// TestAHeldAnswerWhoseSpeechFailsStaysPending fails if a holder's answer that
// the voice could not speak is dropped instead of waiting for an invitation.
func TestAHeldAnswerWhoseSpeechFailsStaysPending(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	r.voice.failSays(errors.New("the vendor stayed silent"))
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	failed := r.waitSession() // the answer's own exchange, after the chime and the silence
	eventually(t, "the failed exchange ended", func() bool { _, _, closed := failed.state(); return closed })
	if got := c.spokenIDs(); len(got) != 0 {
		t.Fatalf("a held answer that was never heard was recorded as spoken: %v", got)
	}

	r.voice.failSays(nil)
	never(t, "a failed answer spoke again with no invitation", 500*time.Millisecond, func() bool { return len(r.voice.opened) > 0 })
	r.heard("Wispr, go ahead.", true, true)
	answer := r.waitSession()
	if got := waitSaid(t, answer); got != "It ships tomorrow." {
		t.Fatalf("said %q", got)
	}
	r.speaker.finish(audio.InputVoice)
	eventually(t, "the answer recorded as spoken", func() bool { return len(c.spokenIDs()) == 1 })
}
