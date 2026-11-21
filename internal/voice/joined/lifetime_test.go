package joined

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// The tests here hold the roster to the recorder's own end of a meeting, to
// an agent's history across its memberships, and to its commands' identity:
// a redelivered command never acts again.

// recorderMeetings is the recorder as the roster and the Supervisor read it:
// the session now, and each session's meeting context, which moveTo cancels
// in the same step that moves the session on, as session.Manager does. No
// session-ended hook runs.
type recorderMeetings struct {
	mu      sync.Mutex
	cur     string
	ctx     context.Context
	cancel  context.CancelFunc
	endedCx context.Context
}

func newRecorderMeetings(cur string) *recorderMeetings {
	ended, end := context.WithCancel(context.Background())
	end()
	m := &recorderMeetings{cur: cur, endedCx: ended}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

func (m *recorderMeetings) session() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

func (m *recorderMeetings) meeting(id string) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" || id != m.cur {
		return m.endedCx
	}
	return m.ctx
}

func (m *recorderMeetings) moveTo(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancel()
	m.cur = id
	m.ctx, m.cancel = context.WithCancel(context.Background())
}

// heldPointerAgents is the unit's Envoy session with each pointer held before
// it reaches the transport, until released or its context ends.
type heldPointerAgents struct {
	*fakeAgents
	entered, release chan struct{}
	once             sync.Once
}

func (a *heldPointerAgents) Send(ctx context.Context, m *envoy.SendRequest) (envoy.SendResult, error) {
	if strings.Contains(m.Message, "came up in the meeting") {
		a.once.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-ctx.Done():
			return envoy.SendResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return envoy.SendResult{}, err
	}
	return a.fakeAgents.Send(ctx, m)
}

// TestAPointerInTransportEndsWithTheRecordersMeeting fails if a pointer held
// before the transport still goes out once the recorder ended its meeting,
// with no session-ended hook and no roster call in between; a meeting still
// recording is the control.
func TestAPointerInTransportEndsWithTheRecordersMeeting(t *testing.T) {
	for _, ends := range []bool{false, true} {
		t.Run(map[bool]string{false: "the meeting records on", true: "the recorder ends the meeting"}[ends], func(t *testing.T) {
			rec := newRecorderMeetings("sess-1")
			r := newRig(t, func(cfg *Config) { cfg.Session, cfg.Meeting = rec.session, rec.meeting })
			a := &heldPointerAgents{fakeAgents: r.agents, entered: make(chan struct{}), release: make(chan struct{})}
			r.roster.SetAgents(a)
			r.join(t, "agent-1", "ABC-12")
			r.roster.OnSegment(r.segment("ABC-12 needs attention", false))
			<-a.entered
			if ends {
				rec.moveTo("sess-2")
			}
			close(a.release)
			r.roster.wg.Wait()
			sent := len(r.pointers("agent-1"))
			if !ends && sent != 1 {
				t.Fatal("control: the pointer of a meeting still recording was not sent")
			}
			if ends && sent != 0 {
				t.Fatal("a pointer reached the transport after the recorder ended its meeting")
			}
		})
	}
}

