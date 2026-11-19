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
	Now    func() time.Time
	Logger *slog.Logger
}

// Concierge implements voice.Concierge.
type Concierge struct {
	model     string
	generator Generator
	records   Records
	store     Store
	publicURL string
	joined    func() []JoinedAgent
	now       func() time.Time
	logger    *slog.Logger
	answers   chan voice.PendingAnswer

	mu     sync.Mutex
	agents Agents
	// lastAsk is the newest task each meeting sent each agent, by meeting
	// and agent session, so a second question replaces the first.
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
	// queue holds announcements not yet handed to Answers, oldest first;
	// offering is the one being handed over now. queued wakes the pump.
	queue    []voice.PendingAnswer
	offering *voice.PendingAnswer
	queued   chan struct{}
	// prefetches is, per exchange, the record read while its asker speaks.
	prefetches map[string]*prefetch
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
		model:      cfg.Model,
		generator:  cfg.Generator,
		records:    cfg.Records,
		store:      cfg.Store,
		publicURL:  cfg.PublicURL,
		joined:     cfg.Joined,
		now:        cfg.Now,
		logger:     cfg.Logger,
		answers:    make(chan voice.PendingAnswer),
		lastAsk:    map[[2]string]string{},
		working:    map[string][]string{},
		askedAgain: map[string]string{},
		againFor:   map[string]string{},
		sentAs:     map[string]string{},
		inflight:   map[string]int{},
		named:      map[string]*named{},
		queued:     make(chan struct{}, 1),
		prefetches: map[string]*prefetch{},
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
// No announcement is dropped: they queue until read.
func (c *Concierge) Answers() <-chan voice.PendingAnswer { return c.answers }

// offerAnswers hands queued announcements to Answers, oldest first, as the
// Supervisor reads them. Before each hand-over, and again every
// offerRecheck while it waits, it drops an announcement whose task is gone
// (its meeting was deleted) or no longer done (its meeting ended).
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
		c.offering = &next
		c.mu.Unlock()
		for c.stillOwed(&next) {
			select {
			case c.answers <- next:
			case <-time.After(offerRecheck):
				continue
			}
			break
		}
		c.mu.Lock()
		c.offering = nil
		c.mu.Unlock()
	}
}

// offerRecheck is how often a waiting announcement checks that its task is
// still owed.
const offerRecheck = 100 * time.Millisecond

// stillOwed reports whether an announcement's task still exists and is done.
func (c *Concierge) stillOwed(answer *voice.PendingAnswer) bool {
	task, err := c.store.VoiceTask(answer.TaskID)
	if err != nil || task.State != storage.VoiceTaskDone {
		c.logger.Info("concierge dropped the announcement of an answer no longer owed", "task", answer.TaskID, "error", err)
		return false
	}
	return true
}

// enqueue queues an announcement for offerAnswers.
func (c *Concierge) enqueue(answer voice.PendingAnswer) {
	c.mu.Lock()
	c.queue = append(c.queue, answer)
	c.mu.Unlock()
	select {
	case c.queued <- struct{}{}:
	default:
	}
}

// SessionEnded cancels the questions its meeting still waits on: their
// answers are kept for the session's page and not announced.
func (c *Concierge) SessionEnded(sessionID string) error {
	cancelled, err := c.store.CancelWorkingVoiceTasks(sessionID)
	if err != nil {
		return err
	}
	if cancelled > 0 {
		c.logger.Info("concierge cancelled the questions of an ended session", "session", sessionID, "cancelled", cancelled)
	}
	return nil
}

// SessionDeleted drops what the Concierge holds of a deleted session: its
// queued announcements, the agents it named and asked, and what it read for
// it. Its tasks went with its rows, so a late reply finds nothing.
func (c *Concierge) SessionDeleted(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = slices.DeleteFunc(c.queue, func(answer voice.PendingAnswer) bool { return answer.SessionID == sessionID })
	for key := range c.lastAsk {
		if key[0] == sessionID {
			delete(c.lastAsk, key)
		}
	}
	prefix := sessionID + "|"
	for key := range c.named {
		if strings.HasPrefix(key, prefix) {
			delete(c.named, key)
		}
	}
	for key := range c.prefetches {
		if strings.HasPrefix(key, prefix) {
			delete(c.prefetches, key)
		}
	}
	// The tasks went with the session's rows: drop every id that names one.
	gone := func(id string) bool {
		if local, ok := c.sentAs[id]; ok {
			id = local
		}
		_, err := c.store.VoiceTask(id)
		return err != nil
	}
	for agent, ids := range c.working {
		ids = slices.DeleteFunc(ids, gone)
		if len(ids) == 0 {
			delete(c.working, agent)
		} else {
			c.working[agent] = ids
		}
	}
	for request, taskID := range c.againFor {
		if gone(taskID) {
			delete(c.againFor, request)
			delete(c.askedAgain, taskID)
		}
	}
	for event, local := range c.sentAs {
		if _, err := c.store.VoiceTask(local); err != nil {
			delete(c.sentAs, event)
		}
	}
}

// Spoken records that a pending answer was spoken.
func (c *Concierge) Spoken(_ context.Context, taskID string) error {
	return c.store.MarkVoiceTaskSpoken(taskID, c.now())
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
