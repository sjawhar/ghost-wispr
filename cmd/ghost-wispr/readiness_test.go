package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/server"
)

// deepgramFake is a Deepgram client source whose connection a test sets.
type deepgramFake struct{ connected atomic.Bool }

func (d *deepgramFake) IsConnected() bool { return d.connected.Load() }

// pingOK is a database that always answers.
type pingOK struct{}

func (pingOK) Ping(context.Context) error { return nil }

// readiness answers GET /healthz/ready through the app's HTTP handler.
func readiness(t *testing.T, checker server.HealthChecker) (int, server.HealthzReadyResponse) {
	t.Helper()
	handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{}, "", checker)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
	var ready server.HealthzReadyResponse
	if err := json.Unmarshal(response.Body.Bytes(), &ready); err != nil {
		t.Fatalf("decode readiness: %v (body=%s)", err, response.Body.String())
	}
	return response.Code, ready
}

// TestReadinessStaysReadyWhileAResumeBringsCaptureBack fails if readiness
// answers 503 in the moment after a resume opens the gate and before the
// microphone has started and Deepgram has connected: a unit that is only
// resuming is not failing. Once the resume's start has finished, Deepgram not
// connected is a fault again.
func TestReadinessStaysReadyWhileAResumeBringsCaptureBack(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	mic := &openMicFake{}
	deepgram := &deepgramFake{}
	checker := server.NewDefaultHealthChecker(deepgram, pingOK{}, mic, capture, nil)
	expect := func(step string, wantCode int, wantMic, wantDeepgram string) {
		t.Helper()
		code, ready := readiness(t, checker)
		if code != wantCode || ready.Mic != wantMic || ready.Deepgram != wantDeepgram {
			t.Fatalf("%s: readiness = %d with mic %q and deepgram %q, want %d with %q and %q", step, code, ready.Mic, ready.Deepgram, wantCode, wantMic, wantDeepgram)
		}
	}

	expect("paused", http.StatusOK, "paused", "paused")
	capture.Resume()
	waitOpen(t, capture)
	expect("gate open, microphone and Deepgram not back yet", http.StatusOK, "resuming", "resuming")
	mic.open.Store(true)
	expect("microphone back, Deepgram connecting", http.StatusOK, "open", "resuming")
	deepgram.connected.Store(true)
	capture.finishResume()
	expect("the resume's start finished", http.StatusOK, "open", "connected")
	deepgram.connected.Store(false)
	expect("Deepgram lost after the resume", http.StatusServiceUnavailable, "open", "disconnected")
}
