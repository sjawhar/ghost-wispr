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
// the Live server answers each commentary append with 1.2 s of speech and
// its words, stamped on a timeline that counts the input audio it received.
type standIns struct {
	token, live *httptest.Server
	bearer      chan string
	said        chan string
	start       chan map[string]any
	heardAudio  atomic.Bool // the session sent input audio before its text turn
	// leadIn counts the input samples received within 300 ms of
	// session.started: the replayed lead-in.
	leadIn    atomic.Int64
	inSamples atomic.Int64
	lastInAt  atomic.Int64
	startedAt atomic.Int64
	// acceptAny makes the token endpoint take any subject token, so the
	// wrong-audience control is not refused.
	acceptAny bool
	// offsets shift each turn's words on the timeline from their audio; the
	// last one holds for later turns.
	offsets []int64
	// answer replies to a text turn through send, with at the timeline now;
	// nil sends 1.2 s of speech and its words.
	answer func(send func(any), at func() int64)
	turns  atomic.Int32
}

// inputMs is the stand-in's timeline now: the input audio received, plus
// the time since the last of it arrived.
func (s *standIns) inputMs() int64 {
	last := s.lastInAt.Load()
	if last == 0 {
		return time.Now().UnixMilli() - s.startedAt.Load()
	}
	return s.inSamples.Load()*1000/gptlive.SampleRate + time.Now().UnixMilli() - last
}

func newStandIns(t *testing.T) *standIns {
	t.Helper()
	s := &standIns{bearer: make(chan string, 1), said: make(chan string, 8), start: make(chan map[string]any, 1)}
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
				Audio   string         `json:"audio"`
				Session map[string]any `json:"session"`
			}
			if err := ws.ReadJSON(&ev); err != nil {
				return
			}
			switch ev.Type {
			case "session.start":
				s.start <- ev.Session
				s.startedAt.Store(time.Now().UnixMilli())
				send(map[string]any{"type": "session.started", "event_id": "e1", "session": map[string]any{"id": "sess_probe"}})
			case "session.input_audio.append":
				s.heardAudio.Store(true)
				if b, err := base64.StdEncoding.DecodeString(ev.Audio); err == nil {
					n := int64(len(b) / 2)
					s.inSamples.Add(n)
					s.lastInAt.Store(time.Now().UnixMilli())
					if time.Now().UnixMilli()-s.startedAt.Load() < 300 {
						s.leadIn.Add(n)
					}
				}
			case "session.commentary.append":
				s.said <- ev.Content
				send(map[string]any{"type": "session.commentary.appended", "event_id": "e2", "client_event_id": ev.EventID})
				turn := int(s.turns.Add(1)) - 1
				if s.answer != nil {
					go s.answer(send, s.inputMs)
					continue
				}
				offset := int64(0)
				if len(s.offsets) > 0 {
					offset = s.offsets[min(turn, len(s.offsets)-1)]
				}
				send(transcript("Hello from the stand-in.", s.inputMs()+offset))
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

// transcript is a fragment of the model's words at startMs on the timeline.
func transcript(text string, startMs int64) map[string]any {
	return map[string]any{"type": "session.output_transcript.delta", "event_id": "t", "delta": text, "start_ms": startMs, "end_ms": startMs + 100}
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	for range probeTurns {
		if got := <-s.said; got != "Say hello." {
			t.Errorf("the probe's text turn was %q", got)
		}
	}
	start := <-s.start
	if start["model"] != "gpt-live-1" || fmt.Sprint(start["delegation"]) != "map[type:client]" || start["store"] != false {
		t.Errorf("session.start config %v, want gpt-live-1, client delegation and store false", start)
	}
	if !s.heardAudio.Load() {
		t.Error("the probe sent GPT-Live no input audio: its timeline runs on input audio")
	}
	if got := s.leadIn.Load(); got < 2*gptlive.SampleRate-1 {
		t.Errorf("the probe sent %d samples at the session's start, want a 2 s lead-in, as the wake replay sends", got)
	}
	for _, want := range []string{
		"control: a token for another audience was refused (invalid_subject_token)",
		"token exchange: ok", "session: started", "audio: 3.60",
		"words: Hello from the stand-in.",
		"timeline offset: 3 speeches", "covers it", "result: PASS",
	} {
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
	if len(wav) != 44+2*24000*36/10 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Errorf("the probe's WAV is %d bytes, want a 44-byte header and 3.6 s of 24 kHz mono PCM16", len(wav))
	}
}

// The probe fails when GPT-Live's words are off from their audio by more
// than the cut-speech margin covers, early or late, or when the offset
// varies from speech to speech by over 200 ms.
func TestTheProbeFailsOnAnOffsetTheMarginDoesNotCover(t *testing.T) {
	for name, c := range map[string]struct {
		offsets []int64
		want    string
	}{
		"late":     {offsets: []int64{250}, want: "raise gptlive.CutMargin"},
		"early":    {offsets: []int64{-250}, want: "raise gptlive.CutMargin"},
		"unstable": {offsets: []int64{0, 250, -50}, want: "unstable"},
		"too far":  {offsets: []int64{2000}, want: "matched no speech"}, // past the 1.2 s speech and the margin
	} {
		t.Run(name, func(t *testing.T) {
			s := newStandIns(t)
			s.offsets = c.offsets
			cfg := s.config(t)
			cfg.Settle = time.Second
			report, err := runProbe(t, s, &cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("probe with words %v ms off their audio = %v, want a failure naming %q\n%s", c.offsets, err, c.want, report)
			}
		})
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
	s.answer = func(send func(any), at func() int64) {
		base := at()
		for i := range 120 {
			send(transcript(fmt.Sprintf("piece%d ", i), base+int64(100*i)))
			send(speechDelta(100 * time.Millisecond))
		}
	}
	cfg := s.config(t)
	cfg.Settle = time.Second
	report, err := runProbe(t, s, &cfg)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, report)
	}
	if !strings.Contains(report, "audio: 36.00 s") || !strings.Contains(report, "piece119") {
		t.Fatalf("report does not hold three whole 12 s answers:\n%s", report)
	}
}

