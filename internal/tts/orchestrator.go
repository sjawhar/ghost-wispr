package tts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
)

// Default orchestrator settings.
const (
	DefaultQueueDepth        = 5
	DefaultMaxRequestsPerMin = 10
	DefaultSynthesisTimeout  = 30 * time.Second
)

// Status values for a speak request.
const (
	StatusQueued    = "queued"
	StatusPlaying   = "playing"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// SpeakStatus represents the current state of a TTS speak request.
type SpeakStatus struct {
	mu           sync.RWMutex `json:"-"`
	ID           string       `json:"id"`
	Status       string       `json:"status"`
	BytesWritten int64        `json:"bytes_written"`
	DurationMs   int64        `json:"duration_ms"`
	Error        string       `json:"error,omitempty"`
}

// Snapshot returns a copy safe for concurrent read.
func (s *SpeakStatus) Snapshot() SpeakStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SpeakStatus{ID: s.ID, Status: s.Status, BytesWritten: s.BytesWritten, DurationMs: s.DurationMs, Error: s.Error}
}

// OrchestratorOpts configures the TTS orchestrator.
type OrchestratorOpts struct {
	QueueDepth        int
	MaxRequestsPerMin int
	SynthesisTimeout  time.Duration
}

func (o *OrchestratorOpts) withDefaults() OrchestratorOpts {
	out := *o
	if out.QueueDepth <= 0 {
		out.QueueDepth = DefaultQueueDepth
	}
	if out.MaxRequestsPerMin <= 0 {
		out.MaxRequestsPerMin = DefaultMaxRequestsPerMin
	}
	if out.SynthesisTimeout <= 0 {
		out.SynthesisTimeout = DefaultSynthesisTimeout
	}
	return out
}

type speakRequest struct {
	id       string
	text     string
	voice    string
	provider string
}

// Orchestrator manages an async TTS pipeline: enqueue text → synthesize → play.
type Orchestrator struct {
	ttsProvider Provider
	speaker     *audio.Speaker
	opts        OrchestratorOpts

	queue    chan *speakRequest
	statuses sync.Map // map[string]*SpeakStatus

	// Rate limiting: sliding window of request timestamps.
	rateMu    sync.Mutex
	rateSlots []time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// NewOrchestrator creates an Orchestrator and starts its worker goroutine.
// The worker stops when the returned cancel function is called or when
// the parent context is cancelled.
func NewOrchestrator(provider Provider, speaker *audio.Speaker, opts OrchestratorOpts) *Orchestrator {
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())

	o := &Orchestrator{
		ttsProvider: provider,
		speaker:     speaker,
		opts:        opts,
		queue:       make(chan *speakRequest, opts.QueueDepth),
		rateSlots:   make([]time.Time, 0, opts.MaxRequestsPerMin),
		ctx:         ctx,
		cancel:      cancel,
	}

	go o.worker()
	return o
}

// Stop signals the worker to drain and exit.
func (o *Orchestrator) Stop() {
	o.cancel()
}

// Speak enqueues a text-to-speech request and returns a request ID.
// It returns an error if the queue is full or rate limit is exceeded.
func (o *Orchestrator) Speak(text, voice, provider string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("text must not be empty")
	}

	// Rate limit check.
	if !o.allowRequest() {
		return "", ErrRateLimited
	}

	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}

	req := &speakRequest{
		id:       id,
		text:     text,
		voice:    voice,
		provider: provider,
	}

	status := &SpeakStatus{ID: id, Status: StatusQueued}
	o.statuses.Store(id, status)

	select {
	case o.queue <- req:
		return id, nil
	default:
		o.statuses.Delete(id)
		return "", ErrQueueFull
	}
}

// Status returns the current status of a speak request.
func (o *Orchestrator) Status(id string) (SpeakStatus, bool) {
	val, ok := o.statuses.Load(id)
	if !ok {
		return SpeakStatus{}, false
	}
	return val.(*SpeakStatus).Snapshot(), true
}

// HasProvider reports whether a TTS provider is configured.
func (o *Orchestrator) HasProvider() bool {
	return o.ttsProvider != nil
}

// HasSpeaker reports whether a speaker is configured.
func (o *Orchestrator) HasSpeaker() bool {
	return o.speaker != nil
}

// worker processes speak requests from the queue sequentially.
func (o *Orchestrator) worker() {
	for {
		select {
		case <-o.ctx.Done():
			return
		case req := <-o.queue:
			o.processRequest(req)
		}
	}
}

// processRequest handles a single TTS request with panic recovery.
func (o *Orchestrator) processRequest(req *speakRequest) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("tts orchestrator panic recovered", "request_id", req.id, "panic", r)
			o.updateStatus(req.id, StatusFailed, 0, 0, fmt.Sprintf("internal error: %v", r))
		}
	}()

	o.updateStatus(req.id, StatusPlaying, 0, 0, "")

	// Synthesize with timeout.
	synthCtx, synthCancel := context.WithTimeout(o.ctx, o.opts.SynthesisTimeout)
	defer synthCancel()

	speechReq := SpeechRequest{
		Text:  req.text,
		Voice: req.voice,
	}

	resp, err := o.ttsProvider.Synthesize(synthCtx, speechReq)
	if err != nil {
		o.updateStatus(req.id, StatusFailed, 0, 0, fmt.Sprintf("synthesis failed: %v", err))
		return
	}

	// Play through speaker.
	result, err := o.speaker.Play(resp.Audio, resp.Format)
	if err != nil {
		o.updateStatus(req.id, StatusFailed, 0, 0, fmt.Sprintf("playback failed: %v", err))
		return
	}

	var bytesWritten, durationMs int64
	if result != nil {
		bytesWritten = result.BytesWritten
		durationMs = result.DurationMs
	}

	o.updateStatus(req.id, StatusCompleted, bytesWritten, durationMs, "")
}

func (o *Orchestrator) updateStatus(id, status string, bytesWritten, durationMs int64, errMsg string) {
	val, ok := o.statuses.Load(id)
	if !ok {
		return
	}
	s := val.(*SpeakStatus)
	s.mu.Lock()
	s.Status = status
	s.BytesWritten = bytesWritten
	s.DurationMs = durationMs
	s.Error = errMsg
	s.mu.Unlock()

}

// allowRequest checks and records a request against the per-minute rate limit.
func (o *Orchestrator) allowRequest() bool {
	o.rateMu.Lock()
	defer o.rateMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)

	// Prune expired slots.
	valid := o.rateSlots[:0]
	for _, t := range o.rateSlots {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	o.rateSlots = valid

	if len(o.rateSlots) >= o.opts.MaxRequestsPerMin {
		return false
	}

	o.rateSlots = append(o.rateSlots, now)
	return true
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Sentinel errors for HTTP status mapping.
var (
	ErrQueueFull   = fmt.Errorf("tts queue is full")
	ErrRateLimited = fmt.Errorf("tts rate limit exceeded")
)
