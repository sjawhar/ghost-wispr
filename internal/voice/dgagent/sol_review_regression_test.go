package dgagent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The positive control uses the same socket, player, and Output as the
// unexpected speech. It proves the byte count sees actual playback.
func TestSolReviewOnlyInjectedSpeechWithoutUserTurn(t *testing.T) {
	for _, heldAnswer := range []bool{false, true} {
		name := "before_first_user_turn"
		if heldAnswer {
			name = "after_held_answer_without_user_turn"
		}
		t.Run(name, func(t *testing.T) {
			setSpeechGap(t, time.Minute)
			stand := startStandIn(t)
			out := newFakeOutput()
			sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
			approved := tone(OutputRate, 20*time.Millisecond, 222)
			sayApproved := func(text string) {
				done := sayAsync(sess, text)
				c.expect(t, "InjectAgentMessage")
				c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text})
				c.sendAudio(approved)
				c.send(map[string]any{"type": "AgentAudioDone"})
				if err := returned(t, done); err != nil {
					t.Fatal(err)
				}
			}
			expectedApproved := len(approved)
			if heldAnswer {
				sayApproved("ABC-12 is in review.")
				expectedApproved += len(approved)
			}
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "ABC-12 shipped yesterday."})
			c.sendAudio(tone(OutputRate, 20*time.Millisecond, 111))
			c.send(map[string]any{"type": "AgentAudioDone"})
			sayApproved("This is the approved marker.")
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
						t.Fatalf("unknown sample")
					}
				}
			}
			if approvedBytes != expectedApproved {
				t.Fatalf("UNKNOWN: approved playback control saw %d bytes, want %d", approvedBytes, expectedApproved)
			}
			t.Logf("approved control: %d bytes; model-only speech: %d bytes", approvedBytes, unexpectedBytes)
			if unexpectedBytes != 0 {
				t.Fatalf("model-only speech reached Output: %d bytes, want 0", unexpectedBytes)
			}
		})
	}
}

func TestSolReviewDebugLogsKeepNoRoomWords(t *testing.T) {
	const words = "SYNTHETIC-ROOM-QUOTE-SOL"
	stand := startStandIn(t)
	var logs lockedBuf
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runner := tools(func(context.Context, voice.ToolCall) (json.RawMessage, error) {
		return nil, errors.New("bad arguments near " + words)
	})
	sess, c, _ := open(t, stand, Config{Logger: logger}, voice.Exchange{Out: newFakeOutput(), Tools: runner})
	c.send(map[string]any{"type": "Warning", "code": "FUNCTION_CALL_FAILED", "description": "call failed: " + words})
	c.send(answerCall("sol-call", "what about ABC-12"))
	c.expect(t, "FunctionCallResponse")
	c.send(map[string]any{"type": "Error", "code": "UNPARSABLE_CLIENT_MESSAGE", "description": "could not parse: " + words})
	waitEvent(t, sess, voice.EventErr)
	_ = sess.Close()
	got := logs.String()
	if !strings.Contains(got, "UNPARSABLE_CLIENT_MESSAGE") {
		t.Fatal("UNKNOWN: logging control saw no error code")
	}
	if strings.Contains(got, words) {
		t.Fatalf("room words reached Debug log:\n%s", got)
	}
}

func TestSolReviewAudioBeforeUnmatchedText(t *testing.T) {
	for _, heldAnswer := range []bool{false, true} {
		name := "before_first_user_turn"
		if heldAnswer {
			name = "after_held_answer_without_user_turn"
		}
		t.Run(name, func(t *testing.T) {
			setSpeechGap(t, time.Minute)
			stand := startStandIn(t)
			out := newFakeOutput()
			sess, c, _ := open(t, stand, Config{}, voice.Exchange{Out: out, Tools: tools(nil)})
			approved := tone(OutputRate, 20*time.Millisecond, 222)
			sayApproved := func(text string) {
				done := sayAsync(sess, text)
				c.expect(t, "InjectAgentMessage")
				c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": text})
				c.sendAudio(approved)
				c.send(map[string]any{"type": "AgentAudioDone"})
				if err := returned(t, done); err != nil {
					t.Fatal(err)
				}
			}
			expectedApproved := len(approved)
			if heldAnswer {
				sayApproved("ABC-12 is in review.")
				expectedApproved += len(approved)
			}
			// Deepgram may send audio before ConversationText.
			c.sendAudio(tone(OutputRate, 20*time.Millisecond, 111))
			c.send(map[string]any{"type": "ConversationText", "role": "assistant", "content": "ABC-12 shipped yesterday."})
			c.send(map[string]any{"type": "AgentAudioDone"})
			sayApproved("This is the approved marker.")
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
						t.Fatalf("unknown sample")
					}
				}
			}
			if approvedBytes != expectedApproved {
				t.Fatalf("UNKNOWN: approved playback control saw %d bytes, want %d", approvedBytes, expectedApproved)
			}
			t.Logf("approved control: %d bytes; audio-first model speech: %d bytes", approvedBytes, unexpectedBytes)
			if unexpectedBytes != 0 {
				t.Fatalf("audio-first model speech reached Output: %d bytes, want 0", unexpectedBytes)
			}
		})
	}
}
