// Package joined keeps the agents that joined the meeting now recording (R17
// of INFRA-4592). A person tells an agent through Dispatch to join, with a
// message that holds the unit's Envoy session and the floor protocol
// (Invitation). The agent joins, asks for the floor and leaves with Envoy
// messages to the unit's session, which the roster reads (Deliver). It sends a
// joined agent a pointer, a meeting and a time range with no words, when its
// issue comes up in the room (OnSegment), and raises its floor requests on
// the Supervisor as hands, which speak only when a person invites them. The
// agent reads the meeting's words from the recorder's own API, which honours
// deletions, never from an Envoy copy.
package joined

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/earcheck"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Defaults for Config's waits (R17).
const (
	// DefaultFloorExpiry is how long a floor request waits for an invitation.
	DefaultFloorExpiry = 60 * time.Second
	// DefaultFloorEvery is how often an agent may ask for the floor.
	DefaultFloorEvery = 5 * time.Minute
	// DefaultPointerEvery is how often an agent may get a pointer.
	DefaultPointerEvery = 60 * time.Second
	// DefaultNotLiveFor is how long Envoy may list a joined agent as not
	// live before it leaves: Envoy calls a session unresponsive after 5
	// minutes, and it is not dead then.
	DefaultNotLiveFor = 10 * time.Minute
)

const (
	// tickEvery is how often Run sends the pointers that waited for their
	// agent's 60 s to pass.
	tickEvery = 5 * time.Second
	// sweepEvery is how often Run reads whether each joined agent is live.
	sweepEvery = time.Minute
	// sendTimeout bounds one message to an agent.
	sendTimeout = 10 * time.Second
)

// ErrNoSuchIssue is Config.Title's answer for a key Dispatch has no issue
// of: a join on it is refused.
var ErrNoSuchIssue = errors.New("no such issue in Dispatch")

// Agents is the unit's Envoy session as the roster uses it; *envoy.Session is
// one.
type Agents interface {
	ID() string
	Lookup(ctx context.Context, id string) (envoy.SessionInfo, error)
	Send(ctx context.Context, request *envoy.SendRequest) (envoy.SendResult, error)
}

// Floor is the voice as the roster raises hands on it; *voice.Supervisor is
// one. A hand belongs to one membership (voice.Hand.Membership), and
// LowerHands takes down that membership's hands and no other.
type Floor interface {
	RaiseHand(h *voice.Hand) error
	LowerHands(membership string)
}

// Config configures a Roster.
type Config struct {
	// Session is the recording session now, "" while none records. The
	// roster reads it under its lock at each commit, so it must return at
	// once and never call the roster or the Supervisor.
	Session func() string
	// Meeting is the context of a recording session's meeting, which the
	// recorder ends in the same step that clears the session, before any
	// session-ended hook; nil leaves a membership's end to Session and the
	// hook. It must return at once and never call the roster.
	Meeting func(sessionID string) context.Context
	// Started is when recording session id started; zero when unknown.
	Started func(id string) time.Time
	// Floor raises the agents' hands.
	Floor Floor
	// Title reads an issue's title from Dispatch, or ErrNoSuchIssue; nil
	// names each agent by its Envoy title.
	Title func(ctx context.Context, key string) (string, error)
	// Notify sends the /ws events agent_joined and agent_left; nil sends none.
	Notify func(kind, sessionID string)
	// Keyterms is given the joined agents' issue keys on each change; nil
	// drops them.
	Keyterms func(keys []string)
	// PublicURL is the recorder's own address, where agents read the
	// transcript.
	PublicURL string
	// Now is the roster's clock; nil is time.Now.
	Now func() time.Time
	// FloorExpiry, FloorEvery, PointerEvery and NotLiveFor are the
	// defaults of those names when 0; a test shortens them.
	FloorExpiry, FloorEvery, PointerEvery, NotLiveFor time.Duration
	Logf                                              func(string, ...any)
}

// Member is one joined agent's membership: from its join to its leave, in
// one meeting. A join after a leave, or in the next meeting, is another.
type Member struct {
	ID        string // the membership, <agent>#<n>, unique while the unit runs
	Agent     string // its Envoy session
	IssueKey  string // its issue, "" when it named none
	Title     string // its issue's title, else its Envoy title
	SessionID string // the recording session it joined
	JoinedAt  time.Time
}

// meeting is one recording session's meeting, from its first join until it
// ends or is deleted; its context is the parent of its memberships'.
type meeting struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc
}

