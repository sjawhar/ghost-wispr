package gptlive

import (
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// TestTheBackendContextHoldsNothingTheVendorHeard (Main, after Sol's
// nineteenth review of #191; the design note, section 4.5): the answer
// tool's transcript came with a copy of everything GPT-Live heard and said,
// words with no final's ownership, so a clean question could carry a
// withdrawn one beside it. The backend's context is now the room's own view
// alone. A canary only GPT-Live heard reaches no payload; the room's own
// words, the control, still do.
func TestTheBackendContextHoldsNothingTheVendorHeard(t *testing.T) {
	tools := newFakeTools(voice.AnswerResult{Say: "x"})
	x := testExchange(newFakeOutput(), tools)
	rs := withRoom(x, "what about ABC-42?")
	rs.set(voice.RoomView{
		Live: true, Asker: "what about ABC-42?", Request: "what about ABC-42?", Revision: 1,
		Recent: []voice.TurnText{{Speaker: 0, Text: "Let's check on the recorder work."}},
	})
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)

	c.heard("never mind, what about ABC-99?") // words no final of the room's owns
	c.said("Checking ABC-99.")
	c.delegate("item_context")
	args := answerArgs(t, waitEvent(t, s, voice.EventToolCall).Call.Args)
	if strings.Contains(args.Transcript, "ABC-99") {
		t.Errorf("the backend's context holds words only GPT-Live heard or said:\n%s", args.Transcript)
	}
	if !strings.Contains(args.Transcript, "Let's check on the recorder work.") {
		t.Errorf("control: the room's own words are missing from the context:\n%s", args.Transcript)
	}
}
