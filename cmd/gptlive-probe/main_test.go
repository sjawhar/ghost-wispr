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
	// wordsAfter sends the default answer's words this long after its audio,
	// rather than just before it as GPT-Live does.
	wordsAfter time.Duration
	// answer replies to a text turn through send, with at the timeline now;
	// nil sends 1.2 s of speech and its words.
	answer func(send func(any), at func() int64)
	// delegate makes the Live server delegate once it has heard loud input
	// audio and then quiet, as GPT-Live does for a spoken question.
	delegate  bool
	loudHeard bool
	delegated bool
	// answered gets the commentary returned on the delegation.
	answered chan string
}

// probeDelegation is the stand-in's delegation id.
const probeDelegation = "item_probe"

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
	s := &standIns{bearer: make(chan string, 1), said: make(chan string, 8), start: make(chan map[string]any, 1), answered: make(chan string, 4)}
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
				Type         string         `json:"type"`
				EventID      string         `json:"event_id"`
				Content      string         `json:"content"`
				Audio        string         `json:"audio"`
				DelegationID *string        `json:"delegation_id"`
				Session      map[string]any `json:"session"`
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
					if s.delegate && !s.delegated {
						loud := false
						for i := 0; i+1 < len(b); i += 2 {
							if v := int16(uint16(b[i]) | uint16(b[i+1])<<8); v > 2000 || v < -2000 {
								loud = true
								break
							}
						}
						if loud {
							s.loudHeard = true
						} else if s.loudHeard {
							s.delegated = true
							send(map[string]any{"type": "session.delegation.created", "event_id": "e5", "offset_ms": s.inputMs(), "delegation": map[string]any{"id": probeDelegation, "type": "delegation", "target": "client"}})
						}
					}
				}
			case "session.thinking.append":
				send(map[string]any{"type": "session.thinking.appended", "event_id": "e6", "client_event_id": ev.EventID})
			case "session.commentary.append":
				send(map[string]any{"type": "session.commentary.appended", "event_id": "e2", "client_event_id": ev.EventID})
				if ev.DelegationID != nil && *ev.DelegationID == probeDelegation {
					s.answered <- ev.Content
					send(transcript("The backend says delegation works.", s.inputMs()))
					send(speechDelta(time.Second))
					continue
				}
				s.said <- ev.Content
				if s.answer != nil {
					go s.answer(send, s.inputMs)
					continue
				}
				if s.wordsAfter > 0 {
					go func() {
						// 1.2 s of speech in real time, its words partway in.
						begin := time.Now()
						sent := false
						for range 12 {
							send(speechDelta(100 * time.Millisecond))
							if !sent && time.Since(begin) >= s.wordsAfter {
								send(transcript("Hello from the stand-in.", s.inputMs()))
								sent = true
							}
							time.Sleep(100 * time.Millisecond)
						}
					}()
					continue
				}
				send(transcript("Hello from the stand-in.", s.inputMs()))
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
		"speech lead: 3 speeches", "each within 300ms", "result: PASS",
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