type member struct {
	Member
	name  string   // how the voice names it
	words []string // its title's words that count toward its issue coming up
	// ctx is the membership's context (liveContext): its Err reports it done
	// the moment the recorder no longer holds its meeting open, and an end
	// (its leave, its meeting's end or deletion, the voice stopping, a join
	// that replaces it) cancels it under r.mu before any slow cleanup.
	ctx    context.Context
	cancel context.CancelFunc
	// ending is set, under r.mu, as the roster begins the membership's end.
	// Its context may be done first, with the recorder's meeting; the
	// roster's cleanup then still runs, once.
	ending bool
	// issuedAt is when the join that made the membership was sent: a floor
	// request or a leave sent before it was for an earlier membership.
	issuedAt time.Time
	// waiting holds the mentions of its issue since the agent's last
	// pointer, waitFrom to waitTo, sent once PointerEvery passed.
	waiting          bool
	waitFrom, waitTo time.Time
	notLiveSince     time.Time // the first sweep that read it not live; only Envoy reading it live clears it
}

// agentHistory is what the roster keeps of one agent session across its
// memberships: the commands it took, so that one delivered again is a no-op,
// and the agent's rate limits, which a leave and a rejoin do not reset.
type agentHistory struct {
	events      []string // the EventIDs of its commands taken, newest last, at most maxEvents
	lastFloor   time.Time
	lastPointer time.Time
	touched     uint64 // when it was last used, for eviction
}

// liveContext is a context whose Err also reports it done the moment alive
// is false, and then cancels it: a membership's, whose alive reads whether
// the recorder still holds its meeting open, so that a check made right
// after the recorder ends the meeting sees it, before any hook runs.
type liveContext struct {
	context.Context
	cancel context.CancelFunc
	alive  func() bool
}

func (c *liveContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if !c.alive() {
		c.cancel()
		return context.Canceled
	}
	return nil
}

// pointer is one pointer to send for a membership, while it is current.
type pointer struct {
	member            *member
	key, sessionID    string
	from, to, started time.Time
}

// Roster is the agents that joined the meeting.
//
// Every step that acts after a slow read (a join after its title and Envoy
// reads, a floor request, a pointer after its start-time read, a sweep after
// its lookup) first reads the membership or the meeting it acts for, then,
// under r.mu, checks it is still current (currentLocked, meetingOpenLocked,
// and for a join the Envoy-session gate and its epoch) before acting. Every
// end (a leave, a meeting's end or deletion, the voice stopping) cancels the
// memberships it ends under r.mu as its first act, before any slow cleanup
// such as lowering hands. So nothing acts for a membership whose end has
// begun, and an end never takes a membership that began since.
//
// No hook runs under r.mu: not the Supervisor's RaiseHand or LowerHands,
// not Envoy, the store, Dispatch, the /ws notice nor the keyterms. The one
// read under r.mu is the recorder's current session (Config.Session), at
// each commit, which returns at once and calls nothing back. Each path
// decides under the lock and calls out after it; a step that called out
// checks again under the lock.
//
// Every effect on a membership runs under the membership's context, whose
// parent is its meeting's: its welcome, its pointers, its hand's outcome, a
// refusal to it while it is current, and its hand's words, which the
// Supervisor says under a context derived from voice.Hand.Context. An end
// cancels them wherever they have got to. Only three sends run under no
// membership: a refusal to an agent with no current membership, under the
// delivery's context; and the notice that a membership ended, which its end
// sends once it has cancelled it, under the roster's context (through the
// Envoy session it joined under when the voice stops).
type Roster struct {
	cfg    Config
	logf   func(string, ...any)
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	agents Agents
	// open is the gate a join commits through: open while the unit has an
	// Envoy session. epoch counts SetAgents calls, so a join that began
	// under one Envoy session never commits under the next, and a stop
	// whose epoch has passed changes nothing.
	open  bool
	epoch int
	seq   int // the last membership number
	// members are the memberships by agent session: the current one, or
	// one whose end has begun and whose cleanup has not finished.
	members  map[string]*member
	meetings map[string]*meeting // the open meetings agents joined, by recording session
	// history is each agent session's history, across its memberships, for
	// at most maxAgents agents (agentHistory).
	history map[string]*agentHistory
	touch   uint64
	// ended are the recording sessions that ended or were deleted, newest
	// last, at most maxEnded: a decision made from a slow read commits
	// nothing for one of them.
	ended []string
	// keySeq numbers the keyterm lists decided under r.mu, in order;
	// keyMu orders their delivery to Keyterms, after r.mu is released, so an
	// older list never lands after a newer one.
	keySeq     int
	keyMu      sync.Mutex
	keyApplied int
}

// maxEnded bounds the ended sessions the roster remembers; a unit records
// a few meetings a day.
const maxEnded = 64

// maxEvents bounds the commands remembered of one agent, and maxAgents the
// agents remembered with no membership: a meeting takes a few commands of
// each of a few agents.
const (
	maxEvents = 64
	maxAgents = 256
)

