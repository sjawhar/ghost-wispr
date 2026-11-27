package voice

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestADrivingVoicesAnswerGetsTheRoomsRecentWordsAsTheyAreNow (Sol's
// eleventh review of #191, a diagnostic): a voice that drives its turn calls
// the answer tool as often as the room asks, long after it opened. At
// 83aaf8b each answer got the room's recent words as they were when the
// exchange opened, words older than the two minutes kept (recentKept)
// included. The answer tool now hands the words kept as they are. Control:
// words within the two minutes reach the answer.
func TestADrivingVoicesAnswerGetsTheRoomsRecentWordsAsTheyAreNow(t *testing.T) {
	const note = "the old note from before"
	for _, aged := range []bool{false, true} {
		name := "control: within the two minutes"
		if aged {
			name = "past the two minutes"
		}
		t.Run(name, func(t *testing.T) {
			c := newFakeConcierge("ABC-12 is in review.")
			r := newDrivingRigWith(t, time.Minute, c)
			r.heard(note, true, false)
			r.heard("Wispr, what is happening with ABC-12?", true, true)
			sess := r.waitSession()
			if aged {
				r.s.mu.Lock()
				for i := range r.s.recent {
					r.s.recent[i].At = time.Now().Add(-recentKept - time.Second)
				}
				r.s.mu.Unlock()
			}
			if _, err := sess.x.Tools.Run(t.Context(), ToolCall{ID: "d1", Name: AnswerTool, Args: json.RawMessage(`{"question":"what is happening with ABC-12?"}`)}); err != nil {
				t.Fatalf("the answer tool: %v", err)
			}
			<-c.asked
			c.mu.Lock()
			x := c.exchanges[len(c.exchanges)-1]
			c.mu.Unlock()
			held := false
			for _, line := range x.Recent {
				held = held || strings.Contains(line.Text, note)
			}
			if held == aged {
				t.Errorf("the answer's recent words %+v hold the note: %v, want %v", x.Recent, held, !aged)
			}
		})
	}
}
