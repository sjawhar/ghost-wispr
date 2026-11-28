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

// TestSolFollowup195DelayedTranscript fails if a speech of GPT-Live's that
// the speaker reports ended before the Supervisor read its transcript ("What
// is its key?") is judged without it: Wispr's own question is then lost, and
// the room's "ABC-12" ends the exchange as side talk in the follow-up window
// instead of reaching the Concierge as the reply. The Supervisor reads a
// session's events on a goroutine of its own, and nothing orders that read
// before the speaker's report. The control lets the read happen before the
// speech ends. (Sol's review of #195 at 451690d6; the bug of INFRA-4592.)
func TestSolFollowup195DelayedTranscript(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "transcript read before the end control"
		if delayed {
			name = "speaker ends before the transcript is read"
		}
		t.Run(name, func(t *testing.T) {
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
			read := func() {
				deadline := time.Now().Add(3 * time.Second)
				for v.starts.Load() <= starts {
					if time.Now().After(deadline) {
						t.Fatal("the Supervisor never read the held transcript and the speech's start")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if !delayed {
				unblock()
				read()
			}
			// Longer than the speaker's 100 ms hold and GPT-Live's 400 ms
			// gap: time played is no order of the Supervisor's reads.
			time.Sleep(550 * time.Millisecond)
			r.speaker.FlushSpeech()
			r.speaker.eventMu.Lock()
			owner := r.speaker.active.owner
			r.speaker.active = nil
			r.speaker.queue = append(r.speaker.queue, audio.SpeechEvent{Input: audio.InputVoice, Owner: owner, At: time.Now()})
			r.speaker.eventMu.Unlock()
			r.speaker.FlushSpeech()
			if delayed {
				unblock()
				read()
			}
			r.hearNow("ABC-12", true)
			sendDelegation(w, "reply", 2000)
			within(t, v.calls, "reply call")
			select {
			case reply := <-capture.calls:
				if reply.x.VoiceAsked != opening {
					t.Fatalf("VoiceAsked = %q, want %q", reply.x.VoiceAsked, opening)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the bare key reply to Wispr's question never reached the Concierge")
			}
		})
	}
}
