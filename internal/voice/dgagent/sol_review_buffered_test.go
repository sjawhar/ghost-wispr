package dgagent

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

func TestSolReviewBufferedDoneRearmsTheGate(t *testing.T) {
	setSpeechGap(t, time.Minute)
	stand := startStandIn(t)
	out := newFakeOutput()
	sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	approved := tone(OutputRate, 20*time.Millisecond, 222)
	sayApproved := func(text string) {
		done := sayAsync(sess, text)
		c.expect(t, "InjectAgentMessage")
		c.sendAudio(approved)
		c.send(map[string]any{"type": "AgentAudioDone"})
		c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text})
		if err := returned(t, done); err != nil {
			t.Fatal(err)
		}
	}
	expectedApproved := len(approved)
	sayApproved("Approved first answer.")
	expectedApproved += len(approved)
	// EndOfAudio already came before the matching text. The next utterance
	// must start untrusted again, even when its audio precedes its text.
	c.sendAudio(tone(OutputRate, 20*time.Millisecond, 111))
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "ABC-12 shipped yesterday."})
	c.send(map[string]any{"type": "AgentAudioDone"})
	sayApproved("Approved final marker.")
	approvedBytes, unexpectedBytes := 0, 0
	for _, p := range out.playbacks() {
		pcm := p.written()
		for i := 0; i+1 < len(pcm); i += 2 {
			switch int16(binary.LittleEndian.Uint16(pcm[i:])) {
			case 222:
				approvedBytes += 2
			case 111:
				unexpectedBytes += 2
			default:
				t.Fatal("unknown sample")
			}
		}
	}
	if approvedBytes != expectedApproved {
		t.Fatalf("UNKNOWN: approved playback control saw %d bytes, want %d", approvedBytes, expectedApproved)
	}
	t.Logf("approved control: %d bytes; next model-only utterance: %d bytes", approvedBytes, unexpectedBytes)
	if unexpectedBytes != 0 {
		t.Fatalf("completed buffered injection left the gate open: %d model-only bytes reached Output", unexpectedBytes)
	}
}

type solBlockedOutput struct {
	out     *fakeOutput
	entered chan struct{}
	release chan struct{}
}

func (o *solBlockedOutput) Begin() voice.Playback {
	return solBlockedPlayback{Playback: o.out.Begin(), out: o}
}

type solBlockedPlayback struct {
	voice.Playback
	out *solBlockedOutput
}

func (p solBlockedPlayback) Write(ctx context.Context, b []byte, f audio.AudioFormat) error {
	select {
	case p.out.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.out.release:
		return p.Playback.Write(ctx, b, f)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSolReviewBufferedSayWaitsForOutput(t *testing.T) {
	setSpeechGap(t, time.Minute)
	stand := startStandIn(t)
	base := newFakeOutput()
	out := &solBlockedOutput{out: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
	sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
	done := sayAsync(sess, "Approved buffered answer.")
	c.expect(t, "InjectAgentMessage")
	approved := tone(OutputRate, 20*time.Millisecond, 222)
	c.sendAudio(approved)
	c.send(map[string]any{"type": "AgentAudioDone"})
	c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "Approved buffered answer."})
	select {
	case <-out.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("UNKNOWN: no Output write was attempted")
	}
	var early bool
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	before := len(base.playbacks()[0].written())
	close(out.release)
	base.playbacks()[0].waitBytes(t, len(approved))
	if !early {
		if err := returned(t, done); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("before release: %d bytes; after release: %d bytes; Say returned before release: %v", before, len(base.playbacks()[0].written()), early)
	if early {
		t.Fatal("Say returned nil before its buffered audio was queued to Output")
	}
}

func TestSolReviewSayQueueBoundaryForEachOrdering(t *testing.T) {
	for _, ordering := range []string{"text_then_audio", "audio_then_text_before_done", "audio_done_then_text"} {
		t.Run(ordering, func(t *testing.T) {
			setSpeechGap(t, time.Minute)
			stand := startStandIn(t)
			base := newFakeOutput()
			out := &solBlockedOutput{out: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
			sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
			done := sayAsync(sess, "Approved answer for ordering.")
			c.expect(t, "InjectAgentMessage")
			pcm := tone(OutputRate, 20*time.Millisecond, 222)
			text := map[string]any{"type": "ConversationText", "role": "assistant", "content": "Approved answer for ordering."}
			end := map[string]any{"type": "AgentAudioDone"}
			switch ordering {
			case "text_then_audio":
				c.send(text)
				c.sendAudio(pcm)
			case "audio_then_text_before_done":
				c.sendAudio(pcm)
				c.send(text)
			case "audio_done_then_text":
				c.sendAudio(pcm)
				c.send(end)
				c.send(text)
			}
			select {
			case <-out.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("UNKNOWN: no Output write was attempted")
			}
			var early bool
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
				early = true
			case <-time.After(100 * time.Millisecond):
			}
			before := len(base.playbacks()[0].written())
			close(out.release)
			if ordering != "audio_done_then_text" {
				c.send(end)
			}
			base.playbacks()[0].waitBytes(t, len(pcm))
			if !early {
				if err := returned(t, done); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("before release: %d bytes; after release: %d bytes; Say returned before release: %v", before, len(base.playbacks()[0].written()), early)
			if early {
				t.Fatal("Say returned nil before Output accepted the buffered audio")
			}
		})
	}
}
