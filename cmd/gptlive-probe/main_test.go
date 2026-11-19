package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// fakeAccessToken is the stand-in token endpoint's access token.
var fakeAccessToken = strings.Join([]string{"stand", "in", "access"}, "-")

// The subject tokens the probe's Subject func mints: one for OpenAI's
// audience, one for any other (the control).
const (
	openAISubject  = "google-id-token"
	controlSubject = "wrong-audience-token"
)

// standIns are a stand-in OpenAI token endpoint and Live server. By default
// the Live server answers a commentary append with 1.2 s of speech and its
// words.
type standIns struct {
	token, live *httptest.Server
	bearer      chan string
	said        chan string
	start       chan map[string]any
	heardAudio  atomic.Bool // the session sent input audio before its text turn
	// acceptAny makes the token endpoint take any subject token, so the
	// wrong-audience control is not refused.
	acceptAny bool
	// answer replies to the text turn through send; nil sends 1.2 s of
	// speech and its words.
	answer func(send func(any))
}

func newStandIns(t *testing.T) *standIns {
	t.Helper()
	s := &standIns{bearer: make(chan string, 1), said: make(chan string, 1), start: make(chan map[string]any, 1)}
	s.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case s.acceptAny:
		case req["subject_token"] == controlSubject:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid subject token.","type":"invalid_request_error","code":"invalid_subject_token"}}`))
			return
		case req["subject_token"] != openAISubject || req["service_account_id"] != "user-probe" || req["identity_provider_id"] != "idp_probe":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"The provided service_account_id mapping does not match token attributes.","type":"invalid_request_error","code":"invalid_grant"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fakeAccessToken, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(s.token.Close)
	upgrader := websocket.Upgrader{}
	s.live = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.bearer <- r.Header.Get("Authorization")
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		var writeMu sync.Mutex
		send := func(v any) {
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = ws.WriteJSON(v)
		}
		for {
			var ev struct {
				Type    string         `json:"type"`
				EventID string         `json:"event_id"`
				Content string         `json:"content"`
				Session map[string]any `json:"session"`
			}
			if err := ws.ReadJSON(&ev); err != nil {
				return
			}
			switch ev.Type {
			case "session.start":
				s.start <- ev.Session
				send(map[string]any{"type": "session.started", "event_id": "e1", "session": map[string]any{"id": "sess_probe"}})
			case "session.input_audio.append":
				s.heardAudio.Store(true)
			case "session.commentary.append":
				s.said <- ev.Content
				send(map[string]any{"type": "session.commentary.appended", "event_id": "e2", "client_event_id": ev.EventID})
				if s.answer != nil {
					go s.answer(send)
					continue
				}
				send(map[string]any{"type": "session.output_transcript.delta", "event_id": "e3", "delta": "Hello from the stand-in."})
				for range 2 {
					send(speechDelta(600 * time.Millisecond))
				}
			case "session.close":
				send(map[string]any{"type": "session.closed", "event_id": "e4", "reason": "close_requested", "usage": map[string]any{"seconds": 2}})
				return
			}
		}
	}))
	t.Cleanup(s.live.Close)
	return s
}

// speechDelta is d of a square wave at 1,000 of 32,767: speech, not silence.
func speechDelta(d time.Duration) map[string]any {
	pcm := make([]byte, 2*int(d*24000/time.Second))
	for i := 0; i < len(pcm); i += 2 {
		v := int16(1000)
		if i/2%48 < 24 {
			v = -1000
		}
		pcm[i], pcm[i+1] = byte(uint16(v)), byte(uint16(v)>>8)
	}
	return map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)}
}

func (s *standIns) config(t *testing.T) probeConfig {
	return probeConfig{
		TokenURL:           s.token.URL,
		LiveURL:            "ws" + strings.TrimPrefix(s.live.URL, "http"),
		IdentityProviderID: "idp_probe",
		ServiceAccountID:   "user-probe",
		Subject: func(_ context.Context, audience string) (string, error) {
			if audience == gptlive.Audience {
				return openAISubject, nil
			}
			return controlSubject, nil
		},
		Text:     "Say hello.",
		MinAudio: time.Second,
		WAV:      filepath.Join(t.TempDir(), "probe.wav"),
	}
}

