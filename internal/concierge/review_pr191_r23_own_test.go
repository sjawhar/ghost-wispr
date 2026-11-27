// Sol's twenty-third review of #191 at 2066336
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6083691449):
// Sol's probes of a reply to GPT-Live's own question, kept as regression
// tests, unchanged. They read the explicit question the model gets.

package concierge

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// These assertions read the actual model question, not the history that
// happens to accompany it. The explicit new question is the negative control
// for accidentally pinning every reply to the original question.
func TestSol191R23OwnQuestionReplyMeaning(t *testing.T) {
	const opening = "why is the recorder blocked?"
	for _, tc := range []struct {
		name, reply, want string
		silent, decline   bool
	}{
		{"new_question_control", "what is the status of ABC-12?", "what is the status of ABC-12?", false, false},
		{"key_answers_original", "ABC-12", opening, false, false},
		{"decline_control", "no, thanks", "", false, true},
		{"side_talk_control", "okay, let's move on to the realtime voice budget", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, v, w := openVoiceBOn(t, opening)
			w.send(map[string]any{"type": "session.output_transcript.delta", "delta": "What is its key?"})
			vendorSpeaks(t, w, r)
			r.hearNow(tc.reply, true)
			sendDelegation(w, "reply", 1000)
			within(t, v.calls, "reply tool call")
			raw := within(t, v.results, "reply result")
			var got voice.AnswerResult
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			t.Logf("reply=%q model-question=%q say=%q holders=%q", tc.reply, modelQuestion(r), got.Say, holderMessages(r))
			if tc.silent || tc.decline {
				if modelQuestion(r) != "" || len(holderMessages(r)) != 0 {
					t.Errorf("side talk/decline reached backend: prompt=%q holder=%q", modelQuestion(r), holderMessages(r))
				}
				if tc.silent && got.Say != "" {
					t.Errorf("side talk answered: %q", got.Say)
				}
				if tc.decline && got.Say != "Okay." {
					t.Errorf("decline=%q, want Okay.", got.Say)
				}
			} else if actual := modelQuestion(r); actual != tc.want {
				t.Errorf("actual model question=%q, want %q; history containing original is insufficient", actual, tc.want)
			}
		})
	}
}

type sol23Snapshot struct {
	x    voice.Exchange
	turn string
}
type sol23Capture struct {
	*Concierge
	calls     chan sol23Snapshot
	holdFirst <-chan struct{}
	count     atomic.Int32
}

func (c *sol23Capture) Answer(ctx context.Context, x *voice.Exchange, words string, say voice.SayFunc) error {
	n := c.count.Add(1)
	c.calls <- sol23Snapshot{x: *x, turn: words}
	if n == 1 && c.holdFirst != nil {
		select {
		case <-c.holdFirst:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.Concierge.Answer(ctx, x, words, say)
}

func sol23Open(t *testing.T, opening string, hold <-chan struct{}) (*cutRig, *watchedVoice, *vendorWire, *sol23Capture) {
	t.Helper()
	url, conns := loopbackVendor(t, false)
	live, err := gptlive.New(gptlive.Config{URL: url, Token: func(context.Context) (string, error) { return "loopback-not-a-secret", nil }})
	if err != nil {
		t.Fatal(err)
	}
	v := watch(live)
	r := &cutRig{rig: newRig(t), speaker: &cutSpeaker{}}
	t.Cleanup(r.concierge.Stop)
	capture := &sol23Capture{Concierge: r.concierge, calls: make(chan sol23Snapshot, 20), holdFirst: hold}
	r.sup = voice.NewSupervisor(&voice.SupervisorConfig{
		Voice: func() (voice.Voice, error) { return v, nil }, Concierge: capture,
		Speaker: r.speaker, WakePhrases: []string{"Wispr"}, TakeBack: TakeBack,
		Red: func() bool { return true }, Session: func() string { return "meeting-1" },
		IdleWait: voice.DefaultIdleWait, FollowUpWait: voice.DefaultFollowUpWait, Logf: t.Logf,
	})
	t.Cleanup(r.sup.Close)
	heldIssue(r.rig, "session", false)
	r.model.turns = answers(20)
	r.hearNow("Wispr, "+opening, true)
	w := within(t, conns, "vendor connection")
	w.next(t, "session.start")
	within(t, v.opened, "opened voice")
	return r, v, w, capture
}

func TestSol191R23QuestionWhileDelegationOpen(t *testing.T) {
	const opening = "why is the recorder blocked?"
	release := make(chan struct{})
	r, v, w, capture := sol23Open(t, opening, release)
	defer close(release)
	sendDelegation(w, "initial", 1000)
	within(t, v.calls, "initial delegation")
	first := within(t, capture.calls, "initial answer entry")
	if first.turn != opening {
		t.Fatalf("control: initial question=%q", first.turn)
	}
	w.send(map[string]any{"type": "session.output_transcript.delta", "delta": "What is its key?"})
	vendorSpeaks(t, w, r)
	r.hearNow("ABC-12", true)
	sendDelegation(w, "reply", 2000)
	within(t, v.calls, "reply delegation")
	reply := within(t, capture.calls, "reply answer entry")
	t.Logf("delegation-open reply: VoiceAsked=%q turn=%q reply=%q", reply.x.VoiceAsked, reply.turn, reply.x.Reply)
	if reply.x.VoiceAsked != opening {
		t.Errorf("VoiceAsked=%q, want earlier room question %q while original delegation is open", reply.x.VoiceAsked, opening)
	}
}

// This is a characterization, not a semantic guess: R10 explicitly treats a
// spoken line ending in '?' as asking. We log the propagated ownership for
// rhetorical/quoted questions and an intermediate question-mark delta whose
// completed speech no longer ends in a question.
func TestSol191R23AskingBoundary(t *testing.T) {
	const opening = "what is happening with the recorder?"
	for _, tc := range []struct {
		name   string
		deltas []string
	}{
		{"plain_statement_control", []string{"The recorder is on."}},
		{"quoted_test_string", []string{"The test expects the text: is it ready?"}},
		{"rhetorical", []string{"What could possibly go wrong?"}},
		{"complete_statement_single_delta_control", []string{"Is it ready? It is ready."}},
		{"question_then_final_statement", []string{"Is it ready?", " It is ready."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, v, w, capture := sol23Open(t, opening, nil)
			for _, delta := range tc.deltas {
				w.send(map[string]any{"type": "session.output_transcript.delta", "delta": delta})
			}
			vendorSpeaks(t, w, r)
			r.hearNow("ABC-12", true)
			sendDelegation(w, "reply", 1000)
			within(t, v.calls, "reply call")
			raw := within(t, v.results, "reply result")
			var got voice.AnswerResult
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			select {
			case call := <-capture.calls:
				t.Logf("spoken=%q VoiceAsked=%q reply=%q backend=%q", strings.Join(tc.deltas, ""), call.x.VoiceAsked, call.x.Reply, modelQuestion(r))
			default:
				t.Logf("spoken=%q side-talk before Concierge; say=%q", strings.Join(tc.deltas, ""), got.Say)
			}
			if tc.name == "plain_statement_control" && modelQuestion(r) != "" {
				t.Errorf("control: plain statement let bare key reach model: %q", modelQuestion(r))
			}
			if (tc.name == "complete_statement_single_delta_control" || tc.name == "question_then_final_statement") && modelQuestion(r) != "" {
				t.Errorf("completed non-question speech let bare-key side talk reach backend: question=%q holders=%q", modelQuestion(r), holderMessages(r))
			}
		})
	}
}