// historyLocked is agent's history, made at its first command.
func (r *Roster) historyLocked(agent string) *agentHistory {
	r.touch++
	h := r.history[agent]
	if h == nil {
		h = &agentHistory{}
		r.history[agent] = h
	}
	h.touched = r.touch
	for len(r.history) > maxAgents {
		oldest, found := "", false
		var at uint64
		for other, oh := range r.history {
			if r.members[other] != nil || (found && oh.touched >= at) {
				continue
			}
			oldest, at, found = other, oh.touched, true
		}
		if !found {
			break
		}
		delete(r.history, oldest)
	}
	return h
}

// meeting is the recorder's context of session id's meeting.
func (r *Roster) meeting(id string) context.Context {
	if r.cfg.Meeting == nil {
		return context.Background()
	}
	return r.cfg.Meeting(id)
}

// meetingOpenLocked reports whether sessionID is the meeting the recorder
// holds open now, read live, at the moment of the commit it guards. The
// roster's record of ended meetings, which every end writes under r.mu as
// its first act, only refuses sooner: it is bounded, so it is never the
// authority.
func (r *Roster) meetingOpenLocked(sessionID string) bool {
	return sessionID != "" && !slices.Contains(r.ended, sessionID) && sessionID == r.cfg.Session()
}

// currentLocked reports whether m is still its agent's membership, its end
// has not begun, and its meeting is the one the recorder holds open now: the
// check, against live state, that every step makes under r.mu at the moment
// it acts for m.
func (r *Roster) currentLocked(m *member) bool {
	return m != nil && r.members[m.Agent] == m && !m.ending && m.ctx.Err() == nil && r.meetingOpenLocked(m.SessionID)
}

// meetingLocked is sessionID's meeting, made at its first join; its context
// ends with the recorder's (recorder) and with the roster.
func (r *Roster) meetingLocked(sessionID string, recorder context.Context) *meeting {
	if mt := r.meetings[sessionID]; mt != nil {
		return mt
	}
	mt := &meeting{id: sessionID}
	mt.ctx, mt.cancel = context.WithCancel(r.ctx)
	context.AfterFunc(recorder, mt.cancel)
	r.meetings[sessionID] = mt
	return mt
}

// endLocked records the sessions of ids as ended.
func (r *Roster) endLocked(ids []string) {
	for _, id := range ids {
		if id != "" && !slices.Contains(r.ended, id) {
			r.ended = append(r.ended, id)
		}
	}
	if extra := len(r.ended) - maxEnded; extra > 0 {
		r.ended = slices.Delete(r.ended, 0, extra)
	}
}

// New returns an empty Roster; Run keeps its pointers and its sweep going.
func New(cfg *Config) *Roster {
	c := *cfg
	c.FloorExpiry = cmp.Or(c.FloorExpiry, DefaultFloorExpiry)
	c.FloorEvery = cmp.Or(c.FloorEvery, DefaultFloorEvery)
	c.PointerEvery = cmp.Or(c.PointerEvery, DefaultPointerEvery)
	c.NotLiveFor = cmp.Or(c.NotLiveFor, DefaultNotLiveFor)
	if c.Now == nil {
		c.Now = time.Now
	}
	logf := c.Logf
	if logf == nil {
		logf = log.Printf
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Roster{cfg: c, logf: logf, ctx: ctx, cancel: cancel, members: map[string]*member{}, meetings: map[string]*meeting{}, history: map[string]*agentHistory{}}
}

// SetAgents gives the roster the unit's Envoy session once it opens, which
// opens the gate joins commit through. nil, as it closes, shuts the gate
// first, so no join still in its reads commits, then ends every
// membership, telling each agent the voice stopped through the session it
// joined under, while that session can still send. A stop whose epoch a
// later SetAgents passed while it cleaned up changes nothing more.
func (r *Roster) SetAgents(agents Agents) {
	r.mu.Lock()
	r.epoch++
	epoch, closing := r.epoch, r.agents
	r.open = agents != nil
	if agents != nil {
		r.agents = agents
		r.mu.Unlock()
		return
	}
	var gone []*member
	for _, m := range r.members {
		if !m.ending {
			m.ending = true
			m.cancel()
			gone = append(gone, m)
		}
	}
	keys := r.keytermsLocked()
	r.mu.Unlock()
	r.applyKeyterms(keys)
	for _, m := range gone {
		r.finishLeave(m)
		r.sendVia(r.ctx, closing, m.Agent, "", "notice", voiceStoppedText)
	}
	r.mu.Lock()
	if r.epoch == epoch {
		r.agents = nil
	}
	r.mu.Unlock()
}

// Close stops Run and the sends still going.
func (r *Roster) Close() {
	r.cancel()
	r.wg.Wait()
}

// Run sends the pointers that waited and reads whether each joined agent is
// live, until ctx ends or Close.
func (r *Roster) Run(ctx context.Context) {
	tick, sweep := time.NewTicker(tickEvery), time.NewTicker(sweepEvery)
	defer tick.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.ctx.Done():
			return
		case <-tick.C:
			r.Tick()
		case <-sweep.C:
			r.Sweep(ctx)
		}
	}
}

