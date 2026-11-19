// Package concierge is Wispr's meeting helper: the agent loop that answers a
// spoken question from Dispatch's record, finds the agent that holds an issue,
// and asks it a read-only question over Envoy. It has its own streaming,
// tool-calling model client (Gemini through go-genai, on Vertex with the
// unit's own Google identity), separate from internal/llm, which makes
// summaries.
//
// Voice A leaves the turn to it (Answer). Voices B and C drive their own turn
// and call its tools (Tools, ToolSpecs). Every question an agent is asked is
// a row of the voice_tasks ledger, which holds no words of the room; the
// agent's written answer lands there and is announced on Answers, rewritten
// for the ear.
package concierge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// DefaultModel is the Concierge's model when concierge.model is unset:
// Gemini 3.5 Flash-Lite, generally available on Vertex (global, us, eu), with
// function calling, built for low-latency agentic work.
const DefaultModel = "gemini-3.5-flash-lite"

// Generator streams a model's answer; *genai.Models is one.
type Generator interface {
	GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
}

// NewGenerator returns the Gemini client the Concierge streams from, built
// from the unit's genai options, as the summarizer's is.
func NewGenerator(ctx context.Context, options *genaiconfig.Options) (Generator, error) {
	config, err := genaiconfig.BuildClientConfig(options)
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("concierge: create the Gemini client: %w", err)
	}
	return client.Models, nil
}

// Records is Dispatch's record as the tools read it; *dispatch.Client is one.
type Records interface {
	Search(ctx context.Context, query string, limit int) ([]dispatch.SearchHit, error)
	Issue(ctx context.Context, key string) (*dispatch.Issue, error)
	Events(ctx context.Context, key string, limit int) ([]dispatch.Event, error)
	DocumentText(ctx context.Context, key, slug string) (string, error)
	Projects(ctx context.Context) ([]string, error)
}

// Agents is the unit's Envoy session as the tools use it; *envoy.Session is
// one.
type Agents interface {
	ID() string
	Lookup(ctx context.Context, id string) (envoy.SessionInfo, error)
	Send(ctx context.Context, request *envoy.SendRequest) (envoy.SendResult, error)
}

// Store is the ledger and the sessions; *storage.SQLiteStore is one.
type Store interface {
	GetSession(id string) (storage.Session, error)
	CreateVoiceTask(task *storage.VoiceTask) error
	RekeyVoiceTask(id, newID string) error
	DeleteVoiceTask(id string) error
	VoiceTask(id string) (storage.VoiceTask, error)
	VoiceTasks(sessionID string) ([]storage.VoiceTask, error)
	AnswerVoiceTask(id, written string) (bool, error)
	EndVoiceTask(id string, state storage.VoiceTaskState) (bool, error)
	MarkVoiceTaskSpoken(id string, at time.Time) error
	CancelWorkingVoiceTasks(sessionID string) (int64, error)
}

// JoinedAgent is an agent that joined the meeting.
type JoinedAgent struct {
	SessionID string
	Title     string
	IssueKey  string
}

// Config configures a Concierge.
type Config struct {
	// Model is the Gemini model; empty is DefaultModel.
	Model     string
	Generator Generator
	// Records is nil while the unit has no Dispatch token or no reach: the
	// tools then answer "I cannot reach Dispatch yet".
	Records Records
	Store   Store
	// PublicURL is the unit's own HTTPS address, so a question names where
	// its meeting's transcript is read.
	PublicURL string
	// Joined lists the agents that joined the meeting; nil is none.
	Joined func() []JoinedAgent
	// Changed is told the meeting whose questions to agents changed, as the
	// session page lists them, after each such write; nil tells nobody. It
	// is called without the Concierge's lock held.
	Changed func(sessionID string)
	Now     func() time.Time
	Logger  *slog.Logger
}

