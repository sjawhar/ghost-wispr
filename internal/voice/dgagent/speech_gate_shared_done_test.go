package dgagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// silentConcierge never answers: after a UserStartedSpeaking, the model's
// own words answer the room's turn, and the session asks the Concierge
// instead. These tests are about what plays, so the answer never comes.
func silentConcierge() tools {
	return func(ctx context.Context, _ voice.ToolCall) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// sharedSentence is one sentence of a Deepgram reply whose sentences share a
// single AgentAudioDone, such as an injection and the model's own words after
// it. textFirst says whether its text comes before its audio.
type sharedSentence struct {
	textFirst bool
}

type sharedEvent struct {
	sentence int // -1 for AgentAudioDone and UserStartedSpeaking
	kind     replyEvent
}

// TestSpeechGateSharedDoneReplies covers one Deepgram reply of one to three
// sentences that ends with a single AgentAudioDone: an injection alone, or
// with the model's own sentences before or after it. Each sentence's text
// may come before or after its own audio, and the last sentence's text may
// come after the AgentAudioDone. A sentence's text never comes after the
// next sentence's audio: no message names its sentence, so nothing could
// tell such audio apart. Each order also runs with a UserStartedSpeaking
// between any two of its events. Sol's review of 65a0c06 found the model's
// audio after an injection, before the model's text, playing; its review
// of 1bd9863 found a UserStartedSpeaking between them playing it.
//
// Whatever the order, no model byte reaches Output, and a Say returns nil
// only once Output has accepted all of its bytes; else it returns an error.
// An injection alone, with no UserStartedSpeaking, plays whole in every
// order. Each order has its own session and runs beside the others.
func TestSpeechGateSharedDoneReplies(t *testing.T) {
	for sentences := 1; sentences <= 3; sentences++ {
		for injected := -1; injected < sentences; injected++ {
			if sentences == 1 && injected < 0 {
				continue
			}
			for _, trace := range everySharedDoneTrace(sentences) {
				name := fmt.Sprintf("%d-sentences/injected-%d/%s", sentences, injected+1, sharedTraceName(trace, injected))
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					runSharedDoneTrace(t, trace, sentences, injected, sentences == 1)
				})
				for at := 1; at < len(trace); at++ {
					cut := make([]sharedEvent, 0, len(trace)+1)
					cut = append(cut, trace[:at]...)
					cut = append(cut, sharedEvent{-1, replyUserStart})
					cut = append(cut, trace[at:]...)
					name := fmt.Sprintf("%d-sentences/injected-%d/%s", sentences, injected+1, sharedTraceName(cut, injected))
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						runSharedDoneTrace(t, cut, sentences, injected, false)
					})
				}
			}
		}
	}
	t.Run("sol-round-6/model-audio-before-its-text-after-an-injection", func(t *testing.T) {
		runSharedDoneTrace(t, []sharedEvent{
			{0, replyText}, {0, replyAudio}, {1, replyAudio}, {1, replyText}, {-1, replyDone},
		}, 2, 0, false)
	})
	t.Run("injection-plays-whole/text-first", func(t *testing.T) {
		runSharedDoneTrace(t, []sharedEvent{{0, replyText}, {0, replyAudio}, {-1, replyDone}}, 1, 0, true)
	})
	t.Run("injection-plays-whole/audio-first", func(t *testing.T) {
		runSharedDoneTrace(t, []sharedEvent{{0, replyAudio}, {0, replyText}, {-1, replyDone}}, 1, 0, true)
	})
	for _, userStart := range []bool{true, false} {
		name := "sol-round-7/model-audio-then-its-text-over-played-speech"
		if userStart {
			name = "sol-round-7/user-start-between-model-audio-and-its-text-over-played-speech"
		}
		t.Run(name, func(t *testing.T) { testUserStartOverPlayedSpeech(t, userStart) })
	}
	t.Run("user-start-over-the-voice-drops-the-audio-that-waits", testUserStartOverTheVoiceDropsHeldAudio)
}

