package concierge

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
)

// The app_state keys of the unit's Envoy session: its id, made at the first
// start and kept, so an answer sent while the unit restarted still reaches
// it, and its resume point.
const (
	envoySessionIDKey = "voice_envoy_session_id"
	envoyResumeKey    = "voice_envoy_resume"
)

// AppState is where the unit keeps small values; *storage.SQLiteStore is one.
type AppState interface {
	GetAppState(key string) (string, bool, error)
	SetAppState(key, value string) error
}

// EnvoyState is what envoy.SessionConfig takes from the unit's state.
type EnvoyState struct {
	SessionID  string
	Resume     *envoy.ResumePoint
	SaveResume func(envoy.ResumePoint) error
}

// LoadEnvoyState reads the unit's Envoy session id, making one at the first
// start, and its resume point.
func LoadEnvoyState(state AppState) (EnvoyState, error) {
	id, ok, err := state.GetAppState(envoySessionIDKey)
	if err != nil {
		return EnvoyState{}, err
	}
	if !ok || envoy.ValidSessionID(id) != nil {
		id = uuid.NewString()
		if err := state.SetAppState(envoySessionIDKey, id); err != nil {
			return EnvoyState{}, err
		}
	}
	loaded := EnvoyState{
		SessionID: id,
		SaveResume: func(point envoy.ResumePoint) error {
			encoded, err := json.Marshal(point)
			if err != nil {
				return err
			}
			return state.SetAppState(envoyResumeKey, string(encoded))
		},
	}
	raw, ok, err := state.GetAppState(envoyResumeKey)
	if err != nil {
		return EnvoyState{}, err
	}
	if ok {
		var point envoy.ResumePoint
		if err := json.Unmarshal([]byte(raw), &point); err != nil {
			return EnvoyState{}, fmt.Errorf("read the Envoy resume point: %w", err)
		}
		if point.StreamID != "" {
			loaded.Resume = &point
		}
	}
	return loaded, nil
}
