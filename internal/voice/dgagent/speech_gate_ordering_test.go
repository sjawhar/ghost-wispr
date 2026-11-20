package dgagent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

type replyEvent uint8

const (
	replyText replyEvent = iota
	replyAudio
	replyDone
)

type gateReply struct {
	injected bool
	order    []replyEvent
}

// TestSpeechGateEveryReplyOrdering is a state-space test for the speech gate.
// It covers the three causally possible orders of each reply's text, audio,
// and AgentAudioDone across every two- and three-reply sequence. A reply is
// either our injected text or a model reply the gate rejects. Each injected
// reply blocks Output.Write before it accepts audio, so the test proves Say
// cannot succeed early. The sample values distinguish every reply at Output.
func TestSpeechGateEveryReplyOrdering(t *testing.T) {
	orders := [][]replyEvent{
		{replyText, replyAudio, replyDone},
		{replyAudio, replyText, replyDone},
		{replyAudio, replyDone, replyText},
	}

	for _, replies := range []int{2, 3} {
		for _, trace := range everyGateTrace(replies, orders) {
			if !t.Run(gateTraceName(trace), func(t *testing.T) {
				runGateTrace(t, trace)
			}) {
				return
			}
		}
	}

	// The five Sol review rounds stay named here, beside the exhaustive
	// matrix, so the concrete regressions remain legible when it fails.
	t.Run("sol-round-1/model-speech-never-plays", TestSolReviewOnlyInjectedSpeechWithoutUserTurn)
	t.Run("sol-round-2/audio-before-unmatched-text", TestSolReviewAudioBeforeUnmatchedText)
	t.Run("sol-round-3/completed-buffer-rearms", TestSolReviewBufferedDoneRearmsTheGate)
	t.Run("sol-round-4/buffered-say-waits-for-output", TestSolReviewBufferedSayWaitsForOutput)
	t.Run("sol-round-5/no-cross-utterance-state", testSolReviewCrossUtteranceState)
}

func everyGateTrace(n int, orders [][]replyEvent) [][]gateReply {
	var traces [][]gateReply
	var visit func([]gateReply)
	visit = func(trace []gateReply) {
		if len(trace) == n {
			traces = append(traces, append([]gateReply(nil), trace...))
			return
		}
		for _, injected := range []bool{false, true} {
			for _, order := range orders {
				visit(append(trace, gateReply{injected: injected, order: order}))
			}
		}
	}
	visit(nil)
	return traces
}

func gateTraceName(trace []gateReply) string {
	parts := make([]string, len(trace))
	for i, reply := range trace {
		kind := "model-rejected"
		if reply.injected {
			kind = "injected"
		}
		order := make([]string, len(reply.order))
		for j, event := range reply.order {
			switch event {
			case replyText:
				order[j] = "text"
			case replyAudio:
				order[j] = "audio"
			case replyDone:
				order[j] = "done"
			}
		}
		parts[i] = kind + "-" + strings.Join(order, "-")
	}
	return strings.Join(parts, "__")
}

type gateStreamEvent struct {
	reply int
	kind  replyEvent
}

// TestSpeechGateEventInterleavings takes the audio-done-text ordering, where
// a reply's text can arrive after its audio is complete, and recursively
// executes every event-level interleaving of two and three replies. The
// reply-local matrix above covers the other legal local orders. Each run has
// either no injection or exactly one at every possible reply position; the
// other replies are rejected model speech. This keeps the session's one-Say
// contract real while exercising every cross-reply race around its text.
func TestSpeechGateEventInterleavings(t *testing.T) {
	for _, replies := range []int{2, 3} {
		for injected := -1; injected < replies; injected++ {
			name := fmt.Sprintf("%d-replies", replies)
			if injected < 0 {
				name += "/all-model"
			} else {
				name += fmt.Sprintf("/injected-%d", injected+1)
			}
			if !t.Run(name, func(t *testing.T) {
				everyAudioDoneTextInterleaving(replies, func(trace []gateStreamEvent) {
					runGateEventInterleaving(t, trace, injected)
				})
			}) {
				return
			}
		}
	}
}