// testUserStartOverTheVoiceDropsHeldAudio fails if a UserStartedSpeaking
// while the voice speaks plays the injection's frames that still wait, or
// reports its Say spoken: someone talks over the voice, which the Supervisor
// stops, and those frames have not played. No model text comes, so only
// the hold could tell them from the model's own words.
func testUserStartOverTheVoiceDropsHeldAudio(t *testing.T) {
	stand := startStandIn(t)
	out := newFakeOutput()
	// The production speech gap ends speech that stops with no end, so a
	// Say whose speech was not cut returns, and the check below reads it.
	sess, c, _ := open(t, stand, Config{speechGap: speechGap}, voice.Exchange{Out: out, Tools: silentConcierge()})
	defer func() { _ = sess.Close() }()
	first := tone(OutputRate, 20*time.Millisecond, 810)
	done := sayAsync(sess, "Cut words.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Cut words."})
	c.sendAudio(first)
	out.nextPlayback(t).waitBytes(t, len(first)) // past the hold: the voice speaks
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 811))
	c.send(map[string]any{"type": "UserStartedSpeaking"})
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Say = %v, want ErrInterrupted: the room spoke over it before all of it played", err)
	}
	barrier := sayAsync(sess, "Ordering barrier.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ordering barrier."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 900))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, barrier); err != nil {
		t.Fatalf("ordering barrier = %v", err)
	}
	if got := outputSamples(t, out); got[811] != 0 || got[810] != len(first) {
		t.Fatalf("waiting/played bytes at Output = %d/%d, want 0/%d", got[811], got[810], len(first))
	}
}

// testUserStartOverPlayedSpeech is Sol's order from the review of 1bd9863:
// an injection whose audio has passed the hold and plays, then the model's
// audio, a UserStartedSpeaking, and 20 ms later the model's text. The model's
// audio still waits when the room speaks over the voice, so it never plays,
// and the injection's audio that played stays played. Without the
// UserStartedSpeaking, the model's text drops the model's audio.
func testUserStartOverPlayedSpeech(t *testing.T, userStart bool) {
	stand := startStandIn(t)
	out := newFakeOutput()
	// The production speech gap ends speech that stops with no end, so the
	// Say returns and the byte check below runs even when the model's audio
	// plays and nothing else ends the reply.
	sess, c, _ := open(t, stand, Config{speechGap: speechGap}, voice.Exchange{Out: out, Tools: silentConcierge()})
	defer func() { _ = sess.Close() }()
	approved := tone(OutputRate, 40*time.Millisecond, 800)
	done := sayAsync(sess, "Approved words.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Approved words."})
	c.sendAudio(approved)
	out.nextPlayback(t).waitBytes(t, len(approved)) // past the hold: the voice speaks
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, -800))
	if userStart {
		c.send(map[string]any{"type": "UserStartedSpeaking"})
	}
	time.Sleep(20 * time.Millisecond)
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Model words."})
	c.send(map[string]any{"type": "AgentAudioDone"})
	err := returned(t, done)
	if !userStart && err != nil {
		t.Fatalf("Say = %v, want nil: its words played whole", err)
	}
	barrier := sayAsync(sess, "Ordering barrier.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ordering barrier."})
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 900))
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, barrier); err != nil {
		t.Fatalf("ordering barrier = %v", err)
	}
	got := outputSamples(t, out)
	if got[-800] != 0 || got[800] != len(approved) {
		t.Fatalf("model/approved bytes at Output = %d/%d, want 0/%d", got[-800], got[800], len(approved))
	}
}

// TestAnInjectionKeepsItsSpacingThroughTheHold fails if the hold on an
// injection's audio splits it: its first frame, which Deepgram sends just
// before the text, playing at once and the rest holdTail later, so the room
// hears a click and a gap. Deepgram sends a reply's audio in bursts faster
// than it plays (the probe's frame log), so its frames reach Output close
// together, after the hold, in both orders of the text and the first frame.
func TestAnInjectionKeepsItsSpacingThroughTheHold(t *testing.T) {
	for _, audioFirst := range []bool{true, false} {
		name := "text-first"
		if audioFirst {
			name = "audio-first"
		}
		t.Run(name, func(t *testing.T) {
			const hold = 200 * time.Millisecond
			setSpeechGap(t, time.Minute)
			stand := startStandIn(t)
			out := &timedOutput{}
			sess, c, _ := open(t, stand, Config{holdTail: hold}, voice.Exchange{Out: out, Tools: tools(nil)})
			defer func() { _ = sess.Close() }()
			done := sayAsync(sess, "Spaced reply.")
			c.expect(t, "InjectAgentMessage")
			frame := tone(OutputRate, 20*time.Millisecond, 700)
			sent := time.Now()
			if audioFirst {
				c.sendAudio(frame)
			}
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Spaced reply."})
			if !audioFirst {
				c.sendAudio(frame)
			}
			for range 4 {
				c.sendAudio(frame)
			}
			c.send(map[string]any{"type": "AgentAudioDone"})
			if err := returned(t, done); err != nil {
				t.Fatalf("Say = %v, want nil", err)
			}
			times := out.writeTimes()
			if len(times) == 0 {
				t.Fatal("UNKNOWN: nothing reached Output")
			}
			if first := times[0].Sub(sent); first < hold/2 {
				t.Errorf("the first frame reached Output %v after it was sent, want about the hold, %v", first, hold)
			}
			if spread := times[len(times)-1].Sub(times[0]); spread > hold/2 {
				t.Errorf("the reply's frames reached Output over %v, want them together: the hold split the reply", spread)
			}
		})
	}
}

