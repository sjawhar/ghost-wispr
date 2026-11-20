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
var ErrNoSuchIssue = errors.New("Dispatch has no such issue")

// Agents is the unit's Envoy session as the roster uses it; *envoy.Session is
// one.
type Agents interface {
	ID() string
	Lookup(ctx context.Context, id string) (envoy.SessionInfo, error)
	Send(ctx context.Context, request *envoy.SendRequest) (envoy.SendResult, error)
}

// Floor is the voice as the roster raises hands on it; *voice.Supervisor is
// one.
type Floor interface {
	RaiseHand(h voice.Hand) error
	LowerHands(agent string)
}

// Config configures a Roster.
type Config struct {
	// Session is the recording session now, "" while none records.
	Session func() string
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

// Member is one joined agent.
type Member struct {
	Agent     string // its Envoy session
	IssueKey  string // its issue, "" when it named none
	Title     string // its issue's title, else its Envoy title
	SessionID string // the recording session it joined
	JoinedAt  time.Time
}

type member struct {
	Member
	name  string   // how the voice names it
	words []string // its title's words that count toward its issue coming up
	// lastPointer is when it was last sent a pointer; waiting holds the
	// mentions since, waitFrom to waitTo, sent once PointerEvery passed.
	lastPointer      time.Time
	waiting          bool
	waitFrom, waitTo time.Time
	lastFloor        time.Time
	floorEvent       string    // the last floor request taken, so a redelivery is no second one
	notLiveSince     time.Time // the first sweep that read it not live
}

// pointer is one pointer to send.
type pointer struct {
	agent, key, sessionID string
	from, to, started     time.Time
}

// Roster is the agents that joined the meeting.
type Roster struct {
	cfg    Config
	logf   func(string, ...any)
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	agents  Agents
	members map[string]*member // by agent session
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
	return &Roster{cfg: c, logf: logf, ctx: ctx, cancel: cancel, members: map[string]*member{}}
}

// SetAgents gives the roster the unit's Envoy session once it opens; nil
// when it closed, and every joined agent leaves, as it hears nothing more.
func (r *Roster) SetAgents(agents Agents) {
	r.mu.Lock()
	r.agents = agents
	var gone []string
	if agents == nil {
		for agent := range r.members {
			gone = append(gone, agent)
		}
	}
	r.mu.Unlock()
	for _, agent := range gone {
		r.leave(agent, "")
	}
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

// Joined lists the agents that joined the meeting now recording, in the
// order they joined.
func (r *Roster) Joined() []Member {
	session := r.cfg.Session()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Member
	for _, m := range r.members {
		if session != "" && m.SessionID == session {
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
	switch c.verb {
	case verbJoin:
		r.join(ctx, m, c.key)
	case verbFloor:
		r.floor(ctx, m, c.draft)
	case verbLeave:
		r.leave(m.SourceSession, "")
	}
	return true, nil
}

func (r *Roster) join(ctx context.Context, m *envoy.Message, key string) {
	agent := m.SourceSession
	sessionID := r.cfg.Session()
	if sessionID == "" {
		r.reply(ctx, m, "refused", noMeetingText)
		return
	}
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
	if title == "" {
		if agents := r.currentAgents(); agents != nil {
			if info, err := agents.Lookup(ctx, agent); err == nil {
				title = info.Title
			}
		}
	}
	name := key
	if name == "" {
		name = cmp.Or(speakable(title), "An agent")
	}
	now := r.cfg.Now()
	r.mu.Lock()
	old := r.members[agent]
	joined := &member{Member: Member{Agent: agent, IssueKey: key, Title: title, SessionID: sessionID, JoinedAt: now}, name: name, words: titleWords(title)}
	again := old != nil && old.SessionID == sessionID
	if again {
		// A join again, or its redelivery: the same meeting keeps its
		// limits.
		joined.JoinedAt, joined.lastPointer, joined.lastFloor, joined.floorEvent = old.JoinedAt, old.lastPointer, old.lastFloor, old.floorEvent
	}
	r.members[agent] = joined
	r.keytermsLocked()
	r.mu.Unlock()
	if old != nil && !again {
		r.notify("agent_left", old.SessionID)
	}
	if !again {
		r.notify("agent_joined", sessionID)
	}
	r.logf("joined: an agent joined meeting %s (issue %q)", sessionID, key)
	r.reply(ctx, m, "welcome", welcome(joined, r.apiURL(), started))
}

func (r *Roster) floor(ctx context.Context, m *envoy.Message, draft string) {
	agent := m.SourceSession
	now := r.cfg.Now()
	r.mu.Lock()
	joined := r.members[agent]
	if joined == nil || joined.SessionID != r.cfg.Session() {
		r.mu.Unlock()
		r.reply(ctx, m, "refused", notJoinedText)
		return
	}
	if joined.floorEvent == m.EventID {
		r.mu.Unlock()
		return // a redelivery of the request already taken
	}
	if !joined.lastFloor.IsZero() && now.Sub(joined.lastFloor) < r.cfg.FloorEvery {
		next := joined.lastFloor.Add(r.cfg.FloorEvery)
		r.mu.Unlock()
		r.reply(ctx, m, "refused", fmt.Sprintf("You may ask for the floor at most once in %s, so this request was not taken. You may ask again from %s UTC.", every(r.cfg.FloorEvery), next.UTC().Format("15:04:05")))
		return
	}
	hand := voice.Hand{ID: m.EventID, SessionID: joined.SessionID, Agent: agent, IssueKey: joined.IssueKey, Name: joined.name}
	r.mu.Unlock()

	draft = strings.Join(strings.Fields(draft), " ")
	if draft == "" {
		r.reply(ctx, m, "refused", "Your floor request holds no words, so it was not taken. Put what you would say on the lines after its first line.")
		return
	}
	if reason := earcheck.Form(draft); reason != "" {
		r.reply(ctx, m, "refused", fmt.Sprintf("Your words were not taken: they have %s. Say it in at most 50 words, 3 sentences and 20 words a sentence, written for the ear, with no file paths, code, backticks or bars.", reason))
		return
	}
	if hand.IssueKey != "" {
		hand.Text = hand.IssueKey + "'s agent says: " + draft
	} else {
		hand.Text = hand.Name + " says: " + draft
	}
	raised := now
	if !m.IssuedAt.IsZero() && m.IssuedAt.Before(now) {
		raised = m.IssuedAt
	}
	hand.Expires = raised.Add(r.cfg.FloorExpiry)
	if err := r.cfg.Floor.RaiseHand(hand); err != nil {
		text := fmt.Sprintf("Your floor request was not taken: %v.", err)
		if errors.Is(err, voice.ErrHandExpired) {
			text = fmt.Sprintf("Your floor request reached the room more than %s after you sent it, so it was dropped. Ask again if the room still needs to hear it.", every(r.cfg.FloorExpiry))
		}
		r.reply(ctx, m, "refused", text)
		return
	}
	r.mu.Lock()
	if current := r.members[agent]; current == joined {
		joined.lastFloor, joined.floorEvent = now, m.EventID
	}
	r.mu.Unlock()
	r.logf("joined: an agent of meeting %s asked for the floor", hand.SessionID)
}

// leave takes agent off the roster and lowers its hands; a reason is sent to
// it as a notice.
func (r *Roster) leave(agent, reason string) {
	r.mu.Lock()
	joined := r.members[agent]
	if joined == nil {
		r.mu.Unlock()
		return
	}
	delete(r.members, agent)
	r.keytermsLocked()
	r.mu.Unlock()
	if r.cfg.Floor != nil {
		r.cfg.Floor.LowerHands(agent)
	}
	r.notify("agent_left", joined.SessionID)
	r.logf("joined: an agent left meeting %s", joined.SessionID)
	if reason != "" {
		r.async(func(ctx context.Context) { r.send(ctx, agent, "", "notice", reason) })
	}
}

// SessionEnded makes the agents of recording session id leave, as its meeting
// ended, and tells them.
func (r *Roster) SessionEnded(id string) { r.leaveMeetings([]string{id}, endedText) }

// SessionsDeleted makes the agents of the deleted sessions leave, and tells
// them.
func (r *Roster) SessionsDeleted(ids []string) { r.leaveMeetings(ids, deletedText) }

func (r *Roster) leaveMeetings(ids []string, reason string) {
	r.mu.Lock()
	var gone []string
	for agent, m := range r.members {
		if slices.Contains(ids, m.SessionID) {
			gone = append(gone, agent)
		}
	}
	r.mu.Unlock()
	for _, agent := range gone {
		r.leave(agent, reason)
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
	ids := make([]string, 0, len(r.members))
	for agent := range r.members {
		ids = append(ids, agent)
	}
	r.mu.Unlock()
	for _, agent := range ids {
		info, err := agents.Lookup(ctx, agent)
		if err != nil {
			r.logf("joined: read whether a joined agent is live: %v", err)
			continue
		}
		now := r.cfg.Now()
		r.mu.Lock()
		joined := r.members[agent]
		gone := false
		switch {
		case joined == nil:
		case info.State == envoy.StateLive:
			joined.notLiveSince = time.Time{}
		case joined.notLiveSince.IsZero():
			joined.notLiveSince = now
		default:
			gone = now.Sub(joined.notLiveSince) >= r.cfg.NotLiveFor
		}
		r.mu.Unlock()
		if gone {
			r.leave(agent, notLiveText)
		}
	}
}

// OnSegment reads a final segment of the room's words: each agent of its
// meeting whose issue it names gets a pointer, at most one in PointerEvery;
// a mention inside that time goes in the next pointer. A call to Wispr sends
// no pointer: it reaches an agent through ask_agent alone.
func (r *Roster) OnSegment(s voice.Segment) {
	if s.Call || s.SessionID == "" {
		return
	}
	now := r.cfg.Now()
	var sends []*pointer
	r.mu.Lock()
	for _, m := range r.members {
		if m.SessionID != s.SessionID || !m.comesUp(s.Text) {
			continue
		}
		if m.waiting || (!m.lastPointer.IsZero() && now.Sub(m.lastPointer) < r.cfg.PointerEvery) {
			if !m.waiting {
				m.waiting, m.waitFrom, m.waitTo = true, s.From, s.To
			}
			m.waitFrom, m.waitTo = minTime(m.waitFrom, s.From), maxTime(m.waitTo, s.To)
			continue
		}
		m.lastPointer = now
		sends = append(sends, &pointer{agent: m.Agent, key: m.IssueKey, sessionID: m.SessionID, from: s.From, to: s.To})
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
		if m.waiting && now.Sub(m.lastPointer) >= r.cfg.PointerEvery {
			m.waiting, m.lastPointer = false, now
			sends = append(sends, &pointer{agent: m.Agent, key: m.IssueKey, sessionID: m.SessionID, from: m.waitFrom, to: m.waitTo})
		}
	}
	r.mu.Unlock()
	r.sendPointers(sends)
}

func (r *Roster) sendPointers(sends []*pointer) {
	for _, p := range sends {
		p.started = r.started(p.sessionID)
		text := pointerText(p, r.apiURL())
		r.async(func(ctx context.Context) { r.send(ctx, p.agent, "", "pointer", text) })
	}
}

// HandDone tells an agent how its floor request came down: spoken, or
// dropped as nobody invited it. A hand withdrawn as its agent left or its
// meeting ended says nothing more.
func (r *Roster) HandDone(h voice.Hand, outcome voice.HandOutcome) {
	var text string
	switch outcome {
	case voice.HandSpoken:
		text = spokenText
	case voice.HandExpired:
		text = fmt.Sprintf("Nobody invited your words within %s, so they were dropped. You may ask for the floor again once %s have passed since you asked.", every(r.cfg.FloorExpiry), every(r.cfg.FloorEvery))
	default:
		return
	}
	r.async(func(ctx context.Context) { r.send(ctx, h.Agent, h.ID, "hand "+outcome.String(), text) })
}

// reply answers message m on its thread, as a notice that asks no reply. Its
// idempotency key is m's and the kind's, so a redelivered message is
// answered once.
func (r *Roster) reply(ctx context.Context, m *envoy.Message, kind, text string) {
	r.send(ctx, m.SourceSession, m.EventID, kind, text)
}

// send sends agent text as a notice, in reply to inReplyTo when set.
func (r *Roster) send(ctx context.Context, agent, inReplyTo, kind, text string) {
	agents := r.currentAgents()
	if agents == nil {
		r.logf("joined: no Envoy session to send a joined agent its %s", kind)
		return
	}
	key := uuid.NewString()
	if inReplyTo != "" {
		key = uuid.NewSHA1(uuid.NameSpaceOID, []byte("ghost-wispr joined "+inReplyTo+" "+kind)).String()
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if _, err := agents.Send(ctx, &envoy.SendRequest{Target: agent, Message: text, NoReply: true, IdempotencyKey: key, InReplyTo: inReplyTo}); err != nil {
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

// keytermsLocked gives Keyterms the issue keys of the agents of the meeting
// now recording, in the order they joined. r.mu is held: the keyterms
// follow the joins in their order.
func (r *Roster) keytermsLocked() {
	if r.cfg.Keyterms == nil {
		return
	}
	session := r.cfg.Session()
	members := make([]*member, 0, len(r.members))
	for _, m := range r.members {
		if m.SessionID == session && m.IssueKey != "" {
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
	r.cfg.Keyterms(keys)
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