func everyAudioDoneTextInterleaving(replies int, yield func([]gateStreamEvent)) {
	const eventsPerReply = 3
	order := [...]replyEvent{replyAudio, replyDone, replyText}
	next := make([]int, replies)
	trace := make([]gateStreamEvent, 0, replies*eventsPerReply)
	var visit func()
	visit = func() {
		if len(trace) == cap(trace) {
			yield(append([]gateStreamEvent(nil), trace...))
			return
		}
		for reply := range next {
			if next[reply] == eventsPerReply {
				continue
			}
			// Binary audio frames have no reply ID, so Deepgram cannot start
			// reply N+1's audio before it has ended reply N's audio. Text is
			// separate and may arrive after that boundary.
			if next[reply] == 0 && reply > 0 && next[reply-1] < 2 {
				continue
			}
			// ConversationText also has no reply ID. It remains in reply order
			// even when a reply's text trails the next reply's audio.
			if next[reply] == 2 && reply > 0 && next[reply-1] < 3 {
				continue
			}
			trace = append(trace, gateStreamEvent{reply: reply, kind: order[next[reply]]})
			next[reply]++
			visit()
			next[reply]--
			trace = trace[:len(trace)-1]
		}
	}
	visit()
}

func runGateEventInterleaving(t *testing.T, trace []gateStreamEvent, injected int) {
	t.Helper()
	setSpeechGap(t, time.Minute)
	stand := startStandIn(t)
	base := newFakeOutput()
	out := newGateOrderingOutput(base)
	sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	defer func() { _ = sess.Close() }()

	before := outputSamples(t, base)
	texts := make([]string, 0, 3)
	samples := make([]int16, 0, 3)
	for reply := range 3 {
		text := fmt.Sprintf("Rejected model reply %d.", reply+1)
		if reply == injected {
			text = fmt.Sprintf("Interleaved injected reply %d.", reply+1)
		}
		texts = append(texts, text)
		samples = append(samples, int16(300+reply))
	}

	var said chan error
	if injected >= 0 {
		out.blockNextWrite()
		said = sayAsync(sess, texts[injected])
		c.expect(t, "InjectAgentMessage")
	}
	for _, event := range trace {
		switch event.kind {
		case replyAudio:
			c.sendAudio(tone(OutputRate, 20*time.Millisecond, samples[event.reply]))
		case replyDone:
			c.send(map[string]any{"type": "AgentAudioDone"})
		case replyText:
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": texts[event.reply]})
		}
	}
	if injected >= 0 {
		out.waitForSayWrite(t, said, injected+1)
		select {
		case err := <-said:
			t.Fatalf("%s: Say returned %v before Output accepted its bytes", gateStreamName(trace), err)
		case <-time.After(time.Millisecond):
		}
		out.releaseWrite()
		if err := returned(t, said); err != nil {
			t.Fatalf("%s: injected Say = %v, want nil", gateStreamName(trace), err)
		}
	}

	barrierText := "Event ordering barrier."
	barrierPCM := tone(OutputRate, 20*time.Millisecond, 900)
	barrier := sayAsync(sess, barrierText)
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": barrierText})
	c.sendAudio(barrierPCM)
	c.send(map[string]any{"type": "AgentAudioDone"})
	select {
	case err := <-barrier:
		if err != nil {
			t.Fatalf("%s: ordering barrier = %v", gateStreamName(trace), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: ordering barrier did not return", gateStreamName(trace))
	}

	got := outputDelta(t, before, outputSamples(t, base))
	for reply, sample := range samples {
		want := 0
		if reply == injected {
			want = len(tone(OutputRate, 20*time.Millisecond, sample))
		}
		if got[sample] != want {
			t.Fatalf("%s: reply %d sample reached Output: %d bytes, want %d", gateStreamName(trace), reply+1, got[sample], want)
		}
	}
	if got[900] != len(barrierPCM) {
		t.Fatalf("%s: barrier reached Output: %d bytes, want %d", gateStreamName(trace), got[900], len(barrierPCM))
	}

}

func gateStreamName(trace []gateStreamEvent) string {
	var b strings.Builder
	for i, event := range trace {
		if i > 0 {
			b.WriteByte('-')
		}
		switch event.kind {
		case replyText:
			b.WriteByte('T')
		case replyAudio:
			b.WriteByte('A')
		case replyDone:
			b.WriteByte('D')
		}
		fmt.Fprint(&b, event.reply+1)
	}
	return b.String()
}

func outputDelta(t *testing.T, before, after map[int16]int) map[int16]int {
	t.Helper()
	delta := make(map[int16]int, len(after))
	for sample, bytes := range after {
		if bytes < before[sample] {
			t.Fatalf("Output sample %d regressed from %d to %d bytes", sample, before[sample], bytes)
		}
		delta[sample] = bytes - before[sample]
	}
	return delta
}

func runGateTrace(t *testing.T, trace []gateReply) {
	t.Helper()
	setSpeechGap(t, time.Minute)
	stand := startStandIn(t)
	base := newFakeOutput()
	out := newGateOrderingOutput(base)
	sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	t.Cleanup(func() { _ = sess.Close() })

	want := make(map[int16]int)
	for i, reply := range trace {
		text := fmt.Sprintf("Injected reply %d.", i)
		sample := int16(1000 + i)
		if !reply.injected {
			text = fmt.Sprintf("Rejected model reply %d.", i)
			sample = -sample
		}
		pcm := tone(OutputRate, 20*time.Millisecond, sample)

		var said chan error
		if reply.injected {
			out.blockNextWrite()
			said = sayAsync(sess, text)
			c.expect(t, "InjectAgentMessage")
			want[sample] += len(pcm)
		}

		for _, event := range reply.order {
			switch event {
			case replyText:
				c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text})
			case replyAudio:
				c.sendAudio(pcm)
			case replyDone:
				c.send(map[string]any{"type": "AgentAudioDone"})
			}
		}

		if !reply.injected {
			continue
		}
		out.waitForSayWrite(t, said, i)
		select {
		case err := <-said:
			t.Fatalf("Say returned %v before Output accepted reply %d", err, i)
		case <-time.After(10 * time.Millisecond):
		}
		out.releaseWrite()
		if err := returned(t, said); err != nil {
			t.Fatalf("Say for authorized reply %d = %v, want nil after Output accepted its bytes", i, err)
		}
	}

	// A final authorized reply is a reader/player barrier. Without it, a
	// final rejected reply can still sit in the socket when Output is read.
	barrierPCM := tone(OutputRate, 20*time.Millisecond, 2000)
	barrier := sayAsync(sess, "Ordering barrier.")
	c.expect(t, "InjectAgentMessage")
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Ordering barrier."})
	c.sendAudio(barrierPCM)
	c.send(map[string]any{"type": "AgentAudioDone"})
	if err := returned(t, barrier); err != nil {
		t.Fatalf("ordering barrier Say = %v", err)
	}
	want[2000] = len(barrierPCM)

	got := outputSamples(t, base)
	for sample, bytes := range got {
		if sample < 0 {
			t.Fatalf("rejected model audio sample %d reached Output: %d bytes", sample, bytes)
		}
		if want[sample] != bytes {
			t.Fatalf("authorized sample %d reached Output: %d bytes, want %d", sample, bytes, want[sample])
		}
	}
	for sample, bytes := range want {
		if got[sample] != bytes {
			t.Fatalf("authorized sample %d reached Output: %d bytes, want %d", sample, got[sample], bytes)
		}
	}
}