// The probe fails when a speech's first word reaches the own-speech filter
// over 300 ms after its audio starts: the room would hear Wispr before the
// filter knew its words.
func TestTheProbeFailsWhenWordsComeAfterTheirAudio(t *testing.T) {
	s := newStandIns(t)
	s.wordsAfter = 600 * time.Millisecond
	cfg := s.config(t)
	cfg.Settle = time.Second
	report, err := runProbe(t, s, &cfg)
	if err == nil || !strings.Contains(err.Error(), "after its audio started") {
		t.Fatalf("probe whose words come 600 ms after their audio = %v, want that failure\n%s", err, report)
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

// A speech whose first word reaches the filter by 300 ms after its audio
// starts passes; a later one, a speech with no words, and nothing measured
// fail the probe.
func TestReportLeadNamesItsVerdict(t *testing.T) {
	ms := time.Millisecond
	at := time.Unix(1_800_000_000, 0)
	sp := func(start, first time.Duration, words bool) spoken {
		s := spoken{start: at.Add(start)}
		if words {
			s.firstWord = at.Add(first)
		}
		return s
	}
	for name, c := range map[string]struct {
		speeches []spoken
		want     string
		fail     bool
	}{
		"text first":    {speeches: []spoken{sp(0, -240*ms, true), sp(5*time.Second, 5*time.Second-100*ms, true)}, want: "from -240ms to -100ms"},
		"just in time":  {speeches: []spoken{sp(0, 300*ms, true)}, want: "within 300ms"},
		"late":          {speeches: []spoken{sp(0, -50*ms, true), sp(time.Second, time.Second+400*ms, true)}, want: "speech 2's first word reached the filter 400ms after its audio started", fail: true},
		"no words":      {speeches: []spoken{sp(0, 0, false)}, want: "speech 1: no words reached the filter", fail: true},
		"nothing heard": {want: "UNKNOWN", fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportLead(&out, c.speeches)
			if !strings.Contains(out.String(), c.want) || (err != nil) != c.fail {
				t.Fatalf("report %q, error %v; want %q and a failure %v", out.String(), err, c.want, c.fail)
			}
		})
	}
}

// The watcher gives a word that arrives between speeches to the next one,
// since GPT-Live sends a speech's text just before its audio.
func TestTheWatcherGivesEachSpeechItsFirstWord(t *testing.T) {
	c := make(chan voice.Event, 8)
	e := watch(c)
	c <- voice.Event{Kind: voice.EventAgentText, Text: "one"}
	time.Sleep(20 * time.Millisecond)
	c <- voice.Event{Kind: voice.EventSpeechStarted}
	c <- voice.Event{Kind: voice.EventAgentText, Text: " two"}
	c <- voice.Event{Kind: voice.EventSpeechEnded}
	c <- voice.Event{Kind: voice.EventSpeechStarted}
	time.Sleep(20 * time.Millisecond)
	c <- voice.Event{Kind: voice.EventAgentText, Text: "late"}
	c <- voice.Event{Kind: voice.EventSpeechEnded}
	c <- voice.Event{Kind: voice.EventSpeechStarted}
	close(c)
	time.Sleep(50 * time.Millisecond)
	got := e.speeches()
	if len(got) != 3 {
		t.Fatalf("%d speeches, want 3", len(got))
	}
	if lead := got[0].firstWord.Sub(got[0].start); lead >= 0 {
		t.Errorf("speech 1's first word came %v after its audio, want before: it arrived first", lead)
	}
	if lead := got[1].firstWord.Sub(got[1].start); lead < 15*time.Millisecond {
		t.Errorf("speech 2's first word came %v after its audio, want about 20ms", lead)
	}
	if !got[2].firstWord.IsZero() {
		t.Errorf("speech 3 got a word, want none")
	}
}

// question is d of a loud tone at 16 kHz: the stand-in takes it for speech.
func question(d time.Duration) []int16 {
	pcm := make([]int16, int(d*16000/time.Second))
	for i := range pcm {
		pcm[i] = 8000
		if i/18%2 == 0 {
			pcm[i] = -8000
		}
	}
	return pcm
}

// The probe asks GPT-Live a spoken question, which GPT-Live delegates to
// the client, and reports the round trip: the delegation, the backend's
// answer and its speech.
func TestTheProbeReportsADelegationRoundTrip(t *testing.T) {
	s := newStandIns(t)
	s.delegate = true
	cfg := s.config(t)
	cfg.Settle, cfg.Turns = time.Second, 2
	cfg.Ask = question(time.Second)
	report, err := runProbe(t, s, &cfg)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, report)
	}
	select {
	case got := <-s.answered:
		if got != delegationAnswer {
			t.Errorf("the delegation was answered %q, want the probe's answer", got)
		}
	default:
		t.Error("the probe returned no answer on the delegation")
	}
	for _, want := range []string{"question: 1.00 s of audio sent", "delegation: GPT-Live delegated", "the backend answered", "its answer began playing", "result: PASS"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not say %q:\n%s", want, report)
		}
	}
}

func TestTheProbeFailsWhenGPTLiveDoesNotDelegate(t *testing.T) {
	s := newStandIns(t)
	cfg := s.config(t)
	cfg.Settle, cfg.Turns = time.Second, 2
	cfg.Ask = question(time.Second)
	cfg.DelegateWait = 2 * time.Second
	if report, err := runProbe(t, s, &cfg); err == nil || !strings.Contains(err.Error(), "did not delegate") {
		t.Fatalf("probe whose question GPT-Live did not delegate = %v, want that failure\n%s", err, report)
	}
}

func TestTheProbeReadsAQuestionWAV(t *testing.T) {
	dir := t.TempDir()
	rec := &recording{pcm: []byte{1, 0, 2, 0, 0xff, 0xff}}
	good := filepath.Join(dir, "q.wav")
	if err := os.WriteFile(good, rec.wavAt(16000), 0o600); err != nil {
		t.Fatal(err)
	}
	pcm, err := readQuestion(good)
	if err != nil || fmt.Sprint(pcm) != "[1 2 -1]" {
		t.Fatalf("readQuestion = %v, %v; want [1 2 -1]", pcm, err)
	}
	bad := filepath.Join(dir, "q24.wav")
	if err := os.WriteFile(bad, rec.wavAt(24000), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readQuestion(bad); err == nil || !strings.Contains(err.Error(), "16000") {
		t.Fatalf("readQuestion of a 24 kHz file = %v, want a refusal naming 16000 Hz", err)
	}
}
