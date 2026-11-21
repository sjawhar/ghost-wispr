package voice

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// TestAMisheardWakeReadsNothing fails if the interim results of a sentence
// that only starts with the name, not as a call, reach the Concierge's
// read-ahead (a Dispatch search) before the final result ends the exchange.
func TestAMisheardWakeReadsNothing(t *testing.T) {
	c := newFakeConcierge("fact")
	r := newRig(t, c)
	r.heard("Whisper", false, false)
	sess := r.waitSession()
	r.heard("Whisper networks are", false, false)
	r.heard("Whisper networks are a problem.", true, true)
	eventually(t, "the misheard wake's exchange ended", func() bool { _, _, closed := sess.state(); return closed })
	for _, h := range c.heardCalls() {
		if strings.Contains(h.words, "networks") {
			t.Fatalf("the Concierge read for %q, a sentence nobody addressed to Wispr", h.words)
		}
	}
}

// TestAnswersThatLandTogetherGetOneChime fails if two held answers that land
// together get two answer chimes, which would overlap on one input.
func TestAnswersThatLandTogetherGetOneChime(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	r.setRed(false)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "One."}
	c.answers <- PendingAnswer{TaskID: "t2", SessionID: "sess-1", Text: "Two."}
	r.waitEvent("voice_answer_ready:sess-1")
	r.waitEvent("voice_answer_ready:sess-1")
	r.setRed(true)
	frames := (len(AnswerChime()) + 959) / 960
	eventually(t, "the answer chime", func() bool { return r.speaker.written(audio.InputChime) >= frames })
	time.Sleep(600 * time.Millisecond) // two more offers
	if got := r.speaker.written(audio.InputChime); got != frames {
		t.Fatalf("%d chime frames written, want one chime of %d", got, frames)
	}
}

// TestAnEndedSessionsAnswerStopsWhileSpeaking fails if a held answer already
// speaking when its session ends goes on: an ended meeting's answers are never
// spoken.
func TestAnEndedSessionsAnswerStopsWhileSpeaking(t *testing.T) {
	c := newFakeConcierge()
	r := newRig(t, c)
	c.answers <- PendingAnswer{TaskID: "t1", SessionID: "sess-1", Text: "It ships tomorrow.", AskedAt: time.Now()}
	answer := r.waitSession()
	waitSaid(t, answer)
	r.s.SessionEnded("sess-1")
	eventually(t, "the answer's exchange ended", func() bool { _, _, closed := answer.state(); return closed })
	if _, ok := r.speaker.cancelledAt(audio.InputVoice); !ok {
		t.Error("the answer's speech was not cut")
	}
}

// TestExchangePreemptsTTSSpeak fails if other speech (POST /api/tts/speak)
// begun through WithFloor can begin while an exchange holds the floor, or
// survives an exchange that takes the floor after it began.
func TestExchangePreemptsTTSSpeak(t *testing.T) {
	r := newRig(t, newFakeConcierge("fact"))
	var pb Playback
	if err := r.s.WithFloor(t.Context(), func() { pb = r.speaker.Begin(audio.InputVoice, nil) }); err != nil {
		t.Fatal(err)
	}
	r.heard("Wispr,", false, false) // a wake takes the floor
	r.waitSession()
	if err := pb.Write(t.Context(), make([]byte, 960), audio.AudioFormat{SampleRate: 24000, Channels: 1}); err == nil {
		t.Fatal("speech begun before the wake still plays")
	}
	started := make(chan struct{})
	go func() {
		_ = r.s.WithFloor(t.Context(), func() { close(started) })
	}()
	select {
	case <-started:
		t.Fatal("speech began while an exchange held the floor")
	case <-time.After(200 * time.Millisecond):
	}
}
