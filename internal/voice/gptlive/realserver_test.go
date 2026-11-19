package gptlive

import (
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// realServerMs is the real GPT-Live's session timeline, as measured from the
// office unit on 2026-11-19 (three sessions): it runs about 1.5 times real
// time, in 200 ms steps, and matches no clock the session holds. Its
// transcript start_ms, its commentary acknowledgements and its usage seconds
// all follow it.
func realServerMs(c *liveConn) int64 {
	ms := c.sessionMs()*3/2 + 1500
	return ms - ms%200
}

// agentTextWithin waits up to d for the session's next agent text.
func agentTextWithin(t *testing.T, s voice.Session, d time.Duration) (string, bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("Events closed")
			}
			if ev.Kind == voice.EventAgentText {
				return ev.Text, true
			}
		case <-deadline:
			return "", false
		}
	}
}

// The real GPT-Live sends each fragment of its words just before their
// audio, in order, stamped on a timeline the session cannot follow. Each word
// must reach the own-speech feed by the time its audio starts, or the
// Supervisor takes Wispr's own echo for a person talking over it.
func TestWordsReachTheOwnSpeechFeedAsTheRealServerSendsThem(t *testing.T) {
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))

	for i, word := range []string{"one,", " two,", " three,", " four,", " five."} {
		c.saidAt(word, realServerMs(c))
		sent := time.Now()
		c.audioDelta(ramp(1000+100*i, SampleRate*3/10)) // its 300 ms of audio, after it
		got, ok := agentTextWithin(t, s, 300*time.Millisecond)
		if !ok || got != word {
			t.Fatalf("word %d: agent text %q (%v) within 300 ms of its fragment, want %q", i, got, ok, word)
		}
		if took := time.Since(sent); took > 300*time.Millisecond {
			t.Fatalf("word %d reached the feed %v after its fragment, after its audio began", i, took)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