func runProbe(t *testing.T, s *standIns, cfg *probeConfig) (string, error) {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := probe(ctx, cfg, &out)
	return out.String(), err
}

func TestTheProbeExchangesATokenAndHearsGPTLiveAnswerAText(t *testing.T) {
	s := newStandIns(t)
	cfg := s.config(t)
	cfg.Settle = time.Second // nothing comes after Say; the 10 s default is TestTheProbeFailsOnASecondSentenceFiveSecondsLater's
	report, err := runProbe(t, s, &cfg)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, report)
	}
	if got := <-s.bearer; got != "Bearer "+fakeAccessToken {
		t.Errorf("the Live connection carried %q, want the exchanged token", got)
	}
	if got := <-s.said; got != "Say hello." {
		t.Errorf("the probe's text turn was %q", got)
	}
	start := <-s.start
	if start["model"] != "gpt-live-1" || fmt.Sprint(start["delegation"]) != "map[type:client]" || start["store"] != false {
		t.Errorf("session.start config %v, want gpt-live-1, client delegation and store false", start)
	}
	if !s.heardAudio.Load() {
		t.Error("the probe sent GPT-Live no input audio: its timeline runs on input audio")
	}
	for _, want := range []string{"control: a token for another audience was refused (invalid_subject_token)", "token exchange: ok", "session: started", "audio: 1.2", "words: Hello from the stand-in.", "timeline offset:", "result: PASS"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not say %q:\n%s", want, report)
		}
	}
	for _, secret := range []string{fakeAccessToken, openAISubject} {
		if strings.Contains(report, secret) {
			t.Errorf("report holds a token:\n%s", report)
		}
	}
	wav, err := os.ReadFile(cfg.WAV)
	if err != nil {
		t.Fatalf("read the probe's WAV: %v", err)
	}
	if len(wav) != 44+2*24000*12/10 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Errorf("the probe's WAV is %d bytes, want a 44-byte header and 1.2 s of 24 kHz mono PCM16", len(wav))
	}
}

func TestTheProbeNamesARefusedExchange(t *testing.T) {
	s := newStandIns(t)
	cfg := s.config(t)
	cfg.ServiceAccountID = "user-unmapped"
	if report, err := runProbe(t, s, &cfg); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("probe with no mapping = %v, want the exchange's invalid_grant\n%s", err, report)
	}
}

// The control tells "OpenAI rejected the token" from "no mapping names it":
// a token endpoint that takes any token proves nothing.
func TestTheProbeFailsWhenTheWrongAudienceControlIsAccepted(t *testing.T) {
	s := newStandIns(t)
	s.acceptAny = true
	cfg := s.config(t)
	if report, err := runProbe(t, s, &cfg); err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("probe with a token endpoint that takes any token = %v, want the control's failure\n%s", err, report)
	}
}

// A long answer of many pieces plays to its end and passes.
func TestTheProbeHearsALongAnswerToItsEnd(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any)) {
		for i := range 120 {
			send(map[string]any{"type": "session.output_transcript.delta", "event_id": fmt.Sprintf("t%d", i), "delta": fmt.Sprintf("piece%d ", i)})
			send(speechDelta(100 * time.Millisecond))
		}
	}
	cfg := s.config(t)
	cfg.Settle = time.Second
	report, err := runProbe(t, s, &cfg)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, report)
	}
	if !strings.Contains(report, "audio: 12.00 s") || !strings.Contains(report, "piece119") {
		t.Fatalf("report does not hold the whole 12 s answer:\n%s", report)
	}
}

// An answer GPT-Live goes on with after Say has returned was cut by an
// exchange that ends on Say: the probe fails rather than passes.
func TestTheProbeFailsOnACutAnswer(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any)) {
		send(speechDelta(1200 * time.Millisecond))
		time.Sleep(1800 * time.Millisecond)
		send(speechDelta(600 * time.Millisecond))
	}
	cfg := s.config(t)
	if report, err := runProbe(t, s, &cfg); err == nil || !strings.Contains(err.Error(), "cut") {
		t.Fatalf("probe with an answer that goes on after Say returned = %v, want a failure for a cut answer\n%s", err, report)
	}
}

