package concierge

import (
	"encoding/base64"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestSolFinal195ReplyDuringDeferredJudgment fails if a reply to GPT-Live's
// own question ("What is its key?") is discarded as side talk because its
// answer tool ran before the Supervisor read the question's transcript: the
// speech is judged once its events are read (voiceSpokeLocked), and the reply
// must not be classified before that judgment. The controls read the
// transcript before the reply, or after the reply but before its tool runs.
// (Sol's review of #195 at 75c3253.)
func TestSolFinal195ReplyDuringDeferredJudgment(t *testing.T) {
	for _, phase := range []string{"read before the reply control", "reply before the read control", "tool before the read"} {
		t.Run(phase, func(t *testing.T) {
			initial := make(chan struct{})
			defer close(initial)
			const opening = "why is the recorder blocked?"
			r, v, w, capture := sol23Open(t, opening, initial)
			entered, release := make(chan struct{}), make(chan struct{})
			var holdOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			hold := func(e voice.Event) {
				if e.Kind == voice.EventAgentText {
					holdOnce.Do(func() { close(entered); <-release })
				}
			}
			v.beforeEvent.Store(&hold)
			t.Cleanup(func() { v.beforeEvent.Store(nil) })
			sendDelegation(w, "initial", 1000)
			within(t, v.calls, "initial call")
			within(t, capture.calls, "initial answer entry")
			starts := v.starts.Load()
			w.send(map[string]any{"type": "session.output_transcript.delta", "delta": "What is its key?"})
			within(t, entered, "the held read of the transcript")
			pcm := make([]byte, 2*2400)
			for i := 0; i < len(pcm); i += 2 {
				binary.LittleEndian.PutUint16(pcm[i:], 4000)
			}
			w.send(map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)})
			deadline := time.Now().Add(3 * time.Second)
			for {
				r.speaker.eventMu.Lock()
				active := r.speaker.active != nil
				r.speaker.eventMu.Unlock()
				if active {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("GPT-Live's audio never reached the speaker")
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(550 * time.Millisecond)
			r.speaker.FlushSpeech()
			r.speaker.eventMu.Lock()
			owner := r.speaker.active.owner
			r.speaker.active = nil
			r.speaker.queue = append(r.speaker.queue, audio.SpeechEvent{Input: audio.InputVoice, Owner: owner, At: time.Now()})
			r.speaker.eventMu.Unlock()
			r.speaker.FlushSpeech()
			read := func() {
				unblock()
				deadline := time.Now().Add(3 * time.Second)
				for v.starts.Load() <= starts {
					if time.Now().After(deadline) {
						t.Fatal("the Supervisor never read the held transcript and the speech's start")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if phase == "read before the reply control" {
				read()
			}
			r.hearNow("ABC-12", true)
			if phase == "reply before the read control" {
				read()
			}
			sendDelegation(w, "reply", 2000)
			within(t, v.calls, "reply call")
			if phase == "tool before the read" {
				select {
				case <-v.results:
					t.Log("the answer tool returned before the speech was judged")
				case <-time.After(150 * time.Millisecond):
				}
				read()
			}
			select {
			case reply := <-capture.calls:
				if reply.x.VoiceAsked != opening {
					t.Fatalf("VoiceAsked = %q, want %q", reply.x.VoiceAsked, opening)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the reply to GPT-Live's question never reached the Concierge")
			}
		})
	}
}