func outputSamples(t *testing.T, out *fakeOutput) map[int16]int {
	t.Helper()
	got := make(map[int16]int)
	for _, playback := range out.playbacks() {
		pcm := playback.written()
		for i := 0; i+1 < len(pcm); i += 2 {
			got[int16(binary.LittleEndian.Uint16(pcm[i:]))] += 2
		}
	}
	return got
}

// gateOrderingOutput is an Output whose next Write waits until the test lets
// it return. It exposes the real Output acceptance boundary, not queueing.
type gateOrderingOutput struct {
	out *fakeOutput

	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

func newGateOrderingOutput(out *fakeOutput) *gateOrderingOutput {
	return &gateOrderingOutput{out: out, entered: make(chan struct{}, 1)}
}

func (o *gateOrderingOutput) Begin() voice.Playback {
	return gateOrderingPlayback{Playback: o.out.Begin(), out: o}
}

func (o *gateOrderingOutput) blockNextWrite() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.gate != nil {
		panic("a Write is already blocked")
	}
	o.gate = make(chan struct{})
}

func (o *gateOrderingOutput) waitForWrite(t *testing.T) {
	t.Helper()
	select {
	case <-o.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("UNKNOWN: authorized audio never reached Output.Write")
	}
}

func (o *gateOrderingOutput) waitForSayWrite(t *testing.T, said <-chan error, reply int) {
	t.Helper()
	select {
	case <-o.entered:
	case err := <-said:
		t.Fatalf("Say for authorized reply %d returned %v before Output accepted its bytes", reply, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("authorized audio of reply %d never reached Output.Write", reply)
	}
}

func (o *gateOrderingOutput) releaseWrite() {
	o.mu.Lock()
	defer o.mu.Unlock()
	close(o.gate)
	o.gate = nil
}

type gateOrderingPlayback struct {
	voice.Playback
	out *gateOrderingOutput
}

func (p gateOrderingPlayback) Write(ctx context.Context, b []byte, f audio.AudioFormat) error {
	p.out.mu.Lock()
	gate := p.out.gate
	p.out.mu.Unlock()
	if gate != nil {
		select {
		case p.out.entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Playback.Write(ctx, b, f)
}

func testSolReviewCrossUtteranceState(t *testing.T) {
	t.Run("rejected-audio-done-text-does-not-mute-next-injection", func(t *testing.T) {
		setSpeechGap(t, time.Minute)
		stand := startStandIn(t)
		out := newFakeOutput()
		sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
		defer func() { _ = sess.Close() }()

		c.sendAudio(tone(OutputRate, 20*time.Millisecond, -111))
		c.send(map[string]any{"type": "AgentAudioDone"})
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Rejected model reply."})

		done := sayAsync(sess, "Approved next reply.")
		c.expect(t, "InjectAgentMessage")
		approved := tone(OutputRate, 20*time.Millisecond, 222)
		c.sendAudio(approved)
		c.send(map[string]any{"type": "AgentAudioDone"})
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Approved next reply."})
		if err := returned(t, done); err != nil {
			t.Fatalf("next Say = %v, want nil", err)
		}
		got := outputSamples(t, out)
		if got[-111] != 0 || got[222] != len(approved) {
			t.Fatalf("rejected/approved bytes = %d/%d, want 0/%d", got[-111], got[222], len(approved))
		}
	})

	t.Run("injected-audio-done-model-audio-injected-text-model-text-done", func(t *testing.T) {
		setSpeechGap(t, time.Minute)
		stand := startStandIn(t)
		base := newFakeOutput()
		out := newGateOrderingOutput(base)
		sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
		defer func() { _ = sess.Close() }()

		injected := tone(OutputRate, 20*time.Millisecond, 666)
		model := tone(OutputRate, 20*time.Millisecond, -777)
		out.blockNextWrite()
		done := sayAsync(sess, "Interleaved injected reply.")
		c.expect(t, "InjectAgentMessage")
		// A1, D1, A2, T1: injection one's completed audio is still pending
		// when model reply two starts. T1 must release A1 and never A2.
		c.sendAudio(injected)
		c.send(map[string]any{"type": "AgentAudioDone"})
		c.sendAudio(model)
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Interleaved injected reply."})
		out.waitForSayWrite(t, done, 1)
		select {
		case err := <-done:
			t.Fatalf("interleaved Say returned %v before Output accepted its bytes", err)
		case <-time.After(10 * time.Millisecond):
		}
		out.releaseWrite()
		// T2, D2
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Rejected model reply."})
		c.send(map[string]any{"type": "AgentAudioDone"})
		if err := returned(t, done); err != nil {
			t.Fatalf("interleaved Say = %v, want nil", err)
		}

		barrier := sayAsync(sess, "Interleaved ordering barrier.")
		c.expect(t, "InjectAgentMessage")
		barrierPCM := tone(OutputRate, 20*time.Millisecond, 888)
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Interleaved ordering barrier."})
		c.sendAudio(barrierPCM)
		c.send(map[string]any{"type": "AgentAudioDone"})
		if err := returned(t, barrier); err != nil {
			t.Fatal(err)
		}
		got := outputSamples(t, base)
		if got[-777] != 0 || got[666] != len(injected) || got[888] != len(barrierPCM) {
			t.Fatalf("injected/model/barrier bytes = %d/%d/%d, want %d/0/%d", got[666], got[-777], got[888], len(injected), len(barrierPCM))
		}
	})

	t.Run("previous-audio-cannot-complete-next-say", func(t *testing.T) {
		setSpeechGap(t, time.Minute)
		stand := startStandIn(t)
		base := newFakeOutput()
		out := newGateOrderingOutput(base)
		sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
		defer func() { _ = sess.Close() }()

		firstPCM := tone(OutputRate, 20*time.Millisecond, 333)
		out.blockNextWrite()
		first := sayAsync(sess, "First approved reply.")
		c.expect(t, "InjectAgentMessage")
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "First approved reply."})
		c.sendAudio(firstPCM)
		c.send(map[string]any{"type": "AgentAudioDone"})
		out.waitForWrite(t)
		out.releaseWrite()
		if err := returned(t, first); err != nil {
			t.Fatal(err)
		}

		secondPCM := tone(OutputRate, 20*time.Millisecond, 444)
		out.blockNextWrite()
		second := sayAsync(sess, "Second approved reply.")
		c.expect(t, "InjectAgentMessage")
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Second approved reply."})
		c.sendAudio(secondPCM)
		out.waitForWrite(t)
		select {
		case err := <-second:
			t.Fatalf("second Say returned %v before its Output write", err)
		case <-time.After(10 * time.Millisecond):
		}
		out.releaseWrite()
		c.send(map[string]any{"type": "AgentAudioDone"})
		if err := returned(t, second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("matching-text-without-audio-makes-say-fail", func(t *testing.T) {
		setSpeechGap(t, time.Minute)
		stand := startStandIn(t)
		sess, c, _ := open(t, stand, Config{sayLimit: 50 * time.Millisecond}, voice.Exchange{Out: newFakeOutput(), Tools: tools(nil)})
		defer func() { _ = sess.Close() }()

		done := sayAsync(sess, "No audio reply.")
		c.expect(t, "InjectAgentMessage")
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "No audio reply."})
		c.send(map[string]any{"type": "AgentAudioDone"})
		if err := returned(t, done); !errors.Is(err, ErrNotSpoken) {
			t.Fatalf("Say with no accepted audio = %v, want ErrNotSpoken", err)
		}
	})

	for _, ending := range []struct {
		name string
		end  func(voice.Session) error
	}{
		{"interrupt", func(sess voice.Session) error { sess.Interrupt(); return nil }},
		{"close", func(sess voice.Session) error { return sess.Close() }},
	} {
		ending := ending
		t.Run("terminal-"+ending.name+"-makes-unwritten-say-fail", func(t *testing.T) {
			setSpeechGap(t, time.Minute)
			stand := startStandIn(t)
			out := newGateOrderingOutput(newFakeOutput())
			sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
			if ending.name == "interrupt" {
				t.Cleanup(func() { _ = sess.Close() })
			}

			out.blockNextWrite()
			done := sayAsync(sess, "Terminal reply.")
			c.expect(t, "InjectAgentMessage")
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Terminal reply."})
			c.sendAudio(tone(OutputRate, 20*time.Millisecond, 555))
			out.waitForWrite(t)
			if err := ending.end(sess); err != nil {
				t.Fatal(err)
			}
			if err := returned(t, done); err == nil || (ending.name == "interrupt" && !errors.Is(err, ErrInterrupted)) {
				t.Fatalf("Say after %s = %v, want an error", ending.name, err)
			}
		})
	}
}
