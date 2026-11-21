package joined

import (
	"context"
	"slices"
	"testing"
)

// TestAnEarlierOpeningTouchesNothingOfALaterOne fails if an opening of the
// unit's Envoy session made before a later one, completing or closing after
// the later one began, installs its session over the later one's, ends a
// membership that joined under the later one, or shuts the later one's
// gate; or if its Close does not end the memberships that joined under its
// own session, and tell them through it. The control opens, closes and
// opens again in order. From review 6006965341 of #138.
func TestAnEarlierOpeningTouchesNothingOfALaterOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		// installedFirst says the earlier opening's session was installed,
		// and an agent joined under it, before the later opening began.
		installedFirst bool
		inOrder        bool
	}{
		{name: "control: open, close and open again in order", installedFirst: true, inOrder: true},
		{name: "the earlier opening completes after the later one joined an agent"},
		{name: "the earlier opening's session had an agent when the later one began", installedFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.opening.Close(context.Background()) // the rig's own opening
			earlier := r.roster.Open()
			older, newer := newFakeAgents(), newFakeAgents()
			if tc.installedFirst {
				earlier.SetAgents(older)
				r.agents = older
				r.join(t, "agent-old", "ABC-12")
			}
			if tc.inOrder {
				earlier.Close(context.Background())
			}
			later := r.roster.Open()
			later.SetAgents(newer)
			r.agents = newer
			r.join(t, "agent-new", "XYZ-9")
			if !tc.installedFirst {
				earlier.SetAgents(older) // completes late
			}
			if !tc.inOrder {
				earlier.Close(context.Background()) // closes late
			}
			r.roster.wg.Wait()
			if got := r.roster.currentAgents(); got != Agents(newer) {
				t.Fatalf("after the earlier opening the roster holds the earlier session: %t, no session: %t; want the later opening's session", got == Agents(older), got == nil)
			}
			var agents []string
			for _, m := range r.roster.Joined() {
				agents = append(agents, m.Agent)
			}
			if !slices.Equal(agents, []string{"agent-new"}) {
				t.Fatalf("joined after the earlier opening's close: %v, want only agent-new", agents)
			}
			if _, err := r.roster.Invitation("ABC-12"); err != nil {
				t.Fatalf("the later opening's gate was shut: %v", err)
			}
			for _, m := range newer.to("agent-new") {
				if m.Message == voiceStoppedText {
					t.Fatal("agent-new, of the later opening, was told the voice stopped")
				}
			}
			if tc.installedFirst {
				got := older.to("agent-old")
				if len(got) == 0 || got[len(got)-1].Message != voiceStoppedText {
					t.Fatalf("agent-old was not told, through the session it joined under, that the voice stopped: %+v", got)
				}
			}
		})
	}
}