// Concierge implements voice.Concierge.
type Concierge struct {
	model     string
	generator Generator
	records   Records
	store     Store
	publicURL string
	joined    func() []JoinedAgent
	changed   func(sessionID string)
	now       func() time.Time
	logger    *slog.Logger
	answers   chan voice.PendingAnswer

	mu     sync.Mutex
	agents Agents
	// lastAsk is the newest task each exchange sent each agent, by exchange
	// (exchangeKey) and agent session, so a second question replaces the
	// first (R7).
	lastAsk map[[2]string]string
	// working lists the unanswered tasks sent to each agent session, so a
	// message from it that names no question can be met with a request to
	// reply again.
	working map[string][]string
	// askedAgain is, per task, the event of the one request its agent got to
	// reply again with in_reply_to; againFor leads that event to the task.
	askedAgain map[string]string
	againFor   map[string]string
	// sentAs leads a sent question's Envoy event to its task's local id
	// until the task takes the event as its id.
	sentAs map[string]string
	// inflight counts the questions on their way to each agent session.
	inflight map[string]int
	// named is, per exchange, the agent sessions ask_agent may ask.
	named map[string]*named
	// queue holds announcements not yet handed to Answers; offering is the
	// one being handed over now, which closing offerDrop withdraws, and
	// offerDone closes once the pump is done with it. queued wakes the pump.
	queue     []voice.PendingAnswer
	offering  *voice.PendingAnswer
	offerDrop chan struct{}
	offerDone chan struct{}
	queued    chan struct{}
	// prefetches is, per exchange, the record read while its asker speaks.
	prefetches map[string]*prefetch
	// sessionOf names the meeting of each task id the maps above hold, local
	// or event, so a meeting's state goes without a read of the store.
	sessionOf map[string]string
	// closed holds, for namedFor, each meeting that ended or was deleted, so
	// an answer still being rewritten, a read or a send still running for it
	// adds nothing back.
	closed map[string]closure
	// projects is Dispatch's project keys, as last read (projectsRead), so a
	// number said after a word that is no project is no issue key.
	projects     map[string]bool
	projectsRead time.Time
	projectsBusy bool
	// fastBound and routingAfter are fastAnswerBound and routingAfter; a
	// test shortens them.
	fastBound    time.Duration
	routingAfter time.Duration
}

// closure is a meeting's end or delete.
type closure struct {
	at      time.Time
	deleted bool
}

var _ voice.Concierge = (*Concierge)(nil)

// New returns a Concierge. It needs a Generator and a Store.
func New(cfg *Config) (*Concierge, error) {
	if cfg.Generator == nil {
		return nil, errors.New("concierge: no model client")
	}
	if cfg.Store == nil {
		return nil, errors.New("concierge: no store")
	}
	c := &Concierge{
		model:        cfg.Model,
		generator:    cfg.Generator,
		records:      cfg.Records,
		store:        cfg.Store,
		publicURL:    cfg.PublicURL,
		joined:       cfg.Joined,
		changed:      cfg.Changed,
		now:          cfg.Now,
		logger:       cfg.Logger,
		answers:      make(chan voice.PendingAnswer),
		lastAsk:      map[[2]string]string{},
		working:      map[string][]string{},
		askedAgain:   map[string]string{},
		againFor:     map[string]string{},
		sentAs:       map[string]string{},
		inflight:     map[string]int{},
		named:        map[string]*named{},
		queued:       make(chan struct{}, 1),
		prefetches:   map[string]*prefetch{},
		sessionOf:    map[string]string{},
		closed:       map[string]closure{},
		fastBound:    fastAnswerBound,
		routingAfter: routingAfter,
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	go c.offerAnswers()
	if c.records != nil {
		// The project list is read at once, so the first question's keys
		// are known (isProject).
		c.projectsBusy = true
		go c.readProjects(context.Background())
	}
	return c, nil
}

// SetAgents gives the Concierge the unit's Envoy session once it is open.
// The session's Deliver is the Concierge's, so the Concierge exists first.
func (c *Concierge) SetAgents(agents Agents) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agents = agents
}

func (c *Concierge) currentAgents() Agents {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agents
}

// Answers reports each holder's answer once it lands, rewritten for the ear.
// No announcement is dropped, except one whose meeting ended or was deleted:
// they queue until read.
func (c *Concierge) Answers() <-chan voice.PendingAnswer { return c.answers }

// offerAnswers hands queued announcements to Answers as the Supervisor reads
// them. It drops one whose task is gone, which it checks before the hand-over
// and every offerRecheck while it waits, and one SessionEnded or
// SessionDeleted withdraws.
func (c *Concierge) offerAnswers() {
	for {
		c.mu.Lock()
		for len(c.queue) == 0 {
			c.mu.Unlock()
			<-c.queued
			c.mu.Lock()
		}
		next := c.queue[0]
		c.queue = c.queue[1:]
		drop, done := make(chan struct{}), make(chan struct{})
		c.offering, c.offerDrop, c.offerDone = &next, drop, done
		c.mu.Unlock()
	offer:
		for c.stillOwed(&next) {
			select {
			case <-drop:
				break offer
			default:
			}
			select {
			case c.answers <- next:
				break offer
			case <-drop:
				break offer
			case <-time.After(offerRecheck):
			}
		}
		c.mu.Lock()
		c.offering, c.offerDrop, c.offerDone = nil, nil, nil
		close(done)
		c.mu.Unlock()
	}
}

