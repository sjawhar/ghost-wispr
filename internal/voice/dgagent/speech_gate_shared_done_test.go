package dgagent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// sharedSentence is one sentence of a Deepgram reply whose sentences share a
// single AgentAudioDone, such as an injection and the model's own words after
// it. textFirst says whether its text comes before its audio.
type sharedSentence struct {
	textFirst bool
}

type sharedEvent struct {
	sentence int // -1 for AgentAudioDone
	kind     replyEvent
}

// TestSpeechGateSharedDoneReplies covers one Deepgram reply of one to three
// sentences that ends with a single AgentAudioDone: an injection alone, or
// with the model's own sentences before or after it. Each sentence's text
// may come before or after its own audio, and the last sentence's text may
// come after the AgentAudioDone. A sentence's text never comes after the
// next sentence's audio: no message names its sentence, so nothing could
// tell such audio apart. Sol's review of 65a0c06 found the model's audio
// after an injection, before the model's text, playing.
//
// Whatever the order, no model byte reaches Output, and a Say returns nil
// only once Output has accepted all of its bytes; else it returns an error.
// An injection alone plays whole in every order.
func TestSpeechGateSharedDoneReplies(t *testing.T) {
	for sentences := 1; sentences <= 3; sentences++ {
		for injected := -1; injected < sentences; injected++ {
			if sentences == 1 && injected < 0 {
				continue
			}
			for _, trace := range everySharedDoneTrace(sentences) {
				name := fmt.Sprintf("%d-sentences/injected-%d/%s", sentences, injected+1, sharedTraceName(trace, injected))
				if !t.Run(name, func(t *testing.T) {
					runSharedDoneTrace(t, trace, sentences, injected, sentences == 1)
				}) {
					return
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
		switch {
		case e.kind == replyDone:
			parts[i] = "D"
		case e.kind == replyText:
			parts[i] = fmt.Sprintf("T%d", e.sentence+1)
		default:
			parts[i] = fmt.Sprintf("A%d", e.sentence+1)
		}
		if e.sentence == injected && e.kind != replyDone {
			parts[i] += "i"
		}
	}
	return strings.Join(parts, "-")
}

func runSharedDoneTrace(t *testing.T, trace []sharedEvent, sentences, injected int, mustBeWhole bool) {
	t.Helper()
	setSpeechGap(t, time.Minute)
	stand := startStandIn(t)
	base := newFakeOutput()
	out := newGateOrderingOutput(base)
	sess, c, _ := open(t, stand, Config{sayLimit: 2 * time.Second}, voice.Exchange{Out: out, Tools: tools(nil)})
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
		}
	}

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