// Joined lists the current memberships, in the order they joined.
func (r *Roster) Joined() []Member {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Member
	for _, m := range r.members {
		if r.currentLocked(m) {
			out = append(out, m.Member)
		}
	}
	slices.SortFunc(out, func(a, b Member) int { return cmp.Or(a.JoinedAt.Compare(b.JoinedAt), cmp.Compare(a.Agent, b.Agent)) })
	return out
}

// Invitation is the join message a person sends an agent through Dispatch to
// have it join on issue key ("" for none): the unit's Envoy session and the
// floor protocol. It fails while the unit has no Envoy session.
func (r *Roster) Invitation(key string) (string, error) {
	if key != "" {
		normalized, err := dispatch.NormalizeIssueKey(key)
		if err != nil {
			return "", err
		}
		key = normalized
	}
	r.mu.Lock()
	agents := r.agents
	r.mu.Unlock()
	if agents == nil {
		return "", errors.New("the unit has no Envoy session: the meeting voice is off, or Envoy is out of reach")
	}
	return invitation(agents.ID(), r.apiURL(), key), nil
}

// apiURL is where agents read the recorder's API.
func (r *Roster) apiURL() string {
	if r.cfg.PublicURL == "" {
		return "$GHOST_WISPR_HOST"
	}
	return strings.TrimRight(r.cfg.PublicURL, "/")
}

// Deliver takes one message to the unit's Envoy session when it is a join, a
// floor request or a leave, and reports whether it was one; any other message
// is the Concierge's. It answers the agent on the message's own reply thread.
func (r *Roster) Deliver(ctx context.Context, m *envoy.Message) (bool, error) {
	c, ok := parse(m.Text)
	if !ok {
		return false, nil
	}
	if m.SourceSession == "" {
		r.logf("joined: a %s message named no session it came from", c.verb)
		return true, nil
	}
	// A command Envoy delivers again was taken: it acts once, whatever the
	// agent's membership is now. One that came while the roster had no Envoy
	// session was not taken (it could not even be answered), so Envoy's next
	// delivery of it acts.
	if m.EventID != "" {
		r.mu.Lock()
		h := r.historyLocked(m.SourceSession)
		taken := slices.Contains(h.events, m.EventID)
		if !taken && r.open {
			h.events = append(h.events, m.EventID)
			if extra := len(h.events) - maxEvents; extra > 0 {
				h.events = slices.Delete(h.events, 0, extra)
			}
		}
		r.mu.Unlock()
		if taken {
			r.logf("joined: a %s message already taken was delivered again", c.verb)
			return true, nil
		}
	}
	switch c.verb {
	case verbJoin:
		r.join(ctx, m, c.key)
	case verbFloor:
		r.floor(ctx, m, c.draft)
	case verbLeave:
		r.leaveCommand(m)
	}
	return true, nil
}

