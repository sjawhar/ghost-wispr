package voice

import (
	"testing"
	"time"
)

// r28HearDrift is r27Hear for a result whose own start (Transcript.From) and
// first word start apart, as Deepgram's can: its words from wordStart, the
// result from resultStart (Sol's twenty-eighth review of #191, after
// https://developers.deepgram.com/docs/interim-results, where the result's
// start stays at 0 while its first word moves 1.41402 ms).
func r28HearDrift(r *rig, captured time.Time, text string, resultStart, wordStart float64, final, endpoint bool, speaker int) Transcript {
	tr := transcriptOf(text, wordStart, final, endpoint, 0)
	for i := range tr.Words {
		tr.Words[i].Speaker = speaker
		tr.Words[i].At = captured.Add(time.Duration(tr.Words[i].Start * float64(time.Second)))
	}
	tr.From = captured.Add(time.Duration(resultStart * float64(time.Second)))
	r.s.OnTranscript(&tr)
	return tr
}

// TestAFirstWordStandsInForAMissingResultStart: with no result start
// (Transcript.From), as a stream that carries none gives, a final replaces
// the pending interim when their first words start within sameResult; a
// final whose first word starts later, of another result, does not, and
// leaves the interim pending (Sol's twenty-eighth review of #191).
func TestAFirstWordStandsInForAMissingResultStart(t *testing.T) {
	for _, tc := range []struct {
		drift    time.Duration
		replaces bool
	}{
		{0, true},
		{time.Millisecond, true},
		{50 * time.Millisecond, false},
	} {
		t.Run(tc.drift.String(), func(t *testing.T) {
			r, _, _, captured := r27Open(t)
			hear := func(text string, start float64, final bool) Transcript {
				tr := transcriptOf(text, start, final, false, 0)
				for i := range tr.Words {
					tr.Words[i].Speaker = 4
					tr.Words[i].At = captured.Add(time.Duration(tr.Words[i].Start * float64(time.Second)))
				}
				r.s.OnTranscript(&tr) // no From: first words stand in
				return tr
			}
			interim := hear("Can you send these notes?", 10, false)
			final := hear("Send the report.", 10+tc.drift.Seconds(), true)
			if !final.endsAt().Before(interim.Words[len(interim.Words)-1].At) {
				t.Fatal("control: the final does not end before the interim's last word began")
			}
			r.s.mu.Lock()
			pending := r.s.turnOf(r.s.ex)
			r.s.mu.Unlock()
			want := "Send the report. Can you send these notes?"
			if tc.replaces {
				want = "Send the report."
			}
			if pending != want {
				t.Fatalf("drift %v: pending %q, want %q", tc.drift, pending, want)
			}
		})
	}
}
