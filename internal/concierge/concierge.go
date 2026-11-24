// Package concierge is Wispr's meeting helper: the agent loop that answers a
// spoken question from Dispatch's record, finds the agent that holds an issue,
// and asks it a read-only question over Envoy. It has its own streaming,
// tool-calling model client (NewGenerator: genai's on Vertex AI or the Gemini
// API, or chat completions on an OpenAI API provider, such as a model
// gateway), separate from internal/llm, which makes summaries.
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
	"sync/atomic"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// DefaultModel is the Concierge's model when concierge.model is unset:
// Gemini 3.5 Flash-Lite, generally available on Vertex (global, us, eu), with
// function calling, built for low-latency agentic work.
const DefaultModel = "gemini-3.5-flash-lite"

// Generator streams a model's answer. NewGenerator's return their provider's
// errors as *providererr.Error, which keeps none of the provider's words.
type Generator interface {
	GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
}

// NewGenerator returns the model client the Concierge streams from, on
// provider p, whose requests carry p's credential: genai's on a Vertex AI or
// Gemini API provider, and chat completions on an OpenAI API provider, such
// as a model gateway (openaiGenerator). It refuses a provider of another API.
func NewGenerator(ctx context.Context, p *providers.Provider) (Generator, error) {
	switch p.API {
	case config.APIVertex, config.APIGemini:
		clientConfig, err := p.GenAIConfig()
		if err != nil {
			return nil, fmt.Errorf("concierge: %w", err)
		}
		client, err := genai.NewClient(ctx, clientConfig)
		if err != nil {
			return nil, fmt.Errorf("concierge: create the Gemini client: %w", err)
		}
		return genaiGenerator{models: client.Models, provider: p.Name, labels: p.GenerateLabels()}, nil
	case config.APIOpenAI:
		return newOpenAIGenerator(p), nil
	}
	return nil, fmt.Errorf("concierge: provider %q speaks the %s API, which the Concierge has no client for (it calls %s)", p.Name, p.API, strings.Join(config.RoleConcierge.APIs, ", "))
}

// genaiGenerator is genai's client, whose errors it reduces as the
// provider's (providererr): genai's own text quotes the provider's message.
// Each call carries the provider's labels, on Vertex AI.
type genaiGenerator struct {
	models   *genai.Models
	provider string            // the provider's name, which its errors name
	labels   map[string]string // the provider's GenerateLabels
}

func (g genaiGenerator) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	if len(g.labels) > 0 {
		// A copy: the Concierge reuses the config it passes.
		var labeled genai.GenerateContentConfig
		if cfg != nil {
			labeled = *cfg
		}
		labeled.Labels = g.labels
		cfg = &labeled
	}
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for response, err := range g.models.GenerateContentStream(ctx, model, contents, cfg) {
			if !yield(response, providererr.From(g.provider, err)) {
				return
			}
		}
	}
}

// Records is Dispatch's record as the tools read it; *dispatch.Client is one.
// Search is Dispatch's normal search, words and meaning; SearchKeywords its
// words leg alone, far faster.
type Records interface {
	Search(ctx context.Context, query string, limit int) ([]dispatch.SearchHit, error)
	SearchKeywords(ctx context.Context, query string, limit int) ([]dispatch.SearchHit, error)
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
	CancelWorkingVoiceTasks(sessionID string) ([]storage.VoiceTask, error)
	CancelWorkingVoiceTasksOfEndedSessions() ([]storage.VoiceTask, error)
	CancelAllWorkingVoiceTasks() ([]storage.VoiceTask, error)
	MaxVoiceTaskNumber() (uint64, error)
}

// JoinedAgent is an agent that joined the meeting.
type JoinedAgent struct {
	SessionID string
	Title     string
	IssueKey  string
}