// TestASayWhoseAudioWaitsPastItsLimitIsSpoken fails if a Say whose words and
// audio came within its limit, but whose audio still waits out the hold when
// the limit passes, returns ErrNotSpoken: the audio then plays anyway, and
// the Supervisor, told it was not spoken, would say the answer again.
func TestASayWhoseAudioWaitsPastItsLimitIsSpoken(t *testing.T) {
	const limit = 200 * time.Millisecond
	stand := startStandIn(t)
	out := newFakeOutput()
	clock := &heldClock{}
	sess, c, _ := open(t, stand, Config{sayLimit: limit, speechGap: time.Minute, holdAfter: clock.after}, voice.Exchange{Out: out, Tools: tools(nil)})
	defer func() { _ = sess.Close() }()
	pcm := tone(OutputRate, 20*time.Millisecond, 150)
	done := sayAsync(sess, "Held words.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Held words."})
	c.sendAudio(pcm)
	c.send(map[string]any{"type": "AgentAudioDone"})
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "read"})
	waitEvent(t, sess, voice.EventUserTurn) // the session has the words and their audio
	select {
	case err := <-done:
		t.Fatalf("Say = %v while its audio waited out the hold, want it to wait for the audio", err)
	case <-time.After(2 * limit): // past the limit, the audio still waits
	}
	clock.release()
	if err := returned(t, done); err != nil {
		t.Fatalf("Say = %v, want nil: its audio came within the limit and played", err)
	}
	if got := out.nextPlayback(t).written(); !bytes.Equal(got, pcm) {
		t.Fatalf("the Say played %d bytes, want its %d", len(got), len(pcm))
	}
}

// timedOutput is an Output that takes every frame at once and records when
// each came.
type timedOutput struct {
	mu    sync.Mutex
	times []time.Time
}

func (o *timedOutput) Begin() voice.Playback { return o }

func (o *timedOutput) Write(context.Context, []byte, audio.AudioFormat) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.times = append(o.times, time.Now())
	return nil
}

func (o *timedOutput) Drain(context.Context) error { return nil }

func (o *timedOutput) writeTimes() []time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]time.Time(nil), o.times...)
}

// everySharedDoneTrace returns every order of one reply's sentences: each
// sentence text-first or audio-first, one AgentAudioDone after all audio,
// and, when the last sentence is audio-first, its text before or after it.
func everySharedDoneTrace(sentences int) [][]sharedEvent {
	var traces [][]sharedEvent
	for mask := range 1 << sentences {
		local := make([]sharedSentence, sentences)
		for i := range local {
			local[i].textFirst = mask&(1<<i) != 0
		}
		lateText := []bool{false}
		if !local[sentences-1].textFirst {
			lateText = append(lateText, true)
		}
		for _, late := range lateText {
			var trace []sharedEvent
			for i, s := range local {
				switch {
				case s.textFirst:
					trace = append(trace, sharedEvent{i, replyText}, sharedEvent{i, replyAudio})
				case i == sentences-1 && late:
					trace = append(trace, sharedEvent{i, replyAudio}, sharedEvent{-1, replyDone}, sharedEvent{i, replyText})
				default:
					trace = append(trace, sharedEvent{i, replyAudio}, sharedEvent{i, replyText})
				}
			}
			if !late {
				trace = append(trace, sharedEvent{-1, replyDone})
			}
			traces = append(traces, trace)
		}
	}
	return traces
}

