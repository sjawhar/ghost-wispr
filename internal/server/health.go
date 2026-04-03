package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// HealthChecker defines the interface for checking component health
type HealthChecker interface {
	IsDeepgramConnected() bool
	IsDBHealthy(ctx context.Context) bool
	IsMicOpen() bool
	IsLLMHealthy() string
}

const llmStatusUnchecked = "unchecked"

type LLMHealthTracker struct {
	mu          sync.RWMutex
	status      string
	errorDetail string
}

func NewLLMHealthTracker() *LLMHealthTracker {
	return &LLMHealthTracker{status: llmStatusUnchecked}
}

func (t *LLMHealthTracker) SetOK() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = storage.ComponentStatusOK
	t.errorDetail = ""
}

func (t *LLMHealthTracker) SetError(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = storage.ComponentStatusError
	t.errorDetail = msg
}

func (t *LLMHealthTracker) IsHealthy() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status != storage.ComponentStatusError
}

func (t *LLMHealthTracker) Status() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.status == "" {
		return llmStatusUnchecked
	}
	return t.status
}

// HealthzLiveResponse is the response for the liveness check
type HealthzLiveResponse struct {
	Status string `json:"status"`
}

// HealthzReadyResponse is the response for the readiness check
type HealthzReadyResponse struct {
	Deepgram string `json:"deepgram"`
	DB       string `json:"db"`
	Mic      string `json:"mic"`
	LLM      string `json:"llm"`
}

// handleHealthzLive handles the /healthz/live endpoint
// Returns 200 if the process is alive (always true if reachable)
func handleHealthzLive(w http.ResponseWriter, r *http.Request, checker HealthChecker) {
	logger := logging.FromContext(r.Context(), slog.Default())
	logger = logging.WithModule(logger, "health")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	resp := HealthzLiveResponse{Status: "alive"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn("failed to write liveness response", "error", err)
	}
}

// handleHealthzReady handles the /healthz/ready endpoint
// Returns 200 only if all components are healthy (Deepgram connected, DB accessible, mic open)
// Returns 503 with component breakdown if any component is unhealthy
func handleHealthzReady(w http.ResponseWriter, r *http.Request, checker HealthChecker) {
	logger := logging.FromContext(r.Context(), slog.Default())
	logger = logging.WithModule(logger, "health")

	w.Header().Set("Content-Type", "application/json")

	// Check component health
	deepgramStatus := storage.ComponentStatusDisconnected
	if checker.IsDeepgramConnected() {
		deepgramStatus = storage.ComponentStatusConnected
	}

	dbStatus := storage.ComponentStatusError
	if checker.IsDBHealthy(r.Context()) {
		dbStatus = storage.ComponentStatusOK
	}

	micStatus := storage.ComponentStatusClosed
	if checker.IsMicOpen() {
		micStatus = storage.ComponentStatusOpen
	}

	llmStatus := checker.IsLLMHealthy()

	resp := HealthzReadyResponse{
		Deepgram: deepgramStatus,
		DB:       dbStatus,
		Mic:      micStatus,
		LLM:      llmStatus,
	}

	// Determine overall health
	allHealthy := deepgramStatus == storage.ComponentStatusConnected && dbStatus == storage.ComponentStatusOK && micStatus == storage.ComponentStatusOpen && llmStatus != storage.ComponentStatusError

	if allHealthy {
		w.WriteHeader(http.StatusOK)
		logger.Debug("readiness check passed", "deepgram", deepgramStatus, "db", dbStatus, "mic", micStatus, "llm", llmStatus)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		logger.Warn("readiness check failed", "deepgram", deepgramStatus, "db", dbStatus, "mic", micStatus, "llm", llmStatus)
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn("failed to write readiness response", "error", err)
	}
}

// DefaultHealthChecker implements HealthChecker using actual components
type DefaultHealthChecker struct {
	resilientClient interface {
		IsConnected() bool
	}
	store interface {
		Ping(ctx context.Context) error
	}
	mic interface {
		IsOpen() bool
	}
	llmTracker interface {
		Status() string
	}
}

// NewDefaultHealthChecker creates a new DefaultHealthChecker
func NewDefaultHealthChecker(resilientClient interface{ IsConnected() bool }, store interface {
	Ping(ctx context.Context) error
}, mic interface{ IsOpen() bool }, llmTracker interface{ Status() string }) *DefaultHealthChecker {
	return &DefaultHealthChecker{
		resilientClient: resilientClient,
		store:           store,
		mic:             mic,
		llmTracker:      llmTracker,
	}
}

func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// IsDeepgramConnected returns true if Deepgram is connected
func (d *DefaultHealthChecker) IsDeepgramConnected() bool {
	if isNilInterfaceValue(d.resilientClient) {
		return false
	}
	return d.resilientClient.IsConnected()
}

// IsDBHealthy returns true if the database is accessible
func (d *DefaultHealthChecker) IsDBHealthy(ctx context.Context) bool {
	if d.store == nil {
		return false
	}
	return d.store.Ping(ctx) == nil
}

// IsMicOpen returns true if the mic is open
func (d *DefaultHealthChecker) IsMicOpen() bool {
	if isNilInterfaceValue(d.mic) {
		return false
	}
	return d.mic.IsOpen()
}

func (d *DefaultHealthChecker) IsLLMHealthy() string {
	if isNilInterfaceValue(d.llmTracker) {
		return llmStatusUnchecked
	}
	status := d.llmTracker.Status()
	if status == "" {
		return llmStatusUnchecked
	}
	return status
}