// Config configures a Concierge.
type Config struct {
	// Model is the model, as Generator's provider names it; empty is
	// DefaultModel, a Gemini model.
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

// questionsAsked counts the questions every Concierge of this process has
// asked, past the highest number the ledger held when each started. Each
// task keeps its number in its row (VoiceTask.AskedNumber), and a never
// mind's mark (QuestionsAsked) orders it among them whatever the wall clock
// does, also against a Concierge started after it.
var questionsAsked atomic.Uint64

// Concierge implements voice.Concierge.
type Concierge struct {
	model     string
	generator Generator
	records   Records
	store     Store
	publicURL string
	joined    func() []JoinedAgent
	changed   func(sessionID string)
	untold    []storage.VoiceTask
	now       func() time.Time
	logger    *slog.Logger
	answers   chan voice.PendingAnswer
	// life ends with Stop (stop): its background work, the rewrite of an
	// answer for the ear, the read of the projects and the sends after a
	// reply, runs under it, and work counts it, as it counts each call from
	// outside that may write the store (enter). done closes when the
	// goroutine that offers answers ends; stopped, once Stop has ended the
	// life and everything it waits for has ended too.
	life     context.Context
	stop     context.CancelFunc
	work     sync.WaitGroup
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	mu     sync.Mutex
	agents Agents
	// lastAsk is the newest question each exchange sent each agent, by
	// exchange (exchangeKey) and agent session, so a second question
	// replaces the first, and the same one again sends nothing (R7).
	lastAsk map[[2]string]sentAsk
	// unsent is, by exchange and question (askBase), a send Envoy did not
	// confirm: its idempotency key and its time, which its retry reuses, so
	// the question Envoy may hold and the task name one time (R7).
	unsent map[[2]string]unsentAsk
	// working lists the unanswered tasks sent to each agent session, so a
	// message from it that names no question can be met with a request to
	// reply again.
	working map[string][]string
	// askedAgain is, per task, the event of the one request its agent got to
	// reply again with in_reply_to; againFor leads that event to the task.
	// againKey is, per task, the idempotency key of a request Envoy did not
	// confirm, which its retry reuses.
	askedAgain map[string]string
	againFor   map[string]string
	againKey   map[string]string
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
	// picks is, per exchange's wake, the two issues an answer offered and
	// the question asked, for the turn that picks one (R5).
	picks map[string]offeredPick
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
	// projectsDown: the last read of the projects failed.
	projectsDown bool
	// fastBound, knownBound, lookupBound, callBound, searchBound,
	// searchSettle and routingAfter are the constants of those names
	// (fastAnswerBound for fastBound, knownAfter for knownBound); a test
	// shortens them.
	fastBound    time.Duration
	knownBound   time.Duration
	lookupBound  time.Duration
	callBound    time.Duration
	searchBound  time.Duration
	searchSettle time.Duration
	fullWait     time.Duration
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
		done:         make(chan struct{}),
		stopped:      make(chan struct{}),
		lastAsk:      map[[2]string]sentAsk{},
		unsent:       map[[2]string]unsentAsk{},
		working:      map[string][]string{},
		askedAgain:   map[string]string{},
		againFor:     map[string]string{},
		againKey:     map[string]string{},
		sentAs:       map[string]string{},
		inflight:     map[string]int{},
		named:        map[string]*named{},
		queued:       make(chan struct{}, 1),
		prefetches:   map[string]*prefetch{},
		picks:        map[string]offeredPick{},
		sessionOf:    map[string]string{},
		closed:       map[string]closure{},
		fastBound:    fastAnswerBound,
		knownBound:   knownAfter,
		lookupBound:  lookupBound,
		callBound:    callBound,
		searchBound:  searchBound,
		searchSettle: searchSettle,
		fullWait:     fullWaitAfterTurn,
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
	c.life, c.stop = context.WithCancel(context.Background())
	// Numbers go on from the highest the ledger holds, so a question asked
	// now is numbered after every one asked before, in this run or another.
	highest, err := c.store.MaxVoiceTaskNumber()
	if err != nil {
		c.stop()
		return nil, fmt.Errorf("concierge: %w", err)
	}
	for {
		asked := questionsAsked.Load()
		if asked >= highest || questionsAsked.CompareAndSwap(asked, highest) {
			break
		}
	}
	// A meeting that ended while no Concierge ran (the voice was off) was
	// told nothing: its working questions are cancelled now, as its end
	// would have, and its page told.
	if ended, err := c.store.CancelWorkingVoiceTasksOfEndedSessions(); err != nil {
		c.logger.Warn("concierge could not cancel the questions of meetings that ended while it was off", "error", err)
	} else {
		// Their holders are told once the Envoy session opens (SetAgents).
		c.untold = ended
		for _, id := range sessionsOf(ended) {
			c.tellChanged(id)
		}
	}
	go func() {
		defer close(c.done)
		c.offerAnswers()
	}()
	if c.records != nil {
		// The project list is read at once, so the first question's keys
		// are known (viewProjects).
		c.projectsBusy = true
		c.background(c.readProjects)
	}
	return c, nil
}

// background runs work under the Concierge's life, which Stop ends and
// waits for. After Stop it runs nothing.
func (c *Concierge) background(work func(ctx context.Context)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.life.Err() != nil {
		return
	}
	c.work.Add(1)
	go func() {
		defer c.work.Done()
		work(c.life)
	}()
}

// errStopped is a call's error once Stop has run: the Concierge takes no
// more.
var errStopped = errors.New("concierge: stopped")

// enter admits one call that may write the store (an answer, a tool call, an
// ask an answer leaves running, a spoken mark, a never mind, a delivered
// reply) as work Stop waits for, so the store closes under none of them. Its
// context ends when parent does or when Stop runs; leave ends it and the
// count. After Stop it admits nothing, and ok is false. c.mu must not be held.
func (c *Concierge) enter(parent context.Context) (ctx context.Context, leave func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.life.Err() != nil {
		return nil, nil, false
	}
	c.work.Add(1)
	ctx, cancel := context.WithCancelCause(parent)
	stopped := context.AfterFunc(c.life, func() { cancel(errStopped) })
	return ctx, func() {
		stopped()
		cancel(nil)
		c.work.Done()
	}, true
}

// Stop ends the Concierge, when the unit stops (VoiceOff, when the voice is
// turned off): the goroutine that offers answers, a rewrite of an answer for
// the ear, every call that may write the store (enter), which it cancels,
// and any other background work end, and Stop waits for them (StopWithin,
// with no deadline). A send to an agent already begun keeps its own bound
// (sendTimeout). An answer that lands after it is kept in the ledger, for
// its session's page, and not announced. A second Stop does nothing.
func (c *Concierge) Stop() { _ = c.StopWithin(context.Background()) }

// StopWithin ends the Concierge as Stop does, and waits for its work within
// ctx. At ctx's end it returns ctx's cause and waits no longer: the work it
// cancelled ends behind it, and a store write that work still makes after
// the unit closed its store is refused there (storage).
func (c *Concierge) StopWithin(ctx context.Context) error {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stop()
		c.mu.Unlock()
		go func() {
			<-c.done
			c.work.Wait()
			close(c.stopped)
		}()
	})
	select {
	case <-c.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("concierge: its work still runs at the stop's deadline: %w", context.Cause(ctx))
	}
}

