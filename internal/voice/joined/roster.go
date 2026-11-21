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
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
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
	// opening is the opening whose Envoy session the membership joined, or
	// last joined again, under, and agents that session: the opening's Close
	// ends it, and tells it through agents.
	opening uint64
	agents  Agents
}

// agentHistory is what the roster keeps of one agent session across its
// memberships: its rate limits, which a leave and a rejoin do not reset.
type agentHistory struct {
	lastFloor   time.Time
	lastPointer time.Time
}

// commandKey is a command's fixed-size record: the first 8 bytes of the
// SHA-256 of its agent session and Envoy event id, so a command costs the
// history the same however long its id. Keys are compared only within one
// session: two of its commands share a key by chance one time in 2^64, so
// with 4,096 held, a new command is mistaken for one held about once in 2^52.
type commandKey uint64

func commandKeyOf(agent, eventID string) commandKey {
	sum := sha256.Sum256([]byte(agent + "\x00" + eventID))
	return commandKey(binary.BigEndian.Uint64(sum[:8]))
}

// commandRecord is one command taken: its key, and the second it was taken,
// counted from the history's base and rounded up, so that it is kept at least
// replayWindow.
type commandRecord struct {
	key commandKey
	at  uint32
}

// sessionCommands is the commands one sending Envoy session had taken, oldest
// first, in a ring that grows to perSessionCommands, found through table: an
// open-addressing table, twice the ring's length, of ring places plus one (0
// for none), so a command is found at once, whatever the session holds.
type sessionCommands struct {
	agent string
	ring  []commandRecord
	head  int
	n     int
	table []uint16
	index int  // its place in commandHistory.largest
	full  bool // it reached perSessionCommands, and its oldest went for its newest; logged once a filling
}

func (s *sessionCommands) at(i int) *commandRecord { return &s.ring[(s.head+i)%len(s.ring)] }

// home is key's first slot in table; the key's bits are a hash already.
func (s *sessionCommands) home(key commandKey) int { return int(uint32(key)) & (len(s.table) - 1) }

// find is the slot of table that holds key's place, or -1.
func (s *sessionCommands) find(key commandKey) int {
	if len(s.table) == 0 {
		return -1
	}
	for i := s.home(key); ; i = (i + 1) & (len(s.table) - 1) {
		switch p := s.table[i]; {
		case p == 0:
			return -1
		case s.ring[p-1].key == key:
			return i
		}
	}
}

func (s *sessionCommands) has(key commandKey) bool { return s.find(key) >= 0 }

// place enters ring place p, holding key, in table.
func (s *sessionCommands) place(key commandKey, p int) {
	i := s.home(key)
	for s.table[i] != 0 {
		i = (i + 1) & (len(s.table) - 1)
	}
	s.table[i] = uint16(p + 1)
}

// unplace empties slot i of table, and moves back each entry after it that
// would no longer be found from its home past the empty slot.
func (s *sessionCommands) unplace(i int) {
	mask := len(s.table) - 1
	for {
		s.table[i] = 0
		j := i
		for {
			j = (j + 1) & mask
			if s.table[j] == 0 {
				return
			}
			k := s.home(s.ring[s.table[j]-1].key)
			// An entry whose home lies cyclically in (i, j] is found without
			// slot i; any other must move into it.
			if (i <= j && i < k && k <= j) || (i > j && (i < k || k <= j)) {
				continue
			}
			break
		}
		s.table[i] = s.table[j]
		i = j
	}
}

// push keeps rec as the newest; the caller made room below perSessionCommands.
func (s *sessionCommands) push(rec commandRecord) {
	if s.n == len(s.ring) {
		s.resize(min(max(8, 2*len(s.ring)), perSessionCommands))
	}
	p := (s.head + s.n) % len(s.ring)
	s.ring[p] = rec
	s.place(rec.key, p)
	s.n++
}

// dropOldest forgets the oldest record, and gives the ring's room back once a
// quarter of it is used.
func (s *sessionCommands) dropOldest() {
	s.unplace(s.find(s.ring[s.head].key))
	s.head = (s.head + 1) % len(s.ring)
	s.n--
	if len(s.ring) > 8 && s.n <= len(s.ring)/4 {
		s.resize(max(8, len(s.ring)/2))
	}
}

