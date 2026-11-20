package joined

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests here carry the names of c-joined's checks in INFRA-4592's
// verification document (R17).

// TestJoinAndLeaveMessages fails if the roster does not follow a join
// message, a leave message, the end of the recorder session and 10 minutes
// not live in Envoy, each with its /ws event and the keyterms; or if a join
// with no meeting recording is taken; or if the join message Sami sends does
// not hold the unit's Envoy session and a floor protocol the roster reads.
func TestJoinAndLeaveMessages(t *testing.T) {
	r := newRig(t, nil)

	// A join: listed, told where to read, its key a keyterm.
	r.join(t, "agent-1", "ABC-12")
	members := r.roster.Joined()
	if len(members) != 1 || members[0].IssueKey != "ABC-12" || members[0].Title != titles["ABC-12"] || members[0].SessionID != "sess-1" {
		t.Fatalf("roster %+v, want agent-1 on ABC-12, titled from Dispatch, in sess-1", members)
	}
	welcome := r.agents.to("agent-1")[0]
	if !welcome.NoReply || welcome.InReplyTo == "" {
		t.Fatalf("the join's answer %+v, want a notice in reply to the join", welcome)
	}
	for _, want := range []string{"sess-1", "https://recorder.example/api/sessions/sess-1", "ghost-wispr floor", "ghost-wispr leave"} {
		if !strings.Contains(welcome.Message, want) {
			t.Errorf("the join's answer does not say %q:\n%s", want, welcome.Message)
		}
	}
	if got := r.notes.lastKeyterms(); !slices.Equal(got, []string{"ABC-12"}) {
		t.Fatalf("keyterms after the join %q, want [ABC-12]", got)
	}

	// A leave message.
	r.deliver(t, "agent-1", "ghost-wispr leave")
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("roster after a leave %+v, want none", got)
	}
	if got := r.notes.lastKeyterms(); len(got) != 0 {
		t.Fatalf("keyterms after the leave %q, want none", got)
	}
	if !slices.Contains(r.floor.loweredFor(), "agent-1") {
		t.Fatal("the leave did not lower the agent's hand")
	}

	// The end of the recorder session.
	r.join(t, "agent-1", "ABC-12")
	before := len(r.agents.to("agent-1"))
	r.roster.SessionEnded("sess-1")
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("roster after its meeting ended %+v, want none", got)
	}
	eventually(t, "the agent told that the meeting ended", func() bool {
		sent := r.agents.to("agent-1")
		return len(sent) > before && strings.Contains(sent[len(sent)-1].Message, "meeting ended")
	})

	// 10 minutes not live in Envoy, from the first sweep that read it so.
	r.setSession("sess-2")
	r.join(t, "agent-1", "ABC-12")
	r.agents.setState("agent-1", envoy.SessionInfo{SessionID: "agent-1", State: envoy.StateUnresponsive})
	r.roster.Sweep(context.Background())
	r.clock.advance(9*time.Minute + 59*time.Second)
	r.roster.Sweep(context.Background())
	if len(r.roster.Joined()) != 1 {
		t.Fatal("the agent left before 10 minutes not live")
	}
	r.clock.advance(time.Second)
	r.roster.Sweep(context.Background())
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("roster after 10 minutes not live %+v, want none", got)
	}

	wantEvents := []string{
		"agent_joined:sess-1", "agent_left:sess-1",
		"agent_joined:sess-1", "agent_left:sess-1",
		"agent_joined:sess-2", "agent_left:sess-2",
	}
	if got := r.notes.all(); !slices.Equal(got, wantEvents) {
		t.Fatalf("/ws events %q, want %q", got, wantEvents)
	}

	// No meeting recording: refused, and told why.
	r.setSession("")
	before = len(r.agents.to("agent-2"))
	r.deliver(t, "agent-2", "ghost-wispr join XYZ-9")
	if len(r.roster.Joined()) != 0 {
		t.Fatal("an agent joined with no meeting recording")
	}
	eventually(t, "the refusal", func() bool { return len(r.agents.to("agent-2")) > before })
	if refusal := r.agents.to("agent-2")[before].Message; !strings.Contains(refusal, "No meeting is recording") {
		t.Fatalf("refusal %q, want it to say no meeting is recording", refusal)
	}

	// Any other message is not the roster's.
	if handled, _ := r.roster.Deliver(context.Background(), &envoy.Message{EventID: "evt-x", SourceSession: "agent-3", Text: "INFRA-1027 ships on Friday."}); handled {
		t.Fatal("the roster took a message that is no join, floor request or leave")
	}

	// The join message Sami sends holds the unit's session and the protocol,
	// and the roster reads each command it teaches.
	r.setSession("sess-3")
	text, err := r.roster.Invitation("ABC-12")
	if err != nil {
		t.Fatalf("Invitation: %v", err)
	}
	for _, want := range []string{"unit-session", "https://recorder.example", "ABC-12", "50 words", "60 s", "5 minutes"} {
		if !strings.Contains(text, want) {
			t.Errorf("the join message does not say %q:\n%s", want, text)
		}
	}
	commands := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if c, ok := parse(line); ok {
			commands[c.verb] = line
		}
	}
	for _, verb := range []string{verbJoin, verbFloor, verbLeave} {
		if _, ok := commands[verb]; !ok {
			t.Errorf("the join message teaches no %s line the roster reads:\n%s", verb, text)
		}
	}
	if c, _ := parse(commands[verbJoin]); c.key != "ABC-12" {
		t.Errorf("the join line it teaches joins %q, want ABC-12", c.key)
	}
}

