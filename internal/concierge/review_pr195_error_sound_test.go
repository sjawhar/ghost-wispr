package concierge

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// errorSoundRecord is what a Supervisor logged and timed: its log lines and
// its error sounds.
type errorSoundRecord struct {
	mu     sync.Mutex
	lines  []string
	errors int
}

func (r *errorSoundRecord) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *errorSoundRecord) timing(event, _ string, _ monoclock.Stamp, _ ...any) {
	if event == "error_sound" {
		r.mu.Lock()
		r.errors++
		r.mu.Unlock()
	}
}

func (r *errorSoundRecord) read() (lines []string, errors int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...), r.errors
}

// TestAnAnswerInItsAcknowledgementsSpeechGetsNoErrorSound fails if the
// meeting voice plays the error sound after GPT-Live spoke an answer in the
// speech its own acknowledgement began, or plays none when GPT-Live played
// nothing after the answer came. Through the real GPT-Live session on a
// loopback socket and the real Supervisor and Concierge, at production's
// ratio of the follow-up window to the idle wait (6 s to 10 s, here 600 ms to
// 1 s). GPT-Live's audio comes in real time, 100 ms each 100 ms, as measured
// from the office unit; its acknowledgement plays from before the answer
// tool runs, and goes on into the answer with no gap, in one playback, as
// GPT-Live's player keeps one while its audio goes on. Sol's review of #195
// at 75c3253 said the idle wait then plays the error sound: with nobody
// speaking it does not, as the speech's end opens the follow-up window, which
// ends the exchange first; but a person who speaks after the answer closes
// that window, and the idle wait then played the error sound for an answer
// that played (INFRA-4592). The error sound now plays only when no audio of
// the voice played after the answer came.
func TestAnAnswerInItsAcknowledgementsSpeechGetsNoErrorSound(t *testing.T) {
	for _, tc := range []struct {
		name string
		// merged: GPT-Live's audio goes on from its acknowledgement into the
		// answer; else its speech ends before the answer comes, and nothing
		// plays after.
		merged   bool
		roomTalk bool // a person speaks, no wake phrase, after the speech
		sound    bool // the error sound plays
		ends     string
	}{
		{"one speech, nobody speaks after", true, false, false, "no follow-up"},
		{"one speech, a person speaks after", true, true, false, "idle"},
		{"nothing played after the answer control", false, false, true, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, conns := loopbackVendor(t, false)
			live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			v := watch(live)
			r := &cutRig{rig: newRig(t), speaker: &cutSpeaker{}}
			t.Cleanup(r.concierge.Stop)
			// The Concierge's answer waits until the acknowledgement plays.
			hold := make(chan struct{})
			var holdOnce sync.Once
			releaseAnswer := func() { holdOnce.Do(func() { close(hold) }) }
			t.Cleanup(releaseAnswer)
			capture := &sol23Capture{Concierge: r.concierge, calls: make(chan sol23Snapshot, 20), holdFirst: hold}
			record := &errorSoundRecord{}
			r.sup = voice.NewSupervisor(&voice.SupervisorConfig{
				Voice: func() (voice.Voice, error) { return v, nil }, Concierge: capture,
				Speaker: r.speaker, WakePhrases: []string{"Wispr"}, TakeBack: TakeBack,
				Red: func() bool { return true }, Session: func() string { return "meeting-1" },
				IdleWait: time.Second, FollowUpWait: 600 * time.Millisecond,
				Logf: record.logf, Timing: record.timing,
			})
			t.Cleanup(r.sup.Close)
			heldIssue(r.rig, "none", false)
			r.model.turns = answers(20)
			r.hearNow("Wispr, what is happening with ABC-12?", true)
			w := within(t, conns, "vendor connection")
			w.next(t, "session.start")
			within(t, v.opened, "opened voice")
			// GPT-Live delegates, and its own acknowledgement plays while
			// the Concierge answers.
			sendDelegation(w, "ask", 1000)
			within(t, v.calls, "the answer tool's call")
			within(t, capture.calls, "the Concierge's answer")

			pcm := make([]byte, 2*2400) // 100 ms of audible audio
			for i := 0; i < len(pcm); i += 2 {
				binary.LittleEndian.PutUint16(pcm[i:], 4000)
			}
			delta := map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)}
			stream := make(chan struct{})
			streaming := make(chan struct{})
			var stopOnce sync.Once
			stopStream := func() {
				stopOnce.Do(func() { close(stream) })
				<-streaming
			}
			t.Cleanup(stopStream)
			go func() { // GPT-Live's acknowledgement, in real time
				defer close(streaming)
				tick := time.NewTicker(100 * time.Millisecond)
				defer tick.Stop()
				for {
					w.send(delta)
					select {
					case <-tick.C:
					case <-stream:
						return
					}
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				r.speaker.eventMu.Lock()
				active := r.speaker.active != nil
				r.speaker.eventMu.Unlock()
				if active {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("GPT-Live's acknowledgement never reached the speaker")
				}
				time.Sleep(5 * time.Millisecond)
			}
			r.speaker.FlushSpeech() // its start
			endSpeech := func() {
				r.speaker.eventMu.Lock()
				owner := r.speaker.active.owner
				r.speaker.active = nil
				r.speaker.queue = append(r.speaker.queue, audio.SpeechEvent{Input: audio.InputVoice, Owner: owner, At: time.Now()})
				r.speaker.eventMu.Unlock()
				r.speaker.FlushSpeech()
			}
			if !tc.merged {
				stopStream()
				time.Sleep(150 * time.Millisecond)
				endSpeech() // the acknowledgement ended before the answer came
			}
			releaseAnswer()
			result := within(t, v.results, "the answer tool's result")
			if !strings.Contains(string(result), "ABC-12") {
				t.Fatalf("control: the answer tool returned %s, want an answer about ABC-12", result)
			}
			if tc.merged {
				time.Sleep(500 * time.Millisecond) // the answer, in the same playback
				stopStream()
				time.Sleep(150 * time.Millisecond)
				endSpeech()
			}
			if tc.roomTalk {
				time.Sleep(100 * time.Millisecond)
				r.hearNow("okay so the budget looks fine", true)
			}
			deadline = time.Now().Add(5 * time.Second)
			for {
				lines, _ := record.read()
				if slicesContainsSub(lines, "exchange ended") || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond) // a sound the end queues
			lines, sounds := record.read()
			if !slicesContainsSub(lines, "exchange ended ("+tc.ends+")") {
				t.Errorf("the exchange did not end by %q; log:\n%s", tc.ends, strings.Join(lines, "\n"))
			}
			if (sounds > 0) != tc.sound {
				t.Errorf("error sounds %d, want one only when nothing played after the answer; log:\n%s", sounds, strings.Join(lines, "\n"))
			}
		})
	}
}

// slicesContainsSub reports whether a line of lines holds sub.
func slicesContainsSub(lines []string, sub string) bool {
	for _, line := range lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}