func sharedTraceName(trace []sharedEvent, injected int) string {
	parts := make([]string, len(trace))
	for i, e := range trace {
		switch e.kind {
		case replyDone:
			parts[i] = "D"
		case replyUserStart:
			parts[i] = "U"
		case replyText:
			parts[i] = fmt.Sprintf("T%d", e.sentence+1)
		case replyAudio:
			parts[i] = fmt.Sprintf("A%d", e.sentence+1)
		}
		if e.sentence == injected && e.sentence >= 0 {
			parts[i] += "i"
		}
	}
	return strings.Join(parts, "-")
}

// heldClock is the hold's timer for a trace: the hold runs out only when the
// test says, once the session has read every event of the trace, so the
// trace's events all come within the hold, as Deepgram sends one reply's,
// however slow a loaded machine runs the test. After that it is real time.
type heldClock struct {
	mu   sync.Mutex
	free bool
	due  []func()
}

func (c *heldClock) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.free {
		time.AfterFunc(d, f)
		return
	}
	c.due = append(c.due, f)
}

func (c *heldClock) release() {
	c.mu.Lock()
	due := c.due
	c.due, c.free = nil, true
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

func runSharedDoneTrace(t *testing.T, trace []sharedEvent, sentences, injected int, mustBeWhole bool) {
	t.Helper()
	stand := startStandIn(t)
	base := newFakeOutput()
	out := newGateOrderingOutput(base)
	clock := &heldClock{}
	sess, c, _ := open(t, stand, Config{sayLimit: 2 * time.Second, speechGap: time.Minute, holdAfter: clock.after}, voice.Exchange{Out: out, Tools: silentConcierge()})
	defer func() { _ = sess.Close() }()

	texts := make([]string, sentences)
	samples := make([]int16, sentences)
	for i := range sentences {
		texts[i] = fmt.Sprintf("Shared model sentence %d.", i+1)
		samples[i] = int16(-(500 + i))
		if i == injected {
			texts[i] = fmt.Sprintf("Shared injected sentence %d.", i+1)
			samples[i] = int16(500 + i)
		}
	}
	name := sharedTraceName(trace, injected)

	var said chan error
	if injected >= 0 {
		out.blockNextWrite()
		said = sayAsync(sess, texts[injected])
		c.expect(t, "InjectAgentMessage")
	}
	for _, e := range trace {
		switch e.kind {
		case replyText:
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": texts[e.sentence]})
		case replyAudio:
			c.sendAudio(tone(OutputRate, 20*time.Millisecond, samples[e.sentence]))
		case replyDone:
			c.send(map[string]any{"type": "AgentAudioDone"})
		case replyUserStart:
			c.send(map[string]any{"type": "UserStartedSpeaking"})
		}
	}
	// The room's words after the trace show the session has read all of it;
	// only then does the hold run out.
	c.send(map[string]any{"type": "ConversationText", "role": "user", "content": "trace read"})
	for waitEvent(t, sess, voice.EventUserTurn).Text != "trace read" {
		// each pass waits for the next turn the session reports
	}
	clock.release()

	spoken := false
	if injected >= 0 {
		select {
		case <-out.entered:
			select {
			case err := <-said:
				t.Fatalf("%s: Say returned %v before Output accepted any bytes", name, err)
			case <-time.After(time.Millisecond):
			}
			out.releaseWrite()
			err := returned(t, said)
			spoken = err == nil
		case err := <-said:
			if err == nil {
				t.Fatalf("%s: Say returned nil before Output accepted its bytes", name)
			}
			out.releaseWrite()
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: Say neither reached Output nor returned", name)
		}
		if mustBeWhole && !spoken {
			t.Fatalf("%s: an injection alone was not spoken", name)
		}
	}

	barrierText := "Shared ordering barrier."
	barrierPCM := tone(OutputRate, 20*time.Millisecond, 900)
	barrier := sayAsync(sess, barrierText)
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": barrierText})
	c.sendAudio(barrierPCM)
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, barrier); err != nil {
		t.Fatalf("%s: ordering barrier = %v", name, err)
	}

	got := outputSamples(t, base)
	for i, sample := range samples {
		want := 0
		if i == injected && spoken {
			want = len(tone(OutputRate, 20*time.Millisecond, sample))
		}
		if got[sample] != want {
			t.Fatalf("%s: sentence %d (sample %d) reached Output: %d bytes, want %d", name, i+1, sample, got[sample], want)
		}
	}
	if got[900] != len(barrierPCM) {
		t.Fatalf("%s: barrier reached Output: %d bytes, want %d", name, got[900], len(barrierPCM))
	}
}
