package concierge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// A reply to a question GPT-Live asked by itself is read as a reply to
// Wispr's own question is (Main, after Sol's twenty-second review of #191):
// Sol's TestSol191R22ClarificationKeyReply shows a key said alone answers the
// question first asked. These are its other two edges.

// TestSideTalkAfterGPTLivesOwnQuestionIsNotAnswered: after GPT-Live's own
// "Anything else?", "okay, let's move on to the realtime voice budget" picks
// nothing, declines nothing, says no key and asks nothing, so it is side talk,
// as after Wispr's own question: the Concierge's model is not called, no
// holder is asked, and the voice gets no words to say. (A decline, "no,
// thanks", is answered "Okay.", as after Wispr's own question.)
func TestSideTalkAfterGPTLivesOwnQuestionIsNotAnswered(t *testing.T) {
	r, v, w := openVoiceBOn(t, "what is happening with the recorder?")
	w.send(map[string]any{"type": "session.output_transcript.delta", "delta": "The recorder is on. Anything else?"})
	vendorSpeaks(t, w, r)
	r.hearNow("okay, let's move on to the realtime voice budget", true)
	sendDelegation(w, "reply", 1000)
	within(t, v.calls, "the reply's call")
	var answer voice.AnswerResult
	if err := json.Unmarshal(within(t, v.results, "the reply's result"), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Say != "" || lastPrompt(r.rig) != "" || len(holderMessages(r)) != 0 {
		t.Errorf("side talk after GPT-Live's own question was answered: say %q, model prompt %t, holders %q", answer.Say, lastPrompt(r.rig) != "", holderMessages(r))
	}
}

// TestAKeyInTheFollowUpWindowIsSideTalkWithoutAQuestion is the control: when
// GPT-Live asked nothing, a key said alone in the follow-up window is side
// talk, as it was before, so only the voice's own question changes how the
// reply is read.
func TestAKeyInTheFollowUpWindowIsSideTalkWithoutAQuestion(t *testing.T) {
	r, v, w := openVoiceBOn(t, "what is happening with the recorder?")
	w.send(map[string]any{"type": "session.output_transcript.delta", "delta": "The recorder is on."})
	vendorSpeaks(t, w, r)
	r.hearNow("ABC-12", true)
	sendDelegation(w, "reply", 1000)
	within(t, v.calls, "the reply's call")
	var answer voice.AnswerResult
	if err := json.Unmarshal(within(t, v.results, "the reply's result"), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Say != "" || lastPrompt(r.rig) != "" {
		t.Errorf("a key with no question before it was answered: say %q, model prompt %t", answer.Say, lastPrompt(r.rig) != "")
	}
	time.Sleep(50 * time.Millisecond)
}