// offerRecheck is how often a waiting announcement checks that its task is
// still there.
const offerRecheck = 100 * time.Millisecond

// stillOwed reports whether an announcement's meeting still records: its
// task is there and its session is active. A read that fails for another
// reason, such as a busy database, keeps it.
func (c *Concierge) stillOwed(answer *voice.PendingAnswer) bool {
	task, err := c.store.VoiceTask(answer.TaskID)
	var session storage.Session
	if err == nil {
		session, err = c.store.GetSession(task.SessionID)
	}
	switch {
	case err == nil && session.Status == storage.SessionActive:
		return true
	case err == nil:
		c.logger.Info("concierge dropped the announcement of an answer whose meeting ended", "task", answer.TaskID)
		return false
	case errors.Is(err, sql.ErrNoRows):
		c.logger.Info("concierge dropped the announcement of an answer whose task is gone", "task", answer.TaskID)
		return false
	default:
		c.logger.Warn("concierge could not check an announcement's task; it keeps it", "task", answer.TaskID, "error", err)
		return true
	}
}

// enqueue queues an announcement for offerAnswers, unless its meeting ended
// or was deleted.
func (c *Concierge) enqueue(answer *voice.PendingAnswer) {
	c.mu.Lock()
	if closed, _ := c.closedLocked(answer.SessionID); closed {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, *answer)
	c.mu.Unlock()
	select {
	case c.queued <- struct{}{}:
	default:
	}
}

// withdrawLocked drops the announcements of meeting sessionID: the queued
// ones, and the one being offered, whose hand-over the returned channel
// closes on (nil when none of the meeting's is offered). A second withdraw
// of the same meeting, an end and a delete together, waits on the same
// hand-over. c.mu is held.
func (c *Concierge) withdrawLocked(sessionID string) chan struct{} {
	c.queue = slices.DeleteFunc(c.queue, func(answer voice.PendingAnswer) bool { return answer.SessionID == sessionID })
	if c.offering == nil || c.offering.SessionID != sessionID {
		return nil
	}
	if c.offerDrop != nil {
		close(c.offerDrop)
		c.offerDrop = nil
	}
	return c.offerDone
}

// forgetLocked drops what the Concierge holds of meeting sessionID: the
// agents it named and asked, what it read for it, and its unanswered tasks.
// With deleted, also the leads from its questions' events and its requests
// to reply again to their tasks; after an end they stay, so a late reply
// still lands in the ledger. c.mu is held.
func (c *Concierge) forgetLocked(sessionID string, deleted bool) {
	prefix := sessionID + "|"
	for key := range c.lastAsk {
		if strings.HasPrefix(key[0], prefix) {
			delete(c.lastAsk, key)
		}
	}
	for key := range c.named {
		if strings.HasPrefix(key, prefix) {
			delete(c.named, key)
		}
	}
	for key, p := range c.prefetches {
		if strings.HasPrefix(key, prefix) {
			p.cancelAll()
			delete(c.prefetches, key)
		}
	}
	of := func(id string) bool { return c.sessionOf[id] == sessionID }
	for agent, ids := range c.working {
		if ids = slices.DeleteFunc(ids, of); len(ids) == 0 {
			delete(c.working, agent)
		} else {
			c.working[agent] = ids
		}
	}
	if !deleted {
		for id, session := range c.sessionOf {
			_, lead := c.sentAs[id]
			_, asked := c.askedAgain[id]
			if session == sessionID && !lead && !asked {
				delete(c.sessionOf, id)
			}
		}
		return
	}
	for taskID, request := range c.askedAgain {
		if of(taskID) {
			delete(c.againFor, request)
			delete(c.askedAgain, taskID)
		}
	}
	for event := range c.sentAs {
		if of(event) {
			delete(c.sentAs, event)
		}
	}
	for id, session := range c.sessionOf {
		if session == sessionID {
			delete(c.sessionOf, id)
		}
	}
}