// TestPointerHoldsNoWords fails if the pointer an agent gets when its issue
// comes up holds a word of what was said, or lacks the meeting and the time
// range to read it at.
func TestPointerHoldsNoWords(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	said := "the zeppelin quorum of ABC-12 slipped past Thursday"
	r.roster.OnSegment(r.segment(said, false))
	eventually(t, "a pointer", func() bool { return len(r.pointers("agent-1")) == 1 })
	pointer := r.pointers("agent-1")[0]
	for _, word := range strings.Fields(said) {
		if word == "ABC-12" || len(word) < 4 {
			continue
		}
		if strings.Contains(strings.ToLower(pointer.Message), strings.ToLower(word)) {
			t.Errorf("the pointer holds the word %q of what was said:\n%s", word, pointer.Message)
		}
	}
	for _, want := range []string{"sess-1", "10:04:57", "10:05:00", "4:57", "5:00", "https://recorder.example/api/sessions/sess-1"} {
		if !strings.Contains(pointer.Message, want) {
			t.Errorf("the pointer does not say %q:\n%s", want, pointer.Message)
		}
	}
	if !pointer.NoReply {
		t.Error("the pointer asks the agent for a reply")
	}
}

// TestTheVoiceStoppingTellsJoinedAgents fails if the agents stay joined once
// the unit's Envoy session closes, as the voice stops, or are not told so
// before it closes.
func TestTheVoiceStoppingTellsJoinedAgents(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	before := len(r.agents.to("agent-1"))
	r.roster.SetAgents(nil)
	if got := r.roster.Joined(); len(got) != 0 {
		t.Fatalf("roster %+v once the Envoy session closed, want none", got)
	}
	sent := r.agents.to("agent-1")
	if len(sent) != before+1 || !strings.Contains(sent[before].Message, "voice stopped") {
		t.Fatalf("the agent was sent %+v, want one notice that the voice stopped", sent[before:])
	}
	if got := r.notes.all(); got[len(got)-1] != "agent_left:sess-1" {
		t.Fatalf("/ws events %q, want agent_left last", got)
	}
}