func (r *Roster) join(ctx context.Context, m *envoy.Message, key string) {
	agent := m.SourceSession
	r.mu.Lock()
	epoch := r.epoch // the Envoy session this join began under
	r.mu.Unlock()
	sessionID := r.cfg.Session()
	if sessionID == "" {
		r.reply(ctx, m, "refused", noMeetingText)
		return
	}
	// The recorder's context of the meeting: a membership ends with it.
	recorder := r.meeting(sessionID)
	started := r.started(sessionID)
	if !started.IsZero() && !m.IssuedAt.IsZero() && m.IssuedAt.Before(started) {
		r.reply(ctx, m, "refused", staleJoinText)
		return
	}
	if key != "" {
		normalized, err := dispatch.NormalizeIssueKey(key)
		if err != nil {
			r.reply(ctx, m, "refused", fmt.Sprintf("%q is not an issue key, so you did not join. An issue key looks like ABC-12.", key))
			return
		}
		key = normalized
	}
	title := ""
	if key != "" && r.cfg.Title != nil {
		read, err := r.cfg.Title(ctx, key)
		switch {
		case errors.Is(err, ErrNoSuchIssue):
			r.reply(ctx, m, "refused", fmt.Sprintf("Dispatch has no issue %s, so you did not join. Join with your own issue's key.", key))
			return
		case err != nil:
			r.logf("joined: read the title of %s: %v", key, err)
		default:
			title = read
		}
	}
	// One lookup: a session Envoy lists as gone cannot be reached, so it
	// does not join; its Envoy title names an agent with no issue title.
	var envoyTitle string
	if agents := r.currentAgents(); agents != nil {
		info, err := agents.Lookup(ctx, agent)
		switch {
		case err != nil:
			r.logf("joined: read whether a joining agent is live: %v", err)
		case info.State == envoy.StateGone:
			r.reply(ctx, m, "refused", goneJoinText)
			return
		default:
			envoyTitle = info.Title
		}
	}
	title = cmp.Or(title, envoyTitle)
	name := key
	if name == "" {
		name = cmp.Or(speakable(title), "An agent")
	}
	now := r.cfg.Now()
	r.mu.Lock()
	// The reads above are slow: the voice may have stopped, and the meeting
	// may have ended or been deleted, while they ran.
	if !r.open || r.epoch != epoch {
		r.mu.Unlock()
		r.reply(ctx, m, "refused", joinStoppedText)
		return
	}
	if !r.meetingOpenLocked(sessionID) || recorder.Err() != nil {
		r.mu.Unlock()
		r.reply(ctx, m, "refused", meetingWentText)
		return
	}
	old := r.members[agent]
	joined := old
	var ended *member // a membership of the agent's this join ends
	again := r.currentLocked(old) && old.SessionID == sessionID
	if again {
		// A join again, or its redelivery: the same membership keeps its id,
		// its limits, its pointers and its time not live.
		joined.IssueKey, joined.Title, joined.name, joined.words = key, title, name, titleWords(title)
	} else {
		if old != nil && !old.ending {
			old.ending = true
			old.cancel() // its membership of a meeting no longer recording ends
			ended = old
		}
		r.seq++
		joined = &member{
			Member:   Member{ID: fmt.Sprintf("%s#%d", agent, r.seq), Agent: agent, IssueKey: key, Title: title, SessionID: sessionID, JoinedAt: now},
			name:     name,
			words:    titleWords(title),
			issuedAt: m.IssuedAt,
		}
		base, cancel := context.WithCancel(r.meetingLocked(sessionID, recorder).ctx)
		joined.ctx = &liveContext{Context: base, cancel: cancel, alive: func() bool {
			return recorder.Err() == nil && r.cfg.Session() == sessionID
		}}
		joined.cancel = cancel
		r.members[agent] = joined
	}
	keys := r.keytermsLocked()
	r.mu.Unlock()
	r.applyKeyterms(keys)
	if ended != nil {
		r.finishLeave(ended)
	}
	if !again {
		r.notify("agent_joined", sessionID)
	}
	r.logf("joined: an agent joined meeting %s (issue %q)", sessionID, key)
	// The welcome goes only to a membership that is still current as it goes,
	// and under its context: an end that comes while it is on its way to the
	// transport stops it there.
	if r.isCurrent(joined) {
		r.reply(joined.ctx, m, "welcome", welcome(joined, r.apiURL(), started))
	}
}

func (r *Roster) floor(ctx context.Context, m *envoy.Message, draft string) {
	agent := m.SourceSession
	now := r.cfg.Now()
	r.mu.Lock()
	joined := r.members[agent]
	if !r.currentLocked(joined) {
		r.mu.Unlock()
		r.reply(ctx, m, "refused", notJoinedText)
		return
	}
	if !m.IssuedAt.IsZero() && !joined.issuedAt.IsZero() && m.IssuedAt.Before(joined.issuedAt) {
		// Sent before the join that made this membership: it was an earlier
		// membership's, and never moves to this one.
		r.mu.Unlock()
		r.reply(joined.ctx, m, "refused", staleCommandText)
		return
	}
	// The Supervisor asks Current at the moment it accepts an invitation for
	// the hand and at the moment it says the words, and says them under the
	// membership's context: a membership whose end began by then is
	// withdrawn, never spoken, and an end while the words play stops them.
	hand := &voice.Hand{
		ID: m.EventID, SessionID: joined.SessionID, Agent: agent, Membership: joined.ID, IssueKey: joined.IssueKey, Name: joined.name,
		Current: func() bool { return r.isCurrent(joined) },
		Context: joined.ctx,
	}
	r.mu.Unlock()

	draft = strings.Join(strings.Fields(draft), " ")
	if draft == "" {
		r.reply(joined.ctx, m, "refused", "Your floor request holds no words, so it was not taken. Put what you would say on the lines after its first line.")
		return
	}
	// The room hears the attribution first, so the spoken-text check and its
	// limits are for the whole text.
	attribution := hand.Name + " says:"
	if hand.IssueKey != "" {
		attribution = hand.IssueKey + "'s agent says:"
	}
	hand.Text = attribution + " " + draft
	if reason := earcheck.Form(hand.Text); reason != "" {
		named := len(strings.Fields(attribution))
		r.reply(joined.ctx, m, "refused", fmt.Sprintf("Your words were not taken: the room hears %q first, and with it they have %s. Say it in at most %d words of your own, so 50 in all, in 3 sentences of 20 words at most (%d in your first), written for the ear, with no file paths, code, backticks or bars.", attribution, reason, 50-named, 20-named))
		return
	}
	raised := now
	if !m.IssuedAt.IsZero() && m.IssuedAt.Before(now) {
		raised = m.IssuedAt
	}
	hand.Expires = raised.Add(r.cfg.FloorExpiry)

	// Decided under r.mu on the membership read above while it is current,
	// with the agent's floor time reserved, so a second request waits out
	// the rate, a rejoin included; raised with r.mu released; then checked
	// again: a membership whose end began meanwhile has its hand lowered by
	// its own id, which touches no other's.
	r.mu.Lock()
	h := r.historyLocked(agent)
	switch {
	case !r.currentLocked(joined):
		r.mu.Unlock()
		r.reply(ctx, m, "refused", notJoinedText)
		return
	case !h.lastFloor.IsZero() && now.Sub(h.lastFloor) < r.cfg.FloorEvery:
		next := h.lastFloor.Add(r.cfg.FloorEvery)
		r.mu.Unlock()
		r.reply(joined.ctx, m, "refused", fmt.Sprintf("You may ask for the floor at most once in %s, so this request was not taken. You may ask again from %s UTC.", every(r.cfg.FloorEvery), next.UTC().Format("15:04:05")))
		return
	}
	prior := h.lastFloor
	h.lastFloor = now
	r.mu.Unlock()

	err := r.cfg.Floor.RaiseHand(hand)

	r.mu.Lock()
	current := r.currentLocked(joined)
	if err != nil && h.lastFloor.Equal(now) {
		h.lastFloor = prior // not taken: the reservation is undone
	}
	r.mu.Unlock()
	if err != nil {
		text := fmt.Sprintf("Your floor request was not taken: %v.", err)
		if errors.Is(err, voice.ErrHandExpired) {
			text = fmt.Sprintf("Your floor request reached the room more than %s after you sent it, so it was dropped. Ask again if the room still needs to hear it.", every(r.cfg.FloorExpiry))
		}
		r.reply(joined.ctx, m, "refused", text)
		return
	}
	if !current {
		r.cfg.Floor.LowerHands(joined.ID) // its end began while the hand went up
		return
	}
	r.logf("joined: an agent of meeting %s asked for the floor", hand.SessionID)
}