// VoiceOff ends the Concierge as Stop does, as the voice is turned off, then
// cancels every question still working, of a meeting still recording too,
// tells each page, and tells each holder not to answer: no one hears an
// answer while the voice is off, and a working question's late reply would
// be offered once it is on again. As after a meeting's end, a late reply
// still lands in the ledger, for the page. Call it before the Envoy session
// closes. Stop alone, at shutdown, keeps them: the unit's Envoy session is
// the same after a restart, so a reply can still land.
func (c *Concierge) VoiceOff() {
	c.Stop()
	cancelled, err := c.store.CancelAllWorkingVoiceTasks()
	if err != nil {
		c.logger.Warn("concierge could not cancel its questions as the voice went off", "error", err)
		return
	}
	for _, id := range sessionsOf(cancelled) {
		c.logger.Info("concierge cancelled the questions of a session as the voice went off", "session", id)
		c.tellChanged(id)
	}
	c.tellHoldersEnded(cancelled, "the voice was turned off")
}

// sessionsOf is the meetings of tasks, each once, in order.
func sessionsOf(tasks []storage.VoiceTask) []string {
	var ids []string
	for i := range tasks {
		if !slices.Contains(ids, tasks[i].SessionID) {
			ids = append(ids, tasks[i].SessionID)
		}
	}
	return ids
}