// resize makes the ring size long, its records in order from its start, and
// its table again.
func (s *sessionCommands) resize(size int) {
	ring := make([]commandRecord, size)
	for i := range s.n {
		ring[i] = *s.at(i)
	}
	s.ring, s.head, s.table = ring, 0, make([]uint16, 2*size)
	for p := range s.n {
		s.place(ring[p].key, p)
	}
}

// sessionsBySize orders the sending sessions most commands first, for the
// backstop (container/heap).
type sessionsBySize []*sessionCommands

func (q sessionsBySize) Len() int           { return len(q) }
func (q sessionsBySize) Less(i, j int) bool { return q[i].n > q[j].n }
func (q sessionsBySize) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *sessionsBySize) Push(x any) {
	s := x.(*sessionCommands)
	s.index = len(*q)
	*q = append(*q, s)
}
func (q *sessionsBySize) Pop() any {
	old := *q
	s := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	return s
}

// commandHistory is the commands the roster took, each kept for replayWindow
// from when it took it, so a command Envoy delivers again acts once. It keeps
// them by sending Envoy session, at most perSessionCommands each: a session
// at its cap forgets its own oldest for its newest, so a session that floods
// makes only its own commands replayable, never another's refused or
// forgotten. maxCommands bounds them all, as a backstop against many
// sessions: at it the history forgets the oldest of the session holding the
// most. It refuses no command, so none Envoy delivers is lost.
type commandHistory struct {
	base      time.Time
	sessions  map[string]*sessionCommands
	largest   sessionsBySize
	total     int
	full      bool   // the latest new command found the history at maxCommands; logged once a filling
	lastPrune uint32 // the second of the last whole prune: commands expire by the second
}

func newCommandHistory(base time.Time) commandHistory {
	return commandHistory{base: base, sessions: map[string]*sessionCommands{}}
}

// stamp is now as a second counted from base, rounded up.
func (h *commandHistory) stamp(now time.Time) uint32 {
	d := now.Sub(h.base)
	if d <= 0 {
		return 0
	}
	return uint32(min((d+time.Second-1)/time.Second, math.MaxUint32))
}

// expired reports whether a command taken at the second at has been kept the
// window at now.
func (h *commandHistory) expired(at uint32, now time.Time) bool {
	return now.Sub(h.base.Add(time.Duration(at)*time.Second)) >= replayWindow
}

// has reports whether agent's command key was taken.
func (h *commandHistory) has(agent string, key commandKey) bool {
	s := h.sessions[agent]
	return s != nil && s.has(key)
}

// size is the commands kept, sessionSize those of agent.
func (h *commandHistory) size() int { return h.total }

func (h *commandHistory) sessionSize(agent string) int {
	if s := h.sessions[agent]; s != nil {
		return s.n
	}
	return 0
}

// add keeps agent's command key, taken at now, and reports whether that made
// room by forgetting agent's own oldest (its cap) or, for the first time
// since the history last had room, another session's (the backstop).
func (h *commandHistory) add(agent string, key commandKey, now time.Time) (ownCap, backstop bool) {
	s := h.sessions[agent]
	if s == nil {
		s = &sessionCommands{agent: agent}
		h.sessions[agent] = s
		heap.Push(&h.largest, s)
	}
	h.pruneSession(s, now)
	switch {
	case s.n >= perSessionCommands:
		ownCap = !s.full
		s.full = true
		h.forgetOldest(s)
	case h.total >= maxCommands:
		if h.stamp(now) != h.lastPrune {
			h.prune(now)
		}
		if h.total >= maxCommands {
			backstop = !h.full
			h.full = true
			h.forgetOldest(h.largest[0])
		}
	default:
		h.full = false
	}
	if h.sessions[agent] == nil { // the backstop emptied it
		h.sessions[agent] = s
		heap.Push(&h.largest, s)
	}
	s.push(commandRecord{key: key, at: h.stamp(now)})
	h.total++
	heap.Fix(&h.largest, s.index)
	return ownCap, backstop
}

// forgetOldest forgets s's oldest command, and s itself once it holds none.
func (h *commandHistory) forgetOldest(s *sessionCommands) {
	s.dropOldest()
	h.total--
	if s.n < perSessionCommands/2 {
		s.full = false
	}
	if s.n == 0 {
		heap.Remove(&h.largest, s.index)
		delete(h.sessions, s.agent)
		return
	}
	heap.Fix(&h.largest, s.index)
}