// TestIssueComesUp fails if a segment that names the agent's key, in any form
// people say it, or two words of its title of 5 letters or more, sends no
// pointer, or if a segment with one such word, or with another key, does.
func TestIssueComesUp(t *testing.T) {
	for _, tc := range []struct {
		key, said string
		want      bool
	}{
		{"ABC-12", "we merged ABC-12 this morning", true},
		{"ABC-12", "what about abc 12 then", true},
		{"ABC-12", "what about abc twelve then", true},
		{"INFRA-1027", "how is 1027 going", true},
		{"INFRA-1027", "is the ghost recorder done", true},
		{"ABC-12", "the agents in this meeting are quiet", true},
		{"ABC-12", "which agent wants the floor", true},
		{"ABC-12", "the floor is wet", false},
		{"ABC-12", "the meeting about XYZ-9 ran long", false},
		{"ABC-12", "we ship ABC-120 next", false},
		{"ABC-12", "we were 12 people", false},
		{"INFRA-1027", "they would rather keep their recorder", false},
	} {
		t.Run(tc.said, func(t *testing.T) {
			r := newRig(t, nil)
			r.join(t, "agent-1", tc.key)
			r.roster.OnSegment(r.segment(tc.said, false))
			if tc.want {
				eventually(t, "a pointer", func() bool { return len(r.pointers("agent-1")) == 1 })
				return
			}
			never(t, "a pointer for a segment that does not name the issue", 200*time.Millisecond, func() bool { return len(r.pointers("agent-1")) > 0 })
		})
	}
}

// TestPointerRate fails if a joined agent gets more than one pointer in 60 s,
// or a mention inside the 60 s is lost instead of sent when they end.
func TestPointerRate(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	r.roster.OnSegment(r.segment("ABC-12 is late", false))
	eventually(t, "the first pointer", func() bool { return len(r.pointers("agent-1")) == 1 })
	for range 3 {
		r.clock.advance(15 * time.Second)
		r.roster.OnSegment(r.segment("ABC-12 again", false))
		r.roster.Tick()
	}
	never(t, "a second pointer within 60 s", 200*time.Millisecond, func() bool { return len(r.pointers("agent-1")) > 1 })
	r.clock.advance(15 * time.Second)
	r.roster.Tick()
	eventually(t, "the second pointer once 60 s passed", func() bool { return len(r.pointers("agent-1")) == 2 })
	second := r.pointers("agent-1")[1].Message
	// The mentions at 15, 30 and 45 s after the first, each said over 3 s.
	for _, want := range []string{"10:05:12", "10:05:45"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second pointer does not span the mentions since the first (%s):\n%s", want, second)
		}
	}
	r.roster.Tick()
	never(t, "a third pointer with no new mention", 200*time.Millisecond, func() bool { return len(r.pointers("agent-1")) > 2 })
}