// SetAgents gives the Concierge the unit's Envoy session once it is open.
// The session's Deliver is the Concierge's, so the Concierge exists first.
// The holders of the questions cancelled at the start are told then.
func (c *Concierge) SetAgents(agents Agents) {
	c.mu.Lock()
	c.agents = agents
	var untold []storage.VoiceTask
	if agents != nil {
		untold, c.untold = c.untold, nil
	}
	c.mu.Unlock()
	if len(untold) > 0 {
		c.background(func(context.Context) { c.tellHoldersEnded(untold, "the meeting ended") })
	}
}

// tellHoldersEnded tells the agent of each cancelled task not to answer
// (tellHolderEnded), all at once, and waits for the sends. It forgets no
// reply route: a late reply still lands in the ledger, for the page.
func (c *Concierge) tellHoldersEnded(tasks []storage.VoiceTask, why string) {
	var wg sync.WaitGroup
	for i := range tasks {
		agent, ok := strings.CutPrefix(tasks[i].Holder, "session:")
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.tellHolderEnded(context.Background(), &tasks[i], agent, storage.VoiceTaskCancelled, why)
		}()
	}
	wg.Wait()
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
// SessionDeleted withdraws. It ends with the Concierge's life (Stop).
func (c *Concierge) offerAnswers() {
	for {
		c.mu.Lock()
		for len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-c.queued:
			case <-c.life.Done():
				return
			}
			c.mu.Lock()
		}
		next := c.queue[0]
		c.queue = c.queue[1:]
		drop, done := make(chan struct{}), make(chan struct{})
		c.offering, c.offerDrop, c.offerDone = &next, drop, done
		c.mu.Unlock()
	offer:
		for c.life.Err() == nil && c.stillOwed(&next) {
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
			case <-c.life.Done():
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
	for key := range c.unsent {
		if strings.HasPrefix(key[0], prefix) {
			delete(c.unsent, key)
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
	for key, p := range c.picks {
		if p.sessionID == sessionID {
			delete(c.picks, key)
		}
	}
	// What exchanges heard before any session existed read and were offered
	// holds room words: it goes too. The agents named there are only
	// session ids, which a running exchange may still need: they go with the
	// gate, or after their hour.
	c.forgetEarlyWordsLocked()
	of := func(id string) bool { return c.sessionOf[id] == sessionID }
	for agent, ids := range c.working {
		if ids = slices.DeleteFunc(ids, of); len(ids) == 0 {
			delete(c.working, agent)
		} else {
			c.working[agent] = ids
		}
	}
	// An ended meeting's tasks get no request to reply again.
	for taskID := range c.againKey {
		if of(taskID) {
			delete(c.againKey, taskID)
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

// projectsView is the cached list of Dispatch's projects as one turn reads
// it, once, where the turn enters the Concierge (Heard, answer). Every
// decision of the turn, its keys, its target and an outage, comes from this
// one reading, so a read of the list that lands mid-turn changes none of
// them; the next turn reads the new list.
type projectsView struct {
	known map[string]bool
	// sure: the list can tell a project from none. It cannot when it was
	// never read or its last read failed, Dispatch not answering.
	sure bool
}

// viewProjects reads the cached list once, for one turn. Until the projects
// are known none is: a "word NN" said in the room is no key, so it never
// reaches Dispatch in an address. After a failed read the list last read
// still answers. It never waits: a stale or missing list is read in the
// background, for a later turn.
func (c *Concierge) viewProjects() projectsView {
	c.mu.Lock()
	refresh := c.now().Sub(c.projectsRead) > projectsFor && !c.projectsBusy && c.records != nil
	if refresh {
		c.projectsBusy = true
	}
	// readProjects replaces the map, never changes it, so the view may keep it.
	view := projectsView{known: c.projects, sure: c.projects != nil && !c.projectsDown}
	c.mu.Unlock()
	if refresh {
		c.background(c.readProjects)
	}
	return view
}

// holds reports whether project is one of the view's projects.
func (v projectsView) holds(project string) bool {
	return v.known[strings.ToUpper(project)]
}

// keyUntold reports whether words name an issue key, other than one of
// except, whose project the view neither holds nor can tell it does not:
// the check failed because Dispatch did not answer, so the turn is an outage
// of Dispatch's, not a key Dispatch has no project for.
func (v projectsView) keyUntold(words string, except []string) bool {
	for _, key := range issueKeysIn(words) {
		project, _, _ := strings.Cut(key, "-")
		if !v.holds(project) && !v.sure && !slices.Contains(except, key) {
			return true
		}
	}
	return false
}

// readProjects reads Dispatch's project keys, within a read's bound, and
// keeps them. A failed read keeps what was known, marks the list unable to
// tell (projectsDown), and is tried again after a minute.
func (c *Concierge) readProjects(ctx context.Context) {
	var keys []string
	err := boundCall(ctx, c.callBound, dispatch.ErrUnreachable, func(ctx context.Context) error {
		var err error
		keys, err = c.records.Projects(ctx)
		return err
	})
	if err != nil { // logged without c.mu: a log write can wait
		c.logger.Warn("concierge could not read Dispatch's projects", "error", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.projectsBusy = false
	// A stop is no failure of Dispatch's.
	c.projectsDown = err != nil && ctx.Err() == nil
	if err != nil {
		if ctx.Err() == nil {
			// A stop is no failure: the next read may come at once.
			c.projectsRead = c.now().Add(time.Minute - projectsFor)
		}
		return
	}
	known := make(map[string]bool, len(keys))
	for _, key := range keys {
		known[strings.ToUpper(key)] = true
	}
	c.projects, c.projectsRead = known, c.now()
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
	if len(cancelled) > 0 {
		c.logger.Info("concierge cancelled the questions of an ended session", "session", sessionID, "cancelled", len(cancelled))
		c.tellChanged(sessionID)
		c.background(func(context.Context) { c.tellHoldersEnded(cancelled, "the meeting ended") })
	}
	return nil
}

// GateClosed drops what the Concierge holds of exchanges heard before their
// recording session existed: their reads, the issues offered them, the
// agents named in them. The Supervisor calls it when the unit stops
// listening; a session's end and its delete drop the reads and offers too.
func (c *Concierge) GateClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetEarlyWordsLocked()
	for key := range c.named {
		if strings.HasPrefix(key, "|") {
			delete(c.named, key)
		}
	}
}

// forgetEarlyWordsLocked drops the reads and offers held under no session
// id, which hold room words. c.mu is held.
func (c *Concierge) forgetEarlyWordsLocked() {
	for key, p := range c.prefetches {
		if strings.HasPrefix(key, "|") {
			p.cancelAll()
			delete(c.prefetches, key)
		}
	}
	for key, p := range c.picks {
		if p.sessionID == "" {
			delete(c.picks, key)
		}
	}
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

// Spoken records that a pending answer was spoken, as work Stop waits for;
// after Stop it records nothing.
func (c *Concierge) Spoken(ctx context.Context, taskID string) error {
	_, leave, ok := c.enter(ctx)
	if !ok {
		return errStopped
	}
	defer leave()
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
