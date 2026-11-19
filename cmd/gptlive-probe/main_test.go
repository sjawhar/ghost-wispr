package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeAccessToken is the stand-in token endpoint's access token.
var fakeAccessToken = strings.Join([]string{"stand", "in", "access"}, "-")

// standIns are a stand-in OpenAI token endpoint and Live server. The Live
// server answers a commentary append with 1.2 s of speech and its words.
type standIns struct {
	token, live *httptest.Server
	bearer      chan string
	said        chan string
}

func newStandIns(t *testing.T) *standIns {
	t.Helper()
	s := &standIns{bearer: make(chan string, 1), said: make(chan string, 1)}
	s.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["subject_token"] != "google-id-token" || req["service_account_id"] != "user-probe" || req["identity_provider_id"] != "idp_probe" {
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
		for {
			var ev struct {
				Type    string `json:"type"`
				EventID string `json:"event_id"`
				Content string `json:"content"`
			}
			if err := ws.ReadJSON(&ev); err != nil {
				return
			}
			switch ev.Type {
			case "session.start":
				_ = ws.WriteJSON(map[string]any{"type": "session.started", "event_id": "e1", "session": map[string]any{"id": "sess_probe"}})
			case "session.commentary.append":
				s.said <- ev.Content
				_ = ws.WriteJSON(map[string]any{"type": "session.commentary.appended", "event_id": "e2", "client_event_id": ev.EventID})
				_ = ws.WriteJSON(map[string]any{"type": "session.output_transcript.delta", "event_id": "e3", "delta": "Hello from the stand-in."})
				// 0.6 s of a square wave at 1,000 of 32,767: speech, not silence.
				pcm := make([]byte, 2*24000*6/10)
				for i := 0; i < len(pcm); i += 2 {
					v := int16(1000)
					if i/2%48 < 24 {
						v = -1000
					}
					pcm[i], pcm[i+1] = byte(uint16(v)), byte(uint16(v)>>8)
				}
				speech := base64.StdEncoding.EncodeToString(pcm)
				for range 2 {
					_ = ws.WriteJSON(map[string]any{"type": "session.output_audio.delta", "delta": speech})
				}
			case "session.close":
				_ = ws.WriteJSON(map[string]any{"type": "session.closed", "event_id": "e4", "reason": "close_requested", "usage": map[string]any{"seconds": 2}})
				return
			}
		}
	}))
	t.Cleanup(s.live.Close)
	return s
}

func (s *standIns) config(t *testing.T) probeConfig {
	return probeConfig{
		TokenURL:           s.token.URL,
		LiveURL:            "ws" + strings.TrimPrefix(s.live.URL, "http"),
		IdentityProviderID: "idp_probe",
		ServiceAccountID:   "user-probe",
		Subject:            func(context.Context, string) (string, error) { return "google-id-token", nil },
		Text:               "Say hello.",
		MinAudio:           time.Second,
		WAV:                filepath.Join(t.TempDir(), "probe.wav"),
	}
}

func TestTheProbeExchangesATokenAndHearsGPTLiveAnswerAText(t *testing.T) {
	s := newStandIns(t)
	cfg := s.config(t)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := probe(ctx, &cfg, &out); err != nil {
		t.Fatalf("probe: %v\n%s", err, out.String())
	}
	if got := <-s.bearer; got != "Bearer "+fakeAccessToken {
		t.Errorf("the Live connection carried %q, want the exchanged token", got)
	}
	if got := <-s.said; got != "Say hello." {
		t.Errorf("the probe's text turn was %q", got)
	}
	report := out.String()
	for _, want := range []string{"token exchange: ok", "session: started", "audio: 1.2", "words: Hello from the stand-in.", "result: PASS"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not say %q:\n%s", want, report)
		}
	}
	for _, secret := range []string{fakeAccessToken, "google-id-token"} {
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
	var out bytes.Buffer
	err := probe(context.Background(), &cfg, &out)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("probe with no mapping = %v, want the exchange's invalid_grant", err)
	}
}