// TestFloorRequests fails if a floor request does not raise a hand that names
// the agent first, if a request is taken from an agent that did not join, a
// second within 5 minutes, or a draft past the spoken-text bar; or if the
// agent is not told how its hand came down.
func TestFloorRequests(t *testing.T) {
	r := newRig(t, nil)
	r.join(t, "agent-1", "ABC-12")
	request := r.deliver(t, "agent-1", "ghost-wispr floor\nThe deploy of the new keys moved to Friday.")
	hands := r.floor.hands()
	if len(hands) != 1 {
		t.Fatalf("raised %d hands, want 1", len(hands))
	}
	h := hands[0]
	want := voice.Hand{
		ID: request.EventID, SessionID: "sess-1", Agent: "agent-1", IssueKey: "ABC-12", Name: "ABC-12",
		Text: "ABC-12's agent says: The deploy of the new keys moved to Friday.", Expires: request.IssuedAt.Add(60 * time.Second),
	}
	if h != want {
		t.Fatalf("hand %+v, want %+v", h, want)
	}

	refused := func(text, reason string) {
		t.Helper()
		before := len(r.agents.to("agent-1"))
		r.deliver(t, "agent-1", text)
		if len(r.floor.hands()) != 1 {
			t.Fatalf("a floor request %q raised a hand", text)
		}
		eventually(t, "the refusal", func() bool { return len(r.agents.to("agent-1")) > before })
		if got := r.agents.to("agent-1")[before].Message; !strings.Contains(got, reason) {
			t.Fatalf("refusal %q, want it to say %q", got, reason)
		}
	}
	r.clock.advance(4*time.Minute + 59*time.Second)
	refused("ghost-wispr floor\nOne more thing about the keys.", "5 minutes")
	r.clock.advance(time.Second)
	refused("ghost-wispr floor\n"+strings.Repeat("word ", 51), "50 words")
	refused("ghost-wispr floor\nSee `deploy/keys.sh` for the steps.", "backtick")
	refused("ghost-wispr floor", "no words")
	r.deliver(t, "agent-1", "ghost-wispr floor\nOne more thing about the keys.")
	if got := len(r.floor.hands()); got != 2 {
		t.Fatalf("a request 5 minutes after the first raised %d hands in all, want 2", got)
	}

	before := len(r.agents.to("agent-2"))
	r.deliver(t, "agent-2", "ghost-wispr floor\nI have news.")
	eventually(t, "the refusal of an agent that did not join", func() bool { return len(r.agents.to("agent-2")) > before })
	if got := r.agents.to("agent-2")[before].Message; !strings.Contains(got, "ghost-wispr join") {
		t.Fatalf("refusal %q, want it to say how to join", got)
	}

	// How the hand came down.
	for _, tc := range []struct {
		outcome voice.HandOutcome
		says    string
	}{{voice.HandExpired, "dropped"}, {voice.HandSpoken, "spoken"}} {
		before := len(r.agents.to("agent-1"))
		r.roster.HandDone(h, tc.outcome)
		eventually(t, "the agent told", func() bool { return len(r.agents.to("agent-1")) > before })
		told := r.agents.to("agent-1")[before]
		if !strings.Contains(told.Message, tc.says) || told.InReplyTo != h.ID {
			t.Fatalf("told %+v, want a reply to the request that says %q", told, tc.says)
		}
	}
}

// TestFloorRequestOnTheVoice fails if a floor request, end to end through a
// real Supervisor, does not play the hand sound, does not drop when it
// expires with its agent told, or if "Wispr, who has a hand up?" does not make
// the voice say the agent's key.
func TestFloorRequestOnTheVoice(t *testing.T) {
	sup := newSupervisorRig(t)
	r := newRig(t, func(cfg *Config) {
		cfg.Now = nil // the Supervisor's clock: hands expire by time.Now
		cfg.Floor = sup.s
		cfg.Session = func() string { return "sess-1" }
		cfg.FloorExpiry = 400 * time.Millisecond
	})
	sup.mu.Lock()
	sup.handDone = r.roster.HandDone
	sup.mu.Unlock()
	r.clock.set(time.Now()) // the floor request is sent now
	r.join(t, "agent-1", "ABC-12")
	request := r.deliver(t, "agent-1", "ghost-wispr floor\nThe deploy moved to Friday.")
	eventually(t, "the hand sound", func() bool { return sup.speaker.played("hand") })

	sup.heard("Wispr, who has a hand up?")
	if got, want := sup.said(t), "ABC-12 has a hand up."; got != want {
		t.Fatalf("the voice said %q, want %q", got, want)
	}

	eventually(t, "the agent told its hand expired", func() bool {
		return slices.ContainsFunc(r.agents.to("agent-1"), func(m envoy.SendRequest) bool {
			return m.InReplyTo == request.EventID && strings.Contains(m.Message, "dropped")
		})
	})
	sup.heard("Wispr, go ahead.")
	never(t, "an expired hand was spoken", 300*time.Millisecond, func() bool { return sup.saidAnything() })
}
