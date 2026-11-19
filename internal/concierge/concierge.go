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
	// reply that names no message it answers still finds its task.
	working map[string][]string
	// sentAs leads a sent question's Envoy event to its task's local id
	// until the task takes the event as its id.
	sentAs map[string]string
	// inflight counts the questions on their way to each agent session.
	inflight map[string]int
	// named is, per exchange, the agent sessions ask_agent may ask.
	named map[string]*named
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
		model:     cfg.Model,
		generator: cfg.Generator,
		records:   cfg.Records,
		store:     cfg.Store,
		publicURL: cfg.PublicURL,
		joined:    cfg.Joined,
		now:       cfg.Now,
		logger:    cfg.Logger,
		answers:   make(chan voice.PendingAnswer, 16),
		lastAsk:   map[[2]string]string{},
		working:   map[string][]string{},
		sentAs:    map[string]string{},
		inflight:  map[string]int{},
		named:     map[string]*named{},
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
func (c *Concierge) Answers() <-chan voice.PendingAnswer { return c.answers }

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