// An answer GPT-Live goes on with after Say has returned was cut by an
// exchange that ends on Say: the probe fails rather than passes.
func TestTheProbeFailsOnACutAnswer(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any), _ func() int64) {
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
	s.answer = func(send func(any), _ func() int64) {
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
	s.answer = func(send func(any), _ func() int64) { send(speechDelta(300 * time.Millisecond)) }
	cfg := s.config(t)
	cfg.Settle = time.Second
	if report, err := runProbe(t, s, &cfg); err == nil {
		t.Fatalf("probe with 0.3 s of speech passed:\n%s", report)
	}
}

func TestTheProbeFailsOnAServerError(t *testing.T) {
	s := newStandIns(t)
	s.answer = func(send func(any), _ func() int64) {
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

// offsetSession is a session that reports the measure it took.
type offsetSession struct {
	voice.Session
	offsets   []time.Duration
	unmatched int
}

func (s offsetSession) TimelineOffsets() []time.Duration { return s.offsets }
func (s offsetSession) UnmatchedWords() int              { return s.unmatched }

// A spread over 200 ms gets the sideband verdict, an offset the margin does
// not cover gets the raise verdict, and both, like too little to measure,
// fail the probe; one within both passes.
func TestReportOffsetsNamesItsVerdict(t *testing.T) {
	ms := time.Millisecond
	for name, c := range map[string]struct {
		s    offsetSession
		want string
		fail bool
	}{
		"unstable":       {s: offsetSession{offsets: []time.Duration{-100 * ms, 150 * ms}}, want: "sideband", fail: true},
		"covered":        {s: offsetSession{offsets: []time.Duration{-50 * ms, 50 * ms}}, want: "covers it"},
		"late":           {s: offsetSession{offsets: []time.Duration{200 * ms, 250 * ms}}, want: "raise gptlive.CutMargin", fail: true},
		"early":          {s: offsetSession{offsets: []time.Duration{-250 * ms, -200 * ms}}, want: "raise gptlive.CutMargin", fail: true},
		"one speech":     {s: offsetSession{offsets: []time.Duration{10 * ms}}, want: "UNKNOWN", fail: true},
		"nothing":        {s: offsetSession{}, want: "UNKNOWN", fail: true},
		"unmatched word": {s: offsetSession{offsets: []time.Duration{0, 10 * ms}, unmatched: 2}, want: "matched no speech", fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportOffsets(&out, c.s)
			if !strings.Contains(out.String(), c.want) || (err != nil) != c.fail {
				t.Fatalf("report %q, error %v; want %q and a failure %v", out.String(), err, c.want, c.fail)
			}
		})
	}
}

// The probe and the unit send input in 250 ms frames. Should GPT-Live's
// timeline count those frames as steps, the session's clock runs up to
// 250 ms ahead of it; the probe starts each turn at a random point of a
// frame so that the spread it measures shows those steps.
func TestTheProbeStartsEachTurnAtARandomPointOfAFrame(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 50 {
		d := framePhase()
		if d < 0 || d >= 250*time.Millisecond {
			t.Fatalf("phase %v, want within one 250 ms frame", d)
		}
		seen[d.Round(10*time.Millisecond)] = true
	}
	if len(seen) < 5 {
		t.Fatalf("50 phases took %d values, want them spread over the frame", len(seen))
	}

	s := newStandIns(t)
	cfg := s.config(t)
	cfg.Settle = time.Second
	calls := 0
	cfg.Phase = func() time.Duration { calls++; return 10 * time.Millisecond }
	if report, err := runProbe(t, s, &cfg); err != nil {
		t.Fatalf("probe: %v\n%s", err, report)
	}
	if calls != probeTurns {
		t.Fatalf("the probe drew %d phases for %d turns", calls, probeTurns)
	}
}
