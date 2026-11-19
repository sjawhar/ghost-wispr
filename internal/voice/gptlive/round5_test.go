package gptlive

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// streamSpeech sends d of the model's speech the way the real GPT-Live
// does, as measured from the office unit: 100 ms of audio every 100 ms. Each
// of words goes out just before the audio it is due with, as GPT-Live sends
// its text ahead of its audio. It stops early when stop closes.
func streamSpeech(c *liveConn, d time.Duration, words map[int]string, stop <-chan struct{}) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for i := range int(d / (100 * time.Millisecond)) {
		if w, ok := words[i]; ok {
			c.said(w)
		}
		c.audioDelta(ramp(1000+i, SampleRate/10))
		select {
		case <-tick.C:
		case <-stop:
			return
		}
	}
}

// The real GPT-Live sends a speech's words just before their audio, and the
// session takes each word as it arrives:
//   - Words that arrived before a barge-in count, though their audio may not
//     have played. This is the measured, accepted limit: up to about 1.4 s
//     of an interrupted answer.
//   - Words that arrive after the barge-in, while the cut speech goes on,
//     do not count.
//   - The next speech's words count, even when they come before its audio.
func TestWordsCountAsTheyArriveUntilABargeIn(t *testing.T) {
	out := newFakeOutput()
	out.pace = true
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(out, newFakeTools(voice.AnswerResult{})))

	cut := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 2 s of speech; its first three words go out ahead of their audio,
		// and one more after the barge-in, before the server stops.
		streamSpeech(c, 2*time.Second, map[int]string{0: "w0", 1: "w1", 2: "w2", 8: "after the cut"}, cut)
	}()
	p := out.nextSpeech(t)
	p.waitSamples(t, SampleRate/2) // 500 ms played
	s.Interrupt()
	time.Sleep(400 * time.Millisecond) // the server goes on 0.4 s, past the word at 0.8 s
	close(cut)
	<-done

	time.Sleep(speechGap + 200*time.Millisecond)
	streamSpeech(c, 300*time.Millisecond, map[int]string{0: "b0"}, nil)
	got := agentTexts(t, s, "b0")
	if fmt.Sprint(got) != "[w0 w1 w2 b0]" {
		t.Fatalf("agent text %q, want [w0 w1 w2 b0]: the words that arrived before the barge-in, and the next speech's", got)
	}
}