// TestAHandsWordsEndWithTheRecordersMeeting fails if a hand's words, held in
// the voice's synthesis, reach the speaker once the recorder ended their
// meeting, with no session-ended hook and no roster call in between; a
// meeting still recording is the control.
func TestAHandsWordsEndWithTheRecordersMeeting(t *testing.T) {
	for _, ends := range []bool{false, true} {
		t.Run(map[bool]string{false: "the meeting records on", true: "the recorder ends the meeting"}[ends], func(t *testing.T) {
			rec := newRecorderMeetings("sess-1")
			sp := &frameSpeaker{frames: make(chan []byte, 8)}
			entered, release := make(chan context.Context, 1), make(chan struct{})
			var roster atomic.Pointer[Roster]
			s := voice.NewSupervisor(&voice.SupervisorConfig{
				Voice:     func() (voice.Voice, error) { return slowVoice{entered: entered, release: release}, nil },
				Concierge: quietConcierge{}, Speaker: sp, WakePhrases: []string{"Wispr"},
				Session: rec.session, Meeting: rec.meeting, Red: func() bool { return true },
				SoundGap: 50 * time.Millisecond, SilenceWait: 100 * time.Millisecond,
				HandDone: func(h *voice.Hand, o voice.HandOutcome) {
					if r := roster.Load(); r != nil {
						r.HandDone(h, o)
					}
				},
				Logf: t.Logf,
			})
			t.Cleanup(s.Close)
			r := newRig(t, func(cfg *Config) {
				cfg.Now, cfg.Floor = nil, s
				cfg.Session, cfg.Meeting = rec.session, rec.meeting
			})
			roster.Store(r.roster)
			r.clock.set(time.Now())
			r.join(t, "agent-1", "ABC-12")
			r.deliver(t, "agent-1", "ghost-wispr floor\nThe release is ready.")
			(&supervisorRig{t: t, s: s}).heard("Wispr, go ahead.")
			select {
			case <-entered:
			case <-time.After(testWait):
				t.Fatal("control: the voice never began to say the words")
			}
			if ends {
				rec.moveTo("sess-2")
			}
			close(release)
			var frame []byte
			select {
			case frame = <-sp.frames:
			case <-time.After(250 * time.Millisecond):
			}
			if !ends && len(frame) == 0 {
				t.Fatal("control: a live hand's words reached no speaker")
			}
			if ends && len(frame) != 0 {
				t.Fatal("a frame of a hand's words reached the speaker after the recorder ended its meeting")
			}
		})
	}
}

// TestARedeliveredCommandIsANoOp fails if a join, a leave or a floor request
// that Envoy delivers again acts again: an old join after its leave, an old
// leave against the agent's next membership and its hand, or an old floor
// request in the next meeting. A fresh command is the control.
func TestARedeliveredCommandIsANoOp(t *testing.T) {
	t.Run("join", func(t *testing.T) {
		r := newRig(t, nil)
		join := r.deliver(t, "agent-1", "ghost-wispr join ABC-12")
		r.deliver(t, "agent-1", "ghost-wispr leave")
		if _, err := r.roster.Deliver(context.Background(), join); err != nil {
			t.Fatal(err)
		}
		if got := r.roster.Joined(); len(got) != 0 {
			t.Fatalf("the redelivered join made a membership after its leave: %+v", got)
		}
		r.deliver(t, "agent-1", "ghost-wispr join ABC-12")
		if len(r.roster.Joined()) != 1 {
			t.Fatal("control: a fresh join was refused")
		}
	})
	t.Run("leave", func(t *testing.T) {
		r := newRig(t, nil)
		r.join(t, "agent-1", "ABC-12")
		leave := r.deliver(t, "agent-1", "ghost-wispr leave")
		r.join(t, "agent-1", "ABC-12")
		current := r.roster.Joined()[0]
		r.deliver(t, "agent-1", "ghost-wispr floor\nThe release is ready.")
		if _, err := r.roster.Deliver(context.Background(), leave); err != nil {
			t.Fatal(err)
		}
		if got := r.roster.Joined(); len(got) != 1 || got[0].ID != current.ID || !r.floor.handUp("agent-1") {
			t.Fatalf("the redelivered leave ended the next membership or its hand: %+v", got)
		}
	})
	t.Run("floor", func(t *testing.T) {
		r := newRig(t, nil)
		r.join(t, "agent-1", "ABC-12")
		floor := r.deliver(t, "agent-1", "ghost-wispr floor\nThe old meeting has its answer.")
		r.roster.SessionEnded("sess-1")
		r.setSession("sess-2")
		r.clock.advance(10 * time.Minute)
		r.join(t, "agent-1", "ABC-12")
		if _, err := r.roster.Deliver(context.Background(), floor); err != nil {
			t.Fatal(err)
		}
		if got := len(r.floor.hands()); got != 1 {
			t.Fatalf("the redelivered floor request raised a hand in the next meeting: %d hands in all", got)
		}
		r.deliver(t, "agent-1", "ghost-wispr floor\nThe new meeting has its answer.")
		if len(r.floor.hands()) != 2 {
			t.Fatal("control: a fresh floor request was refused")
		}
	})
}

