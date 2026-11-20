package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/joined"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// Joined agents (R17): agents a person tells, through Dispatch, to join the
// meeting. The roster lives as long as the unit has a speaker for the voice;
// it hears agents only while the Concierge's Envoy session is open.

// newLiveKeyterms is the live stream's keyterms when the voice can run (the
// rule of voiceKeyterms: a speaker and a Nova-3 model), else nil: the wake
// phrases, the joined agents' keys and Dispatch's project keys.
func newLiveKeyterms(cfg *config.Config, projects func() []string) *transcript.LiveKeyterms {
	if voiceKeyterms(cfg, nil) == nil {
		return nil
	}
	return transcript.NewLiveKeyterms(cfg.Voice.WakePhrases, projects)
}

// joinedDeps is what the roster needs from the rest of the unit.
type joinedDeps struct {
	session   func() string // the recording session now
	store     *storage.SQLiteStore
	floor     joined.Floor
	dispatch  *dispatch.Client // nil with no Dispatch token or URL
	notify    func(kind, sessionID string)
	keyterms  *transcript.LiveKeyterms // nil when the stream takes none
	publicURL string
}

// startJoinedAgents starts the roster and its pointers and sweep, which run
// until ctx ends.
func startJoinedAgents(ctx context.Context, deps *joinedDeps) *joined.Roster {
	roster := joined.New(&joined.Config{
		Session: deps.session,
		Started: func(id string) time.Time {
			session, err := deps.store.GetSession(id)
			if err != nil {
				return time.Time{}
			}
			return session.StartedAt
		},
		Floor:  deps.floor,
		Title:  issueTitle(deps.dispatch),
		Notify: deps.notify,
		Keyterms: func(keys []string) {
			if deps.keyterms != nil {
				deps.keyterms.SetJoined(keys)
			}
		},
		PublicURL: deps.publicURL,
		Logf:      log.Printf,
	})
	go roster.Run(ctx)
	return roster
}

// issueTitle reads an issue's title from Dispatch, for a join; nil with no
// Dispatch.
func issueTitle(client *dispatch.Client) func(context.Context, string) (string, error) {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, key string) (string, error) {
		issue, err := client.Issue(ctx, key)
		if dispatch.IsNotFound(err) {
			return "", joined.ErrNoSuchIssue
		}
		if err != nil {
			return "", err
		}
		return issue.Title, nil
	}
}

// joinedAgentsOf is the roster as the Concierge lists it (list_joined_agents
// and issue_holder); nil with no roster.
func joinedAgentsOf(roster *joined.Roster) func() []concierge.JoinedAgent {
	if roster == nil {
		return nil
	}
	return func() []concierge.JoinedAgent {
		members := roster.Joined()
		agents := make([]concierge.JoinedAgent, 0, len(members))
		for _, m := range members {
			agents = append(agents, concierge.JoinedAgent{SessionID: m.Agent, Title: m.Title, IssueKey: m.IssueKey})
		}
		return agents
	}
}

// deliverTo is the Envoy session's Deliver: a join, a floor request or a
// leave goes to the roster, and every other message to the Concierge.
func deliverTo(roster *joined.Roster, c *concierge.Concierge) func(context.Context, *envoy.Message) error {
	if roster == nil {
		return c.Deliver
	}
	return func(ctx context.Context, message *envoy.Message) error {
		if handled, err := roster.Deliver(ctx, message); handled {
			return err
		}
		return c.Deliver(ctx, message)
	}
}

// joinedHooks reaches the roster from the session hooks, which are wired
// before it exists. Until set, it does nothing.
type joinedHooks struct {
	mu     sync.Mutex
	roster *joined.Roster
}

func (h *joinedHooks) set(roster *joined.Roster) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.roster = roster
}

func (h *joinedHooks) get() *joined.Roster {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.roster
}

// SessionEnded makes the agents of an ended meeting leave.
func (h *joinedHooks) SessionEnded(sessionID string) {
	if roster := h.get(); roster != nil {
		roster.SessionEnded(sessionID)
	}
}

// SessionsDeleted makes the agents of deleted meetings leave.
func (h *joinedHooks) SessionsDeleted(sessionIDs []string) {
	if roster := h.get(); roster != nil {
		roster.SessionsDeleted(sessionIDs)
	}
}

// OnSegment hands the roster a final segment of the room's words.
func (h *joinedHooks) OnSegment(s *voice.Segment) {
	if roster := h.get(); roster != nil {
		roster.OnSegment(s)
	}
}

// HandDone tells the roster how a raised hand came down.
func (h *joinedHooks) HandDone(hand *voice.Hand, outcome voice.HandOutcome) {
	if roster := h.get(); roster != nil {
		roster.HandDone(hand, outcome)
	}
}

// handRaiser is the voice as joined agents raise hands on it; the
// voice.Supervisor is one.
type handRaiser interface {
	RaiseHand(h *voice.Hand) error
	LowerHands(membership string)
}

// RaiseHand raises a joined agent's hand on the voice; with no voice, or the
// voice off, it is refused.
func (l *voiceLink) RaiseHand(h *voice.Hand) error {
	if v, ok := l.get().(handRaiser); ok {
		return v.RaiseHand(h)
	}
	return voice.ErrHandVoiceOff
}

// LowerHands lowers the hands of a joined agent's membership as it ends.
func (l *voiceLink) LowerHands(membership string) {
	if v, ok := l.get().(handRaiser); ok {
		v.LowerHands(membership)
	}
}

// wireJoinedRoutes gives the API the meeting's joined agents and the join
// message.
func wireJoinedRoutes(hooks *server.ControlHooks, roster *joined.Roster) {
	hooks.JoinedAgents = func(sessionID string) []server.JoinedAgent {
		var agents []server.JoinedAgent
		for _, m := range roster.Joined() {
			if m.SessionID == sessionID {
				agents = append(agents, server.JoinedAgent{Agent: m.Agent, IssueKey: m.IssueKey, Title: m.Title, JoinedAt: m.JoinedAt})
			}
		}
		return agents
	}
	hooks.JoinMessage = func(issueKey string) (string, error) {
		message, err := roster.Invitation(issueKey)
		if errors.Is(err, dispatch.ErrBadIssueKey) {
			return "", server.ErrBadIssueKey
		}
		return message, err
	}
}
