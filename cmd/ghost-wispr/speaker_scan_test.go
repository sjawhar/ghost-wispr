package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/ghost-wispr/internal/server"
)

// TestTheSpeakerSaysWhyItCannotPlayBeforeAScanWorks fails if, before any
// microphone start has run PortAudio's device scan, as in a unit that starts
// paused, or after the scan failed, GET /api/status does not warn why the
// speaker cannot play and POST /api/tts/speak does not refuse with that reason
// (409), or if either still does once a scan has worked.
func TestTheSpeakerSaysWhyItCannotPlayBeforeAScanWorks(t *testing.T) {
	var scan portAudioScan
	warnings := newWarningsRegistry()
	spoken := 0
	handler, err := server.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.ModePerm}}, server.NewHub(), nil, &server.ControlHooks{
		Warnings:        warnings.List,
		TTSSpeak:        func(string, string, string) (string, error) { spoken++; return "speech", nil },
		TTSHasSpeaker:   func() bool { return true },
		TTSHasProvider:  func() bool { return true },
		TTSSpeakerWaits: scan.speakerWaits,
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	speak := func() (int, string) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/tts/speak", strings.NewReader(`{"text":"hello"}`)))
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body.Error
	}
	speakerWarned := func() []string {
		return slices.DeleteFunc(statusWarnings(t, warnings), func(w string) bool { return !strings.HasPrefix(w, "Speaker unavailable") })
	}

	for _, step := range []struct {
		name   string
		before func()
		reason string
	}{
		{name: "no scan yet", before: func() {}, reason: speakerWaitsForScanWarning},
		{name: "the scan failed", before: func() { scan.finished(false) }, reason: speakerNoPortAudioWarning},
	} {
		step.before()
		scan.warn(warnings)
		if got := speakerWarned(); !slices.Equal(got, []string{step.reason}) {
			t.Fatalf("%s: status speaker warnings = %q, want %q", step.name, got, step.reason)
		}
		if code, reason := speak(); code != http.StatusConflict || reason != step.reason {
			t.Fatalf("%s: speak = %d %q, want 409 %q", step.name, code, reason, step.reason)
		}
	}
	if spoken != 0 {
		t.Fatalf("%d speech requests reached the speaker while it could not play", spoken)
	}

	scan.finished(true)
	scan.warn(warnings)
	if got := speakerWarned(); len(got) != 0 {
		t.Fatalf("status speaker warnings once a scan worked = %q, want none", got)
	}
	if code, reason := speak(); code != http.StatusAccepted || spoken != 1 {
		t.Fatalf("speak once a scan worked = %d %q (%d spoken), want 202 and one request", code, reason, spoken)
	}
	if !strings.Contains(speakerWaitsForScanWarning, "first resume") {
		t.Fatalf("the warning %q does not say a unit that starts paused speaks only after its first resume", speakerWaitsForScanWarning)
	}
}