// leaveCommand ends the membership a leave message is for: the agent's
// current one, unless the leave was sent before the join that made it.
func (r *Roster) leaveCommand(m *envoy.Message) {
	r.mu.Lock()
	current := r.members[m.SourceSession]
	stale := current != nil && !m.IssuedAt.IsZero() && !current.issuedAt.IsZero() && m.IssuedAt.Before(current.issuedAt)
	r.mu.Unlock()
	if current == nil || stale {
		return // nothing to leave, or a leave of an earlier membership
	}
	r.leaveMember(m.SourceSession, current, "")
}

// leaveMember ends agent's membership when its end has not begun and it is
// want (any for nil): a decision made from an earlier read never ends a
// membership that began since. The membership is cancelled under r.mu
// first, then its hands are lowered.
func (r *Roster) leaveMember(agent string, want *member, reason string) {
	r.mu.Lock()
	m := r.members[agent]
	if m == nil || m.ending || (want != nil && m != want) {
		r.mu.Unlock()
		return
	}
	m.ending = true
	m.cancel()
	keys := r.keytermsLocked()
	r.mu.Unlock()
	r.applyKeyterms(keys)
	r.finishLeave(m)
	if reason != "" {
		r.async(func(ctx context.Context) { r.send(ctx, agent, "", "notice", reason) })
	}
}

// finishLeave is the slow cleanup of a membership already cancelled: its
// hands, and no other membership's, are lowered; it comes off the roster if
// no membership replaced it meanwhile; and the pages are told.
func (r *Roster) finishLeave(m *member) {
	if r.cfg.Floor != nil {
		r.cfg.Floor.LowerHands(m.ID)
	}
	r.mu.Lock()
	if r.members[m.Agent] == m {
		delete(r.members, m.Agent)
	}
	r.mu.Unlock()
	r.notify("agent_left", m.SessionID)
	r.logf("joined: an agent left meeting %s", m.SessionID)
}

// SessionEnded makes the agents of recording session id leave, as its meeting
// ended, and tells them.
func (r *Roster) SessionEnded(id string) { r.leaveMeetings([]string{id}, endedText) }

// SessionsDeleted makes the agents of the deleted sessions leave, and tells
// them.
func (r *Roster) SessionsDeleted(ids []string) { r.leaveMeetings(ids, deletedText) }

// leaveMeetings ends the meetings of ids: under r.mu it records them as
// ended, so a join or a segment still in its reads commits nothing for them,
// and cancels each meeting and the memberships it ends; only then does it
// lower their hands, one membership at a time.
func (r *Roster) leaveMeetings(ids []string, reason string) {
	r.mu.Lock()
	r.endLocked(ids)
	var gone []*member
	for _, m := range r.members {
		if !m.ending && slices.Contains(ids, m.SessionID) {
			m.ending = true
			m.cancel()
			gone = append(gone, m)
		}
	}
	for _, id := range ids {
		if mt := r.meetings[id]; mt != nil {
			mt.cancel() // every membership of it, whatever its cleanup's turn
			delete(r.meetings, id)
		}
	}
	keys := r.keytermsLocked()
	r.mu.Unlock()
	r.applyKeyterms(keys)
	for _, m := range gone {
		r.finishLeave(m)
		r.async(func(ctx context.Context) { r.send(ctx, m.Agent, "", "notice", reason) })
	}
}