// GPT-Live marks no end of an answer, so the probe listens 10 s after Say
// returns: a second sentence 5 s after the first fails it.
func TestTheProbeFailsOnASecondSentenceFiveSecondsLater(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any)) {
		send(speechDelta(1200 * time.Millisecond))
		time.Sleep(5 * time.Second)
		send(speechDelta(600 * time.Millisecond))
	}
	cfg := s.config(t)
	report, err := runProbe(t, s, &cfg)
	if err == nil || !strings.Contains(err.Error(), "Say returned before the answer finished") {
		t.Fatalf("probe with a second sentence 5 s after the first = %v, want a failure naming that Say returned too early\n%s", err, report)
	}
}

func TestTheProbeFailsOnTooLittleSpeech(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any)) { send(speechDelta(300 * time.Millisecond)) }
	cfg := s.config(t)
	cfg.Settle = time.Second
	if report, err := runProbe(t, s, &cfg); err == nil {
		t.Fatalf("probe with 0.3 s of speech passed:\n%s", report)
	}
}

func TestTheProbeFailsOnAServerError(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any)) {
		send(speechDelta(1200 * time.Millisecond))
		send(map[string]any{"type": "error", "event_id": "e9", "error": map[string]any{"type": "server_error", "code": "internal_error", "message": "boom"}})
	}
	cfg := s.config(t)
	cfg.Settle = time.Second
	if report, err := runProbe(t, s, &cfg); err == nil || !strings.Contains(err.Error(), "internal_error") {
		t.Fatalf("probe with a server error = %v, want the error\n%s", err, report)
	}
}

// The unit's service sets GOOGLE_API_USE_CLIENT_CERTIFICATE=false for its
// ID-token paths; the probe does the same unless the caller chose.
func TestTheProbeTurnsOffGoogleClientCertificatesUnlessSet(t *testing.T) {
	t.Setenv(clientCertificateEnv, "")
	_ = os.Unsetenv(clientCertificateEnv)
	clientCertificatesOff()
	if got := os.Getenv(clientCertificateEnv); got != "false" {
		t.Fatalf("%s = %q after clientCertificatesOff, want false", clientCertificateEnv, got)
	}
	t.Setenv(clientCertificateEnv, "true")
	clientCertificatesOff()
	if got := os.Getenv(clientCertificateEnv); got != "true" {
		t.Fatalf("clientCertificatesOff changed the caller's %s=true to %q", clientCertificateEnv, got)
	}
}

// offsetSession is a session that reports the offsets it measured.
type offsetSession struct {
	voice.Session
	offsets []time.Duration
}

func (s offsetSession) TimelineOffsets() []time.Duration { return s.offsets }

// R4T9: a spread over 200 ms gets the sideband verdict; one within it gets
// the margin's.
func TestReportOffsetsNamesAnUnstableSpread(t *testing.T) {
	var out bytes.Buffer
	reportOffsets(&out, offsetSession{offsets: []time.Duration{-100 * time.Millisecond, 150 * time.Millisecond}})
	if !strings.Contains(out.String(), "unstable") || !strings.Contains(out.String(), "sideband") {
		t.Fatalf("report %q, want the unstable verdict for a 250 ms spread", out.String())
	}
	out.Reset()
	reportOffsets(&out, offsetSession{offsets: []time.Duration{-50 * time.Millisecond, 50 * time.Millisecond}})
	if !strings.Contains(out.String(), "covers it") {
		t.Fatalf("report %q, want CutMargin to cover a 100 ms spread within 50 ms", out.String())
	}
	out.Reset()
	reportOffsets(&out, offsetSession{offsets: []time.Duration{200 * time.Millisecond, 250 * time.Millisecond}})
	if !strings.Contains(out.String(), "raise gptlive.CutMargin") {
		t.Fatalf("report %q, want a larger margin for fragments 250 ms outside their speech", out.String())
	}
	out.Reset()
	reportOffsets(&out, offsetSession{})
	if !strings.Contains(out.String(), "UNKNOWN") {
		t.Fatalf("report %q, want UNKNOWN with nothing measured", out.String())
	}
}