// closedLocked reports whether meeting sessionID ended or was deleted, and
// whether it was deleted; c.mu is held.
func (c *Concierge) closedLocked(sessionID string) (closed, deleted bool) {
	now := c.now()
	for id, closure := range c.closed {
		if now.Sub(closure.at) > namedFor {
			delete(c.closed, id)
		}
	}
	closure, closed := c.closed[sessionID]
	return closed, closure.deleted
}

// closeLocked marks meeting sessionID ended, or deleted; c.mu is held.
func (c *Concierge) closeLocked(sessionID string, deleted bool) {
	_, wasDeleted := c.closedLocked(sessionID)
	c.closed[sessionID] = closure{at: c.now(), deleted: deleted || wasDeleted}
}

// projectsFor is how long a read of Dispatch's project keys is kept.
const projectsFor = 10 * time.Minute

// isProject reports whether key names one of Dispatch's projects. Until the
// projects are known, or while their read fails, none does: a "word NN" said
// in the room is no key, so it never reaches Dispatch in an address. With
// wait, a stale or missing list is read first; without, it is read in the
// background and this call answers from what is known.
func (c *Concierge) isProject(ctx context.Context, project string, wait bool) bool {
	c.mu.Lock()
	stale := c.now().Sub(c.projectsRead) > projectsFor
	refresh := stale && !c.projectsBusy && c.records != nil
	if refresh {
		c.projectsBusy = true
	}
	known := c.projects
	c.mu.Unlock()
	if refresh {
		if wait {
			known = c.readProjects(ctx)
		} else {
			go c.readProjects(context.WithoutCancel(ctx))
		}
	}
	return known[strings.ToUpper(project)]
}

// readProjects reads Dispatch's project keys and keeps them. A failed read
// keeps what was known and is tried again after a minute.
func (c *Concierge) readProjects(ctx context.Context) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, prefetchTimeout)
	defer cancel()
	keys, err := c.records.Projects(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.projectsBusy = false
	if err != nil {
		c.logger.Warn("concierge could not read Dispatch's projects", "error", err)
		c.projectsRead = c.now().Add(time.Minute - projectsFor)
		return c.projects
	}
	known := make(map[string]bool, len(keys))
	for _, key := range keys {
		known[strings.ToUpper(key)] = true
	}
	c.projects, c.projectsRead = known, c.now()
	return known
}

// SessionEnded cancels the questions its meeting still waits on and
// withdraws its announcements not yet read, including one still being
// rewritten: the answers are kept for the session's page and not announced.
// A late reply still lands in the ledger.
func (c *Concierge) SessionEnded(sessionID string) error {
	cancelled, err := c.store.CancelWorkingVoiceTasks(sessionID)
	c.mu.Lock()
	c.closeLocked(sessionID, false)
	handed := c.withdrawLocked(sessionID)
	c.forgetLocked(sessionID, false)
	c.mu.Unlock()
	if handed != nil {
		<-handed
	}
	if err != nil {
		return err
	}
	if cancelled > 0 {
		c.logger.Info("concierge cancelled the questions of an ended session", "session", sessionID, "cancelled", cancelled)
		c.tellChanged(sessionID)
	}
	return nil
}

// SessionDeleted drops what the Concierge holds of a deleted meeting: its
// announcements, the agents it named and asked, what it read for it, and its
// tasks. Once it returns, nothing of the meeting is handed to Answers, and a
// read or a send still running for it adds nothing back. Its tasks went with
// its rows, so a late reply finds nothing.
func (c *Concierge) SessionDeleted(sessionID string) {
	c.mu.Lock()
	c.closeLocked(sessionID, true)
	handed := c.withdrawLocked(sessionID)
	c.forgetLocked(sessionID, true)
	c.mu.Unlock()
	if handed != nil {
		<-handed
	}
}

// Spoken records that a pending answer was spoken.
func (c *Concierge) Spoken(_ context.Context, taskID string) error {
	if err := c.store.MarkVoiceTaskSpoken(taskID, c.now()); err != nil {
		return err
	}
	if task, err := c.task(taskID); err == nil {
		c.tellChanged(task.SessionID)
	}
	return nil
}

// tellChanged tells Changed that meeting sessionID's questions changed.
func (c *Concierge) tellChanged(sessionID string) {
	if c.changed != nil && sessionID != "" {
		c.changed(sessionID)
	}
}

// ago says how long before now t was, for the ear.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