// TestACommandTheRosterCouldNotTakeActsWhenDeliveredAgain fails if a join
// that arrived before the roster had its Envoy session, so it could take
// nothing and answer nothing, is acknowledged (Deliver returns nil, and Envoy
// answers it done and moves its resume point past it) or remembered as taken:
// Deliver must refuse it with ErrNotReady, so Envoy delivers it again, and
// delivered again once the roster holds the session, it must join. A join
// taken once and delivered again is the control: it joins once.
func TestACommandTheRosterCouldNotTakeActsWhenDeliveredAgain(t *testing.T) {
	r := newRig(t, nil)
	r.roster.CloseAgents(context.Background())
	join := &envoy.Message{EventID: "e-early", SourceSession: "agent-1", Text: "ghost-wispr join ABC-12", IssuedAt: r.clock.Now()}
	if _, err := r.roster.Deliver(context.Background(), join); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a join with no Envoy session returned %v, want ErrNotReady so that Envoy delivers it again", err)
	}
	if len(r.roster.Joined()) != 0 {
		t.Fatal("control: a join with no Envoy session made a membership")
	}
	r.roster.SetAgents(r.agents)
	if _, err := r.roster.Deliver(context.Background(), join); err != nil {
		t.Fatal(err)
	}
	if got := r.roster.Joined(); len(got) != 1 {
		t.Fatalf("the join delivered again once the roster had its Envoy session made %d memberships, want 1", len(got))
	}
	if _, err := r.roster.Deliver(context.Background(), join); err != nil {
		t.Fatal(err)
	}
	if got := r.roster.Joined(); len(got) != 1 || got[0].ID != "agent-1#1" {
		t.Fatalf("control: the join taken and delivered again acted again: %+v", got)
	}
}

// TestAnAgentsRatesSurviveARejoin fails if an agent that leaves and joins
// again may ask for the floor again within 5 minutes, or get a second pointer
// within 60 s; once those pass, the next is taken.
func TestAnAgentsRatesSurviveARejoin(t *testing.T) {
	t.Run("floor", func(t *testing.T) {
		r := newRig(t, nil)
		r.join(t, "agent-1", "ABC-12")
		r.deliver(t, "agent-1", "ghost-wispr floor\nThe first request is ready.")
		r.deliver(t, "agent-1", "ghost-wispr leave")
		r.clock.advance(time.Second)
		r.join(t, "agent-1", "ABC-12")
		r.deliver(t, "agent-1", "ghost-wispr floor\nThe second request is ready.")
		if got := len(r.floor.hands()); got != 1 {
			t.Fatalf("a rejoin made %d floor requests within 5 minutes, want 1", got)
		}
		r.clock.advance(5 * time.Minute)
		r.deliver(t, "agent-1", "ghost-wispr floor\nThe later request is ready.")
		if len(r.floor.hands()) != 2 {
			t.Fatal("control: a request after 5 minutes was refused")
		}
	})
	t.Run("pointer", func(t *testing.T) {
		r := newRig(t, nil)
		r.join(t, "agent-1", "ABC-12")
		r.roster.OnSegment(r.segment("ABC-12 first mention", false))
		r.roster.wg.Wait()
		r.deliver(t, "agent-1", "ghost-wispr leave")
		r.clock.advance(time.Second)
		r.join(t, "agent-1", "ABC-12")
		r.roster.OnSegment(r.segment("ABC-12 second mention", false))
		r.roster.wg.Wait()
		if got := len(r.pointers("agent-1")); got != 1 {
			t.Fatalf("a rejoin got %d pointers within 60 s, want 1", got)
		}
		r.clock.advance(time.Minute)
		r.roster.Tick()
		r.roster.wg.Wait()
		if len(r.pointers("agent-1")) != 2 {
			t.Fatal("control: the mention held for the 60 s was not sent once they passed")
		}
	})
}