// pruneSession forgets s's commands kept the window: its oldest first, which
// were taken first.
func (h *commandHistory) pruneSession(s *sessionCommands, now time.Time) {
	for s.n > 0 && h.sessions[s.agent] == s && h.expired(s.at(0).at, now) {
		h.forgetOldest(s)
	}
}

// prune forgets every command kept the window.
func (h *commandHistory) prune(now time.Time) {
	h.lastPrune = h.stamp(now)
	for _, s := range h.sessions { // forgetOldest may delete s: allowed while ranging
		h.pruneSession(s, now)
	}
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
	// asyncMu orders async's wg.Add against Close: closing, set under it,
	// stops async from starting anything once Close waits.
	asyncMu sync.Mutex
	closing bool

	mu     sync.Mutex
	agents Agents
	// open is the gate a join commits through: open while the unit has an
	// Envoy session. epoch counts the sessions installed and closed, so a
	// join that began under one Envoy session never commits under the next,
	// and a stop whose epoch has passed changes nothing. openings counts the
	// openings made (Open), and agentsOpening is the one whose session is
	// installed.
	open          bool
	epoch         int
	openings      uint64
	agentsOpening uint64
	seq           int // the last membership number
	// members are the memberships by agent session: the current one, or
	// one whose end has begun and whose cleanup has not finished.
	members  map[string]*member
	meetings map[string]*meeting // the open meetings agents joined, by recording session
	// history is each agent session's rate limits, across its memberships
	// (agentHistory): kept while one of them runs or while it is joined
	// (pruneHistoryLocked).
	history map[string]*agentHistory
	// commands are the commands taken within Envoy's replay window.
	commands commandHistory
	// ready, made while the roster has no Envoy session, is closed by the
	// SetAgents (Opening) that gives it one: a command waits on it
	// (notReadyWait).
	ready chan struct{}
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

// replayWindow is how long Envoy can deliver a command again. Its
// notification stream keeps a message 72 h (legion
// packages/envoy/internal/bus/stream.go: the stream's MaxAge is
// contracts.DeliveryDuplicateWindow, 259200000 ms), and no consumer, a
// re-registration's resume point included, reads what the stream no longer
// keeps. The roster remembers each command it took for that long, from when
// it took it by its own clock, which is no earlier than when Envoy stored it.
const replayWindow = 72 * time.Hour

// perSessionCommands bounds the commands the roster keeps for one sending
// Envoy session: nearly one a minute for all of Envoy's 72 h window, where an
// agent's joins, leaves and floor requests come a few an hour. A session past
// it replaces its own oldest, which Envoy could then deliver again.
const perSessionCommands = 4096

// historySessions is how many sending sessions at their cap the history is
// sized for; maxCommands, their product, bounds all the commands it keeps:
// about 20 MiB with every one of 256 sessions at its cap (20 bytes a command,
// TestTheCommandHistoryStaysCompact). Many sessions holding a command or two
// each cost more a command: each has a ring of at least 8 records, its table
// and its map entry. Past the bound the roster replaces the oldest of the
// session holding the most.
const (
	historySessions = 256
	maxCommands     = perSessionCommands * historySessions
)

// notReadyWait is how long a command that comes while the roster has no
// Envoy session waits for one before it is refused (ErrNotReady): the unit's
// session can deliver a frame while it still registers, a moment before an
// opening's SetAgents gives the roster the session, and such a command is
// taken rather than refused. It also spaces Envoy's redeliveries of a
// command the roster cannot take, whatever backoff Envoy itself applies.
const notReadyWait = 2 * time.Second

// ErrNotReady is Deliver's answer to a command that comes while the roster
// has no Envoy session and none is given within notReadyWait: before an
// opening's SetAgents gives it one, or once its Close took it away. The
// roster can neither take nor answer the command then, so it does not
// acknowledge it: the unit's Envoy session answers the frame nak, and Envoy
// delivers it again for as long as its stream keeps it. legion's relay
// spaces those deliveries 1, 2, 4, 8 and 16 s and then 30 s apart
// (session_relay.go, relayNakDelayFor), with no limit on their number (the
// session consumer's MaxDeliver is -1), and notReadyWait spaces them at
// least 2 s whatever the relay does.
var ErrNotReady = errors.New("joined: the roster has no Envoy session to take the command with")

// historyLocked is agent's history, made at its first rate limit.
func (r *Roster) historyLocked(agent string) *agentHistory {
	h := r.history[agent]
	if h == nil {
		h = &agentHistory{}
		r.history[agent] = h
	}
	return h
}

// pruneHistoryLocked forgets the commands Envoy can no longer deliver again,
// and the history of an agent with no limit running and no membership.
func (r *Roster) pruneHistoryLocked(now time.Time) {
	r.commands.prune(now)
	for agent, h := range r.history {
		if r.members[agent] == nil &&
			now.Sub(h.lastFloor) >= r.cfg.FloorEvery && now.Sub(h.lastPointer) >= r.cfg.PointerEvery {
			delete(r.history, agent)
		}
	}
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
	return &Roster{
		cfg: c, logf: logf, ctx: ctx, cancel: cancel,
		members: map[string]*member{}, meetings: map[string]*meeting{}, history: map[string]*agentHistory{},
		commands: newCommandHistory(c.Now()),
	}
}

// Opening is one opening of the unit's Envoy session for the roster, as one
// start of the meeting voice opens it (Open). Its SetAgents installs the
// session only while no later opening was made, and its Close ends only the
// memberships that joined under its own session: an earlier opening that
// completes, or closes, after a later one began touches no session or
// membership of the later one.
type Opening struct {
	r *Roster
	n uint64
}

// Open makes an opening of the unit's Envoy session, which supersedes every
// earlier one: an earlier opening's SetAgents installs nothing from now on.
func (r *Roster) Open() *Opening {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openings++
	return &Opening{r: r, n: r.openings}
}

// SetAgents gives the roster the opening's Envoy session once it opens,
// which opens the gate joins commit through, unless a later opening was made
// meanwhile: the session then belongs to no start of the voice, and is not
// installed. agents is never nil: Close ends the session.
func (o *Opening) SetAgents(agents Agents) {
	if agents == nil {
		panic("joined: SetAgents(nil); Close ends the Envoy session")
	}
	r := o.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if o.n != r.openings {
		r.logf("joined: an Envoy session opened after a later start of the voice began; the roster does not take it")
		return
	}
	r.epoch++
	r.open = true
	r.agents, r.agentsOpening = agents, o.n
	if r.ready != nil {
		close(r.ready)
		r.ready = nil
	}
}

// Close ends the opening's Envoy session for the roster, as it closes. While
// its session is the one installed, it shuts the gate first, so no join
// still in its reads commits. Then it ends every membership that joined
// under its session, and none other, and tells each agent the voice stopped
// through that session, while it can still send (tellStopped): all at once,
// within ctx. A close whose epoch a later SetAgents passed while it cleaned
// up changes nothing more.
func (o *Opening) Close(ctx context.Context) {
	r := o.r
	r.mu.Lock()
	installed := r.agentsOpening == o.n && r.agents != nil
	var epoch int
	if installed {
		r.epoch++
		epoch = r.epoch
		r.open = false
	}
	var gone []*member
	for _, m := range r.members {
		if !m.ending && m.opening == o.n {
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
	}
	r.tellStopped(ctx, gone)
	if !installed {
		return
	}
	r.mu.Lock()
	if r.epoch == epoch {
		r.agents = nil
	}
	r.mu.Unlock()
}

// tellStopped sends each of gone's agents the notice that the voice stopped,
// through the session it joined under (member.agents), each on a goroutine
// of its own, so the notices go out together. Each send runs within ctx,
// Close's end and sendTimeout. It waits for them within ctx; at ctx's end it
// waits no longer, and logs the agents not told. A send still under way then
// sees ctx's end too.
func (r *Roster) tellStopped(ctx context.Context, gone []*member) {
	if len(gone) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.ctx, cancel)()
	var mu sync.Mutex
	untold := make(map[string]bool, len(gone))
	for _, m := range gone {
		untold[m.Agent] = true
	}
	var sends sync.WaitGroup
	for _, m := range gone {
		sends.Add(1)
		go func() {
			defer sends.Done()
			if r.sendVia(ctx, m.agents, m.Agent, "", "notice", voiceStoppedText) {
				mu.Lock()
				delete(untold, m.Agent)
				mu.Unlock()
			}
		}()
	}
	sent := make(chan struct{})
	go func() {
		sends.Wait()
		close(sent)
	}()
	select {
	case <-sent:
	case <-ctx.Done():
	}
	if ctx.Err() == nil {
		return // every send ended in time; sendVia logged any that failed
	}
	mu.Lock()
	names := slices.Sorted(maps.Keys(untold))
	mu.Unlock()
	if len(names) > 0 {
		r.logf("joined: the stop ended before these agents were told the voice stopped: %s (%v)", strings.Join(names, ", "), context.Cause(ctx))
	}
}

// Close stops Run and the sends still going. Nothing async starts once it
// began: a hand's outcome the Supervisor reports during it is dropped, not
// raced with the wait.
func (r *Roster) Close() {
	r.asyncMu.Lock()
	r.closing = true
	r.cancel()
	r.asyncMu.Unlock()
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
	r.mu.Lock()
	ready := r.open || r.waitReadyLocked(ctx)
	r.mu.Unlock()
	if !ready {
		return true, ErrNotReady
	}
	now := r.cfg.Now() // no hook runs under r.mu
	r.mu.Lock()
	if !r.open { // taken away meanwhile
		r.mu.Unlock()
		return true, ErrNotReady
	}
	// A command Envoy delivers again was taken: it acts once, whatever the
	// agent's membership is now, for as long as Envoy can bring it back.
	var ownCap, backstop bool
	if m.EventID != "" {
		key := commandKeyOf(m.SourceSession, m.EventID)
		if r.commands.has(m.SourceSession, key) {
			r.mu.Unlock()
			r.logf("joined: a %s message already taken was delivered again", c.verb)
			return true, nil
		}
		ownCap, backstop = r.commands.add(m.SourceSession, key, now)
	}
	r.mu.Unlock()
	if ownCap {
		r.logf("joined: session %s sent %d commands within Envoy's window; each new one replaces its oldest, which Envoy could then deliver again", m.SourceSession, perSessionCommands)
	}
	if backstop {
		r.logf("joined: the roster keeps %d commands from all sessions; each new one replaces the oldest of the session holding the most", maxCommands)
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

// waitReadyLocked waits, without r.mu, up to notReadyWait for an opening's
// SetAgents to give the roster its Envoy session, and reports whether it
// did; ctx's end and Close end the wait too. r.mu is held on entry and on
// return.
func (r *Roster) waitReadyLocked(ctx context.Context) bool {
	timer := time.NewTimer(notReadyWait)
	defer timer.Stop()
	for !r.open {
		if r.ready == nil {
			r.ready = make(chan struct{})
		}
		ready := r.ready
		r.mu.Unlock()
		select {
		case <-ready:
		case <-timer.C:
			r.mu.Lock()
			return r.open
		case <-ctx.Done():
			r.mu.Lock()
			return r.open
		case <-r.ctx.Done():
			r.mu.Lock()
			return r.open
		}
		r.mu.Lock()
	}
	return true
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
		joined.opening, joined.agents = r.agentsOpening, r.agents
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
			opening:  r.agentsOpening,
			agents:   r.agents,
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

// Sweep forgets the history Envoy can no longer replay (pruneHistoryLocked),
// reads whether each joined agent is live in Envoy, and makes one leave that
// Envoy has listed as not live for NotLiveFor.
func (r *Roster) Sweep(ctx context.Context) {
	now := r.cfg.Now()
	r.mu.Lock()
	r.pruneHistoryLocked(now)
	r.mu.Unlock()
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

// sendVia sends as send does, through agents, and reports whether the send
// was made: a stop tells its agents through the session they joined under,
// never one opened since.
func (r *Roster) sendVia(ctx context.Context, agents Agents, agent, inReplyTo, kind, text string) bool {
	if agents == nil {
		r.logf("joined: no Envoy session to send a joined agent its %s", kind)
		return false
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
		return false
	}
	return true
}

// async runs f on a goroutine Close waits for, with a context Close ends;
// once Close began, it runs nothing.
func (r *Roster) async(f func(ctx context.Context)) {
	r.asyncMu.Lock()
	defer r.asyncMu.Unlock()
	if r.closing {
		return
	}
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
