package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusServer is the office unit's local status endpoint, under test
// control. It holds no session text.
type statusServer struct {
	mu     sync.Mutex
	paused bool
	active string
	server *httptest.Server
}

func newStatusServer(t *testing.T, paused bool, active string) *statusServer {
	t.Helper()
	s := &statusServer{paused: paused, active: active}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"paused":            s.paused,
			"active_session_id": s.active,
		})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *statusServer) set(paused bool, active string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused, s.active = paused, active
}

func TestProbeRefusesBeforeTokensWhenTheUnitIsNotPausedAndIdle(t *testing.T) {
	for name, state := range map[string]struct {
		paused bool
		active string
	}{
		"recording": {paused: false},
		"active":    {paused: true, active: "recording-session"},
	} {
		t.Run(name, func(t *testing.T) {
			live := newStandIns(t)
			status := newStatusServer(t, state.paused, state.active)
			cfg := live.config(t)
			cfg.StatusURL = status.server.URL
			report, err := runProbe(t, live, &cfg)
			if err == nil || !strings.Contains(err.Error(), "safety guard") {
				t.Fatalf("probe while the unit is %s = %v, want safety guard refusal\n%s", name, err, report)
			}
			if live.tokenCalls.Load() != 0 {
				t.Fatalf("the safety guard let %d token exchanges leave the unit", live.tokenCalls.Load())
			}
			select {
			case <-live.conns:
				t.Fatal("the safety guard opened GPT-Live")
			default:
			}
		})
	}
}

func TestProbeChecksAgainBeforeItsFirstAudio(t *testing.T) {
	live := newStandIns(t)
	status := newStatusServer(t, true, "")
	live.onStarted = func() { status.set(false, "someone-else") }
	cfg := live.config(t)
	cfg.StatusURL = status.server.URL
	cfg.Settle = time.Second
	report, err := runProbe(t, live, &cfg)
	if err == nil || !strings.Contains(err.Error(), "before audio") {
		t.Fatalf("probe whose unit became active after session.start = %v, want before-audio guard refusal\n%s", err, report)
	}
	if live.heardAudio.Load() {
		t.Fatal("the probe sent lead-in audio after the second safety guard failed")
	}
}
