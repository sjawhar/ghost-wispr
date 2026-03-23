package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// HealthChecker defines the interface for checking component health
type HealthChecker interface {
	IsDeepgramConnected() bool
	IsDBHealthy(ctx context.Context) bool
	IsMicOpen() bool
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
	deepgramStatus := "disconnected"
	if checker.IsDeepgramConnected() {
		deepgramStatus = "connected"
	}

	dbStatus := "error"
	if checker.IsDBHealthy(r.Context()) {
		dbStatus = "ok"
	}

	micStatus := "closed"
	if checker.IsMicOpen() {
		micStatus = "open"
	}

	resp := HealthzReadyResponse{
		Deepgram: deepgramStatus,
		DB:       dbStatus,
		Mic:      micStatus,
	}

	// Determine overall health
	allHealthy := deepgramStatus == "connected" && dbStatus == "ok" && micStatus == "open"

	if allHealthy {
		w.WriteHeader(http.StatusOK)
		logger.Debug("readiness check passed", "deepgram", deepgramStatus, "db", dbStatus, "mic", micStatus)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		logger.Warn("readiness check failed", "deepgram", deepgramStatus, "db", dbStatus, "mic", micStatus)
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
}

// NewDefaultHealthChecker creates a new DefaultHealthChecker
func NewDefaultHealthChecker(resilientClient interface{ IsConnected() bool }, store interface{ Ping(ctx context.Context) error }, mic interface{ IsOpen() bool }) *DefaultHealthChecker {
	return &DefaultHealthChecker{
		resilientClient: resilientClient,
		store:           store,
		mic:             mic,
	}
}

// IsDeepgramConnected returns true if Deepgram is connected
func (d *DefaultHealthChecker) IsDeepgramConnected() bool {
	if d.resilientClient == nil {
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
	if d.mic == nil {
		return false
	}
	return d.mic.IsOpen()
}