// Sweep reads whether each joined agent is live in Envoy, and makes one leave
// that Envoy has listed as not live for NotLiveFor.
func (r *Roster) Sweep(ctx context.Context) {
	agents := r.currentAgents()
	if agents == nil {
		return
	}
	r.mu.Lock()
	read := make(map[string]*member, len(r.members))
	for agent, m := range r.members {
		if r.currentLocked(m) {
			read[agent] = m
		}
	}
	r.mu.Unlock()
	for agent, target := range read {
		info, err := agents.Lookup(ctx, agent)
		if err != nil {
			r.logf("joined: read whether a joined agent is live: %v", err)
			continue
		}
		now := r.cfg.Now()
		r.mu.Lock()
		// The lookup is slow: it counts only toward the membership it was
		// made for, while that membership is current.
		gone := false
		switch {
		case !r.currentLocked(target):
		case info.State == envoy.StateLive:
			target.notLiveSince = time.Time{}
		case target.notLiveSince.IsZero():
			target.notLiveSince = now
		default:
			gone = now.Sub(target.notLiveSince) >= r.cfg.NotLiveFor
		}
		r.mu.Unlock()
		if gone {
			r.leaveMember(agent, target, notLiveText)
		}
	}
}

// OnSegment reads a final segment of the room's words: each agent of its
// meeting whose issue it names gets a pointer, at most one in PointerEvery;
// a mention inside that time goes in the next pointer. A call to Wispr sends
// no pointer: it reaches an agent through ask_agent alone.
func (r *Roster) OnSegment(s *voice.Segment) {
	if s.Call || s.SessionID == "" {
		return
	}
	now := r.cfg.Now()
	var sends []*pointer
	r.mu.Lock()
	if !r.meetingOpenLocked(s.SessionID) {
		r.mu.Unlock()
		return // its meeting ended while the segment was on its way
	}
	for _, m := range r.members {
		if !r.currentLocked(m) || m.SessionID != s.SessionID || !m.comesUp(s.Text) {
			continue
		}
		h := r.historyLocked(m.Agent)
		if m.waiting || (!h.lastPointer.IsZero() && now.Sub(h.lastPointer) < r.cfg.PointerEvery) {
			if !m.waiting {
				m.waiting, m.waitFrom, m.waitTo = true, s.From, s.To
			}
			m.waitFrom, m.waitTo = minTime(m.waitFrom, s.From), maxTime(m.waitTo, s.To)
			continue
		}
		h.lastPointer = now
		sends = append(sends, &pointer{member: m, key: m.IssueKey, sessionID: m.SessionID, from: s.From, to: s.To})
	}
	r.mu.Unlock()
	r.sendPointers(sends)
}

// Tick sends each pointer that waited for its agent's PointerEvery to pass.
func (r *Roster) Tick() {
	now := r.cfg.Now()
	var sends []*pointer
	r.mu.Lock()
	for _, m := range r.members {
		if !r.currentLocked(m) || !m.waiting {
			continue
		}
		if h := r.historyLocked(m.Agent); now.Sub(h.lastPointer) >= r.cfg.PointerEvery {
			m.waiting, h.lastPointer = false, now
			sends = append(sends, &pointer{member: m, key: m.IssueKey, sessionID: m.SessionID, from: m.waitFrom, to: m.waitTo})
		}
	}
	r.mu.Unlock()
	r.sendPointers(sends)
}

// sendPointers sends each pointer while its membership is current: it reads
// when the meeting started (slow), then checks under r.mu before it starts
// the send, and again on the send's own goroutine right before it sends.
// The send runs under the membership's context, which an end cancels.
func (r *Roster) sendPointers(sends []*pointer) {
	for _, p := range sends {
		p.started = r.started(p.sessionID)
		if !r.isCurrent(p.member) {
			continue
		}
		text := pointerText(p, r.apiURL())
		r.async(func(context.Context) {
			if r.isCurrent(p.member) {
				r.send(p.member.ctx, p.member.Agent, "", "pointer", text)
			}
		})
	}
}

// isCurrent is currentLocked for a caller that does not hold r.mu: it takes
// r.mu briefly and calls no hook, so the Supervisor may ask it (a hand's
// Current) under its own lock.
func (r *Roster) isCurrent(m *member) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentLocked(m)
}

