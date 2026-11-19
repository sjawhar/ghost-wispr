package gptlive

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// B3: the Supervisor opens a session with up to two seconds of the room's
// audio at once, the wake replay. A server whose timeline counts input audio
// is then two seconds ahead of the wall clock. The session's clock counts the
// input audio it sent too, so a cut speech's words still match it.
func TestACutSpeechsWordsMatchItAfterTheWakeReplay(t *testing.T) {
	out := newFakeOutput()
	x := testExchange(out, newFakeTools(voice.AnswerResult{}))
	tap := make(chan audio.Frame, 16)
	for range 8 { // 2 s, held while the session starts and then sent at once
		tap <- audio.Frame{PCM: make([]int16, 4000), Rate: 16000, Channels: 1}
	}
	x.Tap = tap
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)
	deadline := time.Now().Add(testWait)
	for c.inSamples.Load() < 2*SampleRate-1 {
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in received %d samples of the replay, want 2 s", c.inSamples.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	at := c.inputMs()
	c.audioDelta(ramp(1000, 2400)) // 100 ms, which plays
	out.nextSpeech(t).waitSamples(t, 2400)
	s.Interrupt()
	c.audioDelta(ramp(1000, 2400)) // 100 ms more, which does not
	c.saidAt("words nobody heard", at+150)
	time.Sleep(speechGap + 200*time.Millisecond)
	c.speak("Sure.")
	got := agentTexts(t, s, "Sure.")
	if slices.Contains(got, "words nobody heard") {
		t.Fatalf("agent text %q: the cut speech's words reached the own-speech feed after a wake replay", got)
	}
}

// B1: a server can send a speech's audio, and all its words, faster than the
// speech plays. Only the words whose audio played before Interrupt are
// Wispr's; and the next speech's words, sent before its audio, are Wispr's
// once it plays.
func TestOnlyWordsWhoseAudioPlayedAreWisprs(t *testing.T) {
	out := newFakeOutput()
	out.pace = true
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	at := c.inputMs()
	c.audioDelta(ramp(1000, 2*SampleRate)) // 2 s at once
	for i := range 5 {
		c.saidAt(fmt.Sprintf("w%d", i), at+int64(400*i))
	}
	p := out.nextSpeech(t)
	p.waitSamples(t, SampleRate/2) // 500 ms played
	s.Interrupt()

	time.Sleep(speechGap + 200*time.Millisecond)
	next := c.inputMs() + 100
	c.saidAt("b0", next) // before its audio
	time.Sleep(100 * time.Millisecond)
	c.audioDelta(ramp(5000, 2400))
	got := agentTexts(t, s, "b0")
	if fmt.Sprint(got) != "[w0 w1 b0]" {
		t.Fatalf("agent text %q, want [w0 w1 b0]: the words whose audio played, and the next speech's", got)
	}
}
