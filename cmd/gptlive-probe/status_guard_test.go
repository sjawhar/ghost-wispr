package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusServer is the office unit's local status endpoint, under test
// control. It holds no session text.
type statusServer struct {
	mu        sync.Mutex
	paused    bool
	capturing bool
	active    string
	server    *httptest.Server
}

func newStatusServer(t *testing.T, paused bool, active string) *statusServer {
	t.Helper()
	s := &statusServer{paused: paused, active: active}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"paused":            s.paused,
			"capturing":         s.capturing,
			"active_session_id": s.active,
		})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *statusServer) set(paused bool, active string) {
	s.setState(paused, false, active)
}

func (s *statusServer) setState(paused, capturing bool, active string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused, s.capturing, s.active = paused, capturing, active
}

func TestProbeRefusesBeforeTokensWhenTheUnitIsNotPausedAndIdle(t *testing.T) {
	for name, state := range map[string]struct {
		paused    bool
		capturing bool
		active    string
	}{
		"recording":            {paused: false},
		"capturing while idle": {paused: true, capturing: true},
		"active":               {paused: true, active: "recording-session"},
	} {
		t.Run(name, func(t *testing.T) {
			live := newStandIns(t)
			status := newStatusServer(t, state.paused, state.active)
			status.setState(state.paused, state.capturing, state.active)
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

// The executable parses its flags before it enters the safety guard. This
// subprocess is a fresh process like the arm64 probe on the Pi.
func TestSolReviewCLIParsesSafetyGuardFlags(t *testing.T) {
	status := newStatusServer(t, false, "")
	if os.Getenv("GPTLIVE_CLI_HELPER") == "1" {
		os.Args = []string{
			"gptlive-probe",
			"--identity-provider-id", "idp_synthetic",
			"--service-account-id", "user-synthetic",
			"--status-url", os.Getenv("GPTLIVE_STATUS_URL"),
			"--timeout", "2s",
		}
		_ = os.Unsetenv(clientCertificateEnv)
		code := run()
		if os.Getenv(clientCertificateEnv) != "false" {
			os.Exit(3)
		}
		os.Exit(code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestSolReviewCLIParsesSafetyGuardFlags")
	cmd.Env = append(os.Environ(), "GPTLIVE_CLI_HELPER=1", "GPTLIVE_STATUS_URL="+status.server.URL)
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run exit = %v, want 1; output:\n%s", err, out)
	}
	if exit.ExitCode() == 3 {
		t.Fatalf("run did not call clientCertificatesOff in production; output:\n%s", out)
	}
	if exit.ExitCode() != 1 {
		t.Fatalf("run exit = %v, want 1; output:\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "build commit: ") {
		t.Fatalf("run does not print its build commit first:\n%s", out)
	}
	if strings.Contains(string(out), "needs --identity-provider-id") || !strings.Contains(string(out), "probe safety guard at start") {
		t.Fatalf("run did not parse flags into the safety guard:\n%s", out)
	}
}