// HandDone tells an agent how its floor request came down: spoken, or
// dropped as nobody invited it. A hand withdrawn as its agent left or its
// meeting ended says nothing more.
func (r *Roster) HandDone(h *voice.Hand, outcome voice.HandOutcome) {
	var text string
	switch outcome {
	case voice.HandSpoken:
		text = spokenText
	case voice.HandExpired:
		text = fmt.Sprintf("Nobody invited your words within %s, so they were dropped. You may ask for the floor again once %s have passed since you asked.", every(r.cfg.FloorExpiry), every(r.cfg.FloorEvery))
	default:
		return
	}
	// Only the membership the hand belongs to is told, while it is current:
	// an agent that left, or joined again since, hears nothing of it.
	r.mu.Lock()
	m := r.members[h.Agent]
	current := m != nil && m.ID == h.Membership && r.currentLocked(m)
	r.mu.Unlock()
	if !current {
		return
	}
	r.async(func(context.Context) { r.send(m.ctx, h.Agent, h.ID, "hand "+outcome.String(), text) })
}

// reply answers message m on its thread, as a notice that asks no reply. Its
// idempotency key is m's and the kind's, so a redelivered message is
// answered once.
func (r *Roster) reply(ctx context.Context, m *envoy.Message, kind, text string) {
	r.send(ctx, m.SourceSession, m.EventID, kind, text)
}

// send sends agent text as a notice, in reply to inReplyTo when set, through
// the unit's Envoy session now.
func (r *Roster) send(ctx context.Context, agent, inReplyTo, kind, text string) {
	r.sendVia(ctx, r.currentAgents(), agent, inReplyTo, kind, text)
}

// sendVia sends as send does, through agents: a stop tells its agents through
// the session they joined under, never one opened since.
func (r *Roster) sendVia(ctx context.Context, agents Agents, agent, inReplyTo, kind, text string) {
	if agents == nil {
		r.logf("joined: no Envoy session to send a joined agent its %s", kind)
		return
	}
	key := uuid.NewString()
	if inReplyTo != "" {
		key = uuid.NewSHA1(uuid.NameSpaceOID, []byte("ghost-wispr joined "+inReplyTo+" "+kind)).String()
	}
	// The send's context reads its parent's Err at each check, so a
	// membership the recorder no longer holds open stops it at once.
	parent := ctx
	timed, cancel := context.WithTimeout(parent, sendTimeout)
	defer cancel()
	sendCtx := &liveContext{Context: timed, cancel: cancel, alive: func() bool { return parent.Err() == nil }}
	if _, err := agents.Send(sendCtx, &envoy.SendRequest{Target: agent, Message: text, NoReply: true, IdempotencyKey: key, InReplyTo: inReplyTo}); err != nil {
		r.logf("joined: send a joined agent its %s: %v", kind, err)
	}
}

// async runs f on a goroutine Close waits for, with a context Close ends.
func (r *Roster) async(f func(ctx context.Context)) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		f(r.ctx)
	}()
}

func (r *Roster) currentAgents() Agents {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agents
}

func (r *Roster) started(sessionID string) time.Time {
	if r.cfg.Started == nil {
		return time.Time{}
	}
	return r.cfg.Started(sessionID)
}

func (r *Roster) notify(kind, sessionID string) {
	if r.cfg.Notify != nil {
		r.cfg.Notify(kind, sessionID)
	}
}

// keyUpdate is a keyterm list decided under r.mu, numbered in the order it
// was decided.
type keyUpdate struct {
	seq  int
	keys []string
}

// keytermsLocked decides the issue keys of the current memberships, from the
// roster's live state, in the order they joined, and numbers the list. r.mu
// is held; applyKeyterms delivers the list once it is released. No caller's
// snapshot enters it, so a stale event can only recompute the live state.
func (r *Roster) keytermsLocked() keyUpdate {
	if r.cfg.Keyterms == nil {
		return keyUpdate{}
	}
	members := make([]*member, 0, len(r.members))
	for _, m := range r.members {
		if r.currentLocked(m) && m.IssueKey != "" {
			members = append(members, m)
		}
	}
	slices.SortFunc(members, func(a, b *member) int { return a.JoinedAt.Compare(b.JoinedAt) })
	keys := make([]string, 0, len(members))
	for _, m := range members {
		if !slices.Contains(keys, m.IssueKey) {
			keys = append(keys, m.IssueKey)
		}
	}
	r.keySeq++
	return keyUpdate{seq: r.keySeq, keys: keys}
}

// applyKeyterms gives Keyterms a list keytermsLocked decided, with r.mu
// released: a list decided before one already given is dropped, so the
// keyterms follow the joins and leaves in their order.
func (r *Roster) applyKeyterms(u keyUpdate) {
	if u.seq == 0 {
		return
	}
	r.keyMu.Lock()
	defer r.keyMu.Unlock()
	if u.seq <= r.keyApplied {
		return
	}
	r.keyApplied = u.seq
	r.cfg.Keyterms(u.keys)
}

// every writes d for a message: "60 s", "5 minutes".
func every(d time.Duration) string {
	if d >= 2*time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return fmt.Sprintf("%g s", d.Seconds())
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
