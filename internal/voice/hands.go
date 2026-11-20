package voice

// A joined agent's floor request (R17): the room hears the hand sound at the
// next gap (R10), and the agent's words wait, as a held answer that needs an
// invitation does, for a person's "Wispr, go ahead", until they expire.
// "Wispr, who has a hand up?" names the agents whose hands are up.

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"time"
)

// Hand is a joined agent's floor request.
type Hand struct {
	// ID names the request, such as the Envoy event that carried it.
	ID string
	// SessionID is the recording session it was raised in.
	SessionID string
	// Agent is the agent's Envoy session; LowerHands lowers its hands.
	Agent string
	// IssueKey is the agent's issue, which "Wispr, go ahead, KEY" names.
	IssueKey string
	// Name is how "Wispr, who has a hand up?" names the agent: its issue
	// key, else its title.
	Name string
	// Text is what the voice says, the agent named first.
	Text string
	// Expires is when it comes down unspoken.
	Expires time.Time
}

// HandOutcome says how a raised hand came down.
type HandOutcome int

const (
	HandSpoken    HandOutcome = iota + 1 // a person invited it, and it was spoken
	HandExpired                          // nobody invited it before it expired
	HandWithdrawn                        // its agent left, its meeting ended or the voice was turned off
)

func (o HandOutcome) String() string {
	switch o {
	case HandSpoken:
		return "spoken"
	case HandExpired:
		return "expired"
	default:
		return "withdrawn"
	}
}

// Segment is one final transcript segment of the room, for the joined
// agents' pointers: Wispr's own words never are one.
type Segment struct {
	SessionID string
	Text      string
	// Call: it is a call to Wispr, which reaches an agent through ask_agent
	// alone.
	Call bool
	// From and To are when it was said, as near as the transcript tells.
	From, To time.Time
}

// The reasons RaiseHand refuses a hand, which its agent is told.
var (
	ErrHandVoiceOff   = errors.New("the meeting voice is off")
	ErrHandNotMeeting = errors.New("its meeting is not the one recording now")
	ErrHandExpired    = errors.New("it expired before it reached the room")
)

// RaiseHand takes a joined agent's floor request: the hand sound plays at
// the next gap with no transcript word, and the words wait for a person's
// invitation until h.Expires. It refuses a hand with the voice off, for a
// meeting that is not the one recording, or that has expired. HandDone is
// told how the hand came down.
func (s *Supervisor) RaiseHand(h *Hand) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed || s.offLocked():
		return ErrHandVoiceOff
	case h.SessionID == "" || h.SessionID != s.cfg.Session():
		return ErrHandNotMeeting
	case !time.Now().Before(h.Expires):
		return ErrHandExpired
	}
	hand := *h // the Supervisor keeps its own copy
	s.pending = append(s.pending, &pendingAnswer{
		PendingAnswer: PendingAnswer{SessionID: h.SessionID, IssueKey: h.IssueKey, Text: h.Text},
		hand:          &hand,
		needsInvite:   true,
	})
	return nil
}

// LowerHands takes down the hands agent raised, as it leaves.
func (s *Supervisor) LowerHands(agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = slices.DeleteFunc(s.pending, func(p *pendingAnswer) bool {
		if p.hand == nil || p.hand.Agent != agent {
			return false
		}
		s.handDoneLocked(p, HandWithdrawn)
		return true
	})
}

// handDoneLocked tells HandDone how p, when it is a hand, came down.
func (s *Supervisor) handDoneLocked(p *pendingAnswer, outcome HandOutcome) {
	if p.hand == nil || s.cfg.HandDone == nil {
		return
	}
	hand, done := *p.hand, s.cfg.HandDone
	s.spawn(func() { done(&hand, outcome) })
}

// dropExpiredLocked takes down the hands that expired by now.
func (s *Supervisor) dropExpiredLocked(now time.Time) {
	s.pending = slices.DeleteFunc(s.pending, func(p *pendingAnswer) bool {
		if p.hand == nil || now.Before(p.hand.Expires) {
			return false
		}
		s.handDoneLocked(p, HandExpired)
		return true
	})
}

// handsUpLocked says, in an exchange of its own, which agents of meeting
// sessionID have a hand up, on "Wispr, who has a hand up?".
func (s *Supervisor) handsUpLocked(sessionID string) {
	s.dropEndedLocked()
	s.dropExpiredLocked(time.Now())
	if !s.cfg.Red() {
		return
	}
	var names []string
	for _, p := range s.pending {
		if p.hand != nil {
			if name := cmp.Or(p.hand.Name, p.hand.IssueKey, "An agent"); !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	s.speakLocked(&pendingAnswer{PendingAnswer: PendingAnswer{SessionID: sessionID, Text: handsLine(names)}, line: true})
}

// handsLine names the agents with a hand up.
func handsLine(names []string) string {
	switch len(names) {
	case 0:
		return "Nobody has a hand up."
	case 1:
		return names[0] + " has a hand up."
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " have a hand up."
	}
}

// heardOut records that p was spoken: a holder's answer in the ledger, a
// hand to its agent; a line such as the hands-up line has nothing to record.
// s.mu is not held.
func (s *Supervisor) heardOut(p *pendingAnswer) {
	switch {
	case p.line:
	case p.hand != nil:
		s.mu.Lock()
		s.handDoneLocked(p, HandSpoken)
		s.mu.Unlock()
	default:
		s.spoken(p.TaskID)
	}
}

// segmentLocked queues t, a final result of the room's words, for the Segment
// hook. It is a call when it holds a wake phrase, or when an exchange holds
// the floor, whose words are said to Wispr.
func (s *Supervisor) segmentLocked(t *Transcript) {
	if s.cfg.Segment == nil {
		return
	}
	_, wake := s.wake.Find(t.Text)
	from := t.At
	if n := len(t.Words); n > 0 {
		if said := t.Words[n-1].End - t.Words[0].Start; said > 0 {
			from = t.At.Add(-time.Duration(said * float64(time.Second)))
		}
	}
	s.segments = append(s.segments, Segment{SessionID: s.cfg.Session(), Text: t.Text, Call: wake || s.ex != nil, From: from, To: t.At})
}
