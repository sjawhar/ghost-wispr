package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// fakeSpeech is OpenAI's /v1/audio/speech as a test sees it: it answers each
// request with pcm, or with refusal and status 400, and keeps the voice of
// each request.
type fakeSpeech struct {
	url     string
	pcm     []byte
	refusal string

	mu     sync.Mutex
	voices []string
}

func startSpeech(t *testing.T, pcm []byte, refusal string) *fakeSpeech {
	t.Helper()
	f := &fakeSpeech{pcm: pcm, refusal: refusal}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Voice string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.voices = append(f.voices, body.Voice)
		f.mu.Unlock()
		if f.refusal != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, f.refusal)
			return
		}
		_, _ = w.Write(f.pcm)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/v1"
	return f
}

func (f *fakeSpeech) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.voices...)
}

// openAIProvider is an OpenAI provider named "speech" at url, signed in
// with a key.
func openAIProvider(t *testing.T, url string) *providers.Provider {
	t.Helper()
	p, err := providers.New(&config.Config{
		Providers:    map[string]config.Provider{"speech": {API: config.APIOpenAI, URL: url, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_SPEECH_KEY"}}},
		ProviderKeys: map[string]string{"EXAMPLE_SPEECH_KEY": "sk-1"},
	}, providers.Options{}).Get("speech")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEngineProviderSpeaksEachRequestsVoiceOnAnEngineOfItsOwn fails if the
// adapter does not speak a request that names no voice in tts.voice and one
// that names a voice in that voice, each on an engine built once for its
// voice, or does not collect the engine's PCM, with its format, the model as
// the provider names it and the voice, into the response.
func TestEngineProviderSpeaksEachRequestsVoiceOnAnEngineOfItsOwn(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 600)
	f := startSpeech(t, pcm, "")
	e, err := NewEngineProvider(openAIProvider(t, f.url), "gpt-4o-mini-tts", "cedar", 0)
	if err != nil {
		t.Fatalf("NewEngineProvider: %v", err)
	}
	if e.Name() != "speech" {
		t.Errorf("Name = %q, want the provider's name", e.Name())
	}
	for _, tc := range []struct{ asked, want string }{{"", "cedar"}, {"alloy", "alloy"}, {"alloy", "alloy"}, {"", "cedar"}} {
		resp, err := e.Synthesize(context.Background(), SpeechRequest{Text: "hello", Voice: tc.asked})
		if err != nil {
			t.Fatalf("Synthesize voice %q: %v", tc.asked, err)
		}
		if !bytes.Equal(resp.Audio, pcm) {
			t.Fatalf("voice %q: audio of %d bytes, want the engine's %d", tc.asked, len(resp.Audio), len(pcm))
		}
		if want := (audio.AudioFormat{SampleRate: 24000, Channels: 1, Encoding: audio.EncodingPCM16}); resp.Format != want {
			t.Errorf("voice %q: format %+v, want %+v", tc.asked, resp.Format, want)
		}
		if resp.Model != "gpt-4o-mini-tts" || resp.Voice != tc.want {
			t.Errorf("voice %q: response reports %q in %q, want gpt-4o-mini-tts in %q", tc.asked, resp.Model, resp.Voice, tc.want)
		}
		if resp.DurationMs != 50 {
			t.Errorf("voice %q: duration %d ms, want the 50 ms of 1200 samples at 24 kHz", tc.asked, resp.DurationMs)
		}
	}
	if got, want := f.asked(), []string{"cedar", "alloy", "alloy", "cedar"}; !slices.Equal(got, want) {
		t.Errorf("OpenAI was asked for voices %q, want %q", got, want)
	}
	e.mu.Lock()
	engines := len(e.engines)
	e.mu.Unlock()
	if engines != 2 {
		t.Errorf("the adapter holds %d engines, want one for each of the 2 voices", engines)
	}
	first, err := e.engine("alloy")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := e.engine("alloy"); again != first {
		t.Error("the adapter built a new engine for a voice it has one for")
	}
}

// TestEngineProviderReportsTheEnginesDefaultVoice fails if a request spoken
// with no voice named, and no tts.voice, reports no voice instead of the one
// the engine spoke in.
func TestEngineProviderReportsTheEnginesDefaultVoice(t *testing.T) {
	f := startSpeech(t, []byte{1, 0, 2, 0}, "")
	e, err := NewEngineProvider(openAIProvider(t, f.url), "gpt-4o-mini-tts", "", 0)
	if err != nil {
		t.Fatalf("NewEngineProvider: %v", err)
	}
	resp, err := e.Synthesize(context.Background(), SpeechRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if resp.Voice != "marin" || f.asked()[0] != "marin" {
		t.Fatalf("reported voice %q after asking for %q, want marin, OpenAI's default", resp.Voice, f.asked()[0])
	}
}

// TestEngineProviderRefusesTextLongerThanItsMaxLength fails if a text longer
// than tts.max_length reaches the provider.
func TestEngineProviderRefusesTextLongerThanItsMaxLength(t *testing.T) {
	f := startSpeech(t, []byte{1, 0}, "")
	e, err := NewEngineProvider(openAIProvider(t, f.url), "gpt-4o-mini-tts", "", 10)
	if err != nil {
		t.Fatalf("NewEngineProvider: %v", err)
	}
	_, err = e.Synthesize(context.Background(), SpeechRequest{Text: strings.Repeat("a", 11)})
	var tooLong *ErrTextTooLong
	if !errors.As(err, &tooLong) || tooLong.MaxLength != 10 {
		t.Fatalf("Synthesize of 11 characters = %v, want ErrTextTooLong of 10", err)
	}
	if n := len(f.asked()); n != 0 {
		t.Fatalf("the provider was asked %d times, want none", n)
	}
}

// TestNewEngineProviderFailsForAProviderWithNoEngine fails if the adapter
// can be built on a provider no engine speaks with: the unit would start
// with a POST /api/tts/speak that fails every request.
func TestNewEngineProviderFailsForAProviderWithNoEngine(t *testing.T) {
	p, err := providers.New(&config.Config{Providers: map[string]config.Provider{
		"claude": {API: config.APIAnthropic, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_CLAUDE_KEY"}},
	}}, providers.Options{}).Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngineProvider(p, "claude-x", "", 0); err == nil {
		t.Fatal("NewEngineProvider on the Anthropic API succeeded")
	}
}

// TestTheSpeakStatusReportsTheModelAndVoiceSpoken fails if a request spoken
// through the adapter does not report, by the time its synthesis is over,
// the model as the provider names it (not provider/model) and the voice it
// asked for: what POST /api/tts/speak returns in X-Ghost-Wispr-TTS-Model and
// X-Ghost-Wispr-TTS-Voice.
func TestTheSpeakStatusReportsTheModelAndVoiceSpoken(t *testing.T) {
	f := startSpeech(t, bytes.Repeat([]byte{1, 0}, 480), "")
	e, err := NewEngineProvider(openAIProvider(t, f.url), "gpt-4o-mini-tts", "cedar", 0)
	if err != nil {
		t.Fatalf("NewEngineProvider: %v", err)
	}
	o := NewOrchestrator(e, newTestSpeaker(t), OrchestratorOpts{MaxRequestsPerMin: 100})
	t.Cleanup(o.Stop)
	id, err := o.Speak("hello", "alloy")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := o.WaitForSynthesis(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status == StatusFailed || status.Model != "gpt-4o-mini-tts" || status.Voice != "alloy" {
		t.Fatalf("status %q (%q) reports %q in %q, want gpt-4o-mini-tts in alloy", status.Status, status.Error, status.Model, status.Voice)
	}
}

// TestARefusedSpeakFailsWithoutTheProvidersMessage fails if a refusal of the
// adapter's engine does not fail the speak request's status, which GET
// /api/tts/status returns, with the provider's status and code, or keeps
// the provider's message, which quotes the text to speak.
func TestARefusedSpeakFailsWithoutTheProvidersMessage(t *testing.T) {
	f := startSpeech(t, nil, `{"error":{"code":"text_rejected","message":"Rejected `+plantedWord+`"}}`)
	e, err := NewEngineProvider(openAIProvider(t, f.url), "gpt-4o-mini-tts", "", 0)
	if err != nil {
		t.Fatalf("NewEngineProvider: %v", err)
	}
	o := NewOrchestrator(e, newTestSpeaker(t), OrchestratorOpts{MaxRequestsPerMin: 100})
	t.Cleanup(o.Stop)
	id, err := o.Speak("We agreed on "+plantedWord+" today.", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := o.WaitForSynthesis(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusFailed || !strings.Contains(status.Error, "HTTP 400 (text_rejected)") || strings.Contains(status.Error, plantedWord) {
		t.Fatalf("status %q with error %q, want failed with OpenAI's 400 and code, and nothing of its message", status.Status, status.Error)
	}
}

// googleToken is a Google token source that always gives one token.
type googleToken struct{}

func (googleToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "token-1", Type: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

// speechProvider is p, named "speech", signed in as a unit signs in: with
// the key sk-1 from its auth.env, or with a Google token.
func speechProvider(t *testing.T, p *config.Provider) *providers.Provider {
	t.Helper()
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: googleToken{}})
	got, err := providers.New(&config.Config{
		Providers:    map[string]config.Provider{"speech": *p},
		ProviderKeys: map[string]string{"EXAMPLE_SPEECH_KEY": "sk-1"},
	}, providers.Options{
		GoogleCredentials: func(context.Context, string, ...string) (*auth.Credentials, error) { return creds, nil },
	}).Get("speech")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// speechKey signs in with the key sk-1.
var speechKey = config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_SPEECH_KEY"}

// spokenStatus speaks text in voice through p and returns the request's
// status once its synthesis is over: what POST /api/tts/speak returns, the
// model in X-Ghost-Wispr-TTS-Model and the voice in X-Ghost-Wispr-TTS-Voice.
func spokenStatus(t *testing.T, p Provider, text, voice string) *SpeakStatus {
	t.Helper()
	o := NewOrchestrator(p, newTestSpeaker(t), OrchestratorOpts{MaxRequestsPerMin: 100})
	t.Cleanup(o.Stop)
	id, err := o.Speak(text, voice)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := o.WaitForSynthesis(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return &status
}

// startGemini is Gemini TTS's streamGenerateContent, on Vertex AI or the
// Gemini API, as a test sees it: it streams pcm in two events, each naming
// modelVersion ("" names none), as each event of Vertex AI's stream does.
func startGemini(t *testing.T, pcm []byte, modelVersion string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range [][]byte{pcm[:len(pcm)/2], pcm[len(pcm)/2:]} {
			inline := map[string]any{"mimeType": "audio/L16;codec=pcm;rate=24000", "data": part}
			event := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"inlineData": inline}}}}}}
			if modelVersion != "" {
				event["modelVersion"] = modelVersion
			}
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\r\n\r\n", data)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestTheSpeakStatusReportsTheModelGeminisAnswerNames fails if a request
// spoken with Gemini TTS, on Vertex AI or the Gemini API, does not report
// the model the answer names in modelVersion, which can differ from the tts
// role's model, instead of the role's model: "not reported by provider" when
// the answer names none, as deploy/pi/accept-slice2.sh reads it, and
// "unknown" when it names one that is no plain token.
func TestTheSpeakStatusReportsTheModelGeminisAnswerNames(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 0}, 480)
	for _, tc := range []struct{ api, modelVersion, want string }{
		{config.APIVertex, "gemini-3.8-flash-tts-001", "gemini-3.8-flash-tts-001"},
		{config.APIGemini, "gemini-3.8-flash-tts-001", "gemini-3.8-flash-tts-001"},
		{config.APIVertex, "", "not reported by provider"},
		{config.APIVertex, "the room said " + plantedWord, "unknown"},
	} {
		p := config.Provider{API: config.APIVertex, URL: startGemini(t, pcm, tc.modelVersion), Project: "example-project", Location: "us", Auth: config.ProviderAuth{Kind: config.AuthGoogle}}
		if tc.api == config.APIGemini {
			p = config.Provider{API: config.APIGemini, URL: p.URL, Auth: speechKey}
		}
		e, err := NewEngineProvider(speechProvider(t, &p), "gemini-3.8-flash-tts", "Kore", 0)
		if err != nil {
			t.Fatalf("%s: NewEngineProvider: %v", tc.api, err)
		}
		status := spokenStatus(t, e, "hello", "")
		if status.Status == StatusFailed || status.Model != tc.want || status.Voice != "Kore" {
			t.Errorf("%s answering modelVersion %q: status %q (%q) reports %q in %q, want %q in Kore", tc.api, tc.modelVersion, status.Status, status.Error, status.Model, status.Voice, tc.want)
		}
	}
}

// pcmWAV is a 24 kHz mono PCM16 WAV of pcm, as Cloud Text-to-Speech sends
// LINEAR16.
func pcmWAV(pcm []byte) []byte {
	wav := []byte("RIFF")
	wav = binary.LittleEndian.AppendUint32(wav, uint32(36+len(pcm)))
	wav = append(wav, "WAVEfmt "...)
	wav = binary.LittleEndian.AppendUint32(wav, 16)
	wav = binary.LittleEndian.AppendUint16(wav, 1) // PCM
	wav = binary.LittleEndian.AppendUint16(wav, 1) // mono
	wav = binary.LittleEndian.AppendUint32(wav, 24000)
	wav = binary.LittleEndian.AppendUint32(wav, 48000)
	wav = binary.LittleEndian.AppendUint16(wav, 2)
	wav = binary.LittleEndian.AppendUint16(wav, 16)
	wav = append(wav, "data"...)
	wav = binary.LittleEndian.AppendUint32(wav, uint32(len(pcm)))
	return append(wav, pcm...)
}

// startCloudTTS is Cloud Text-to-Speech's text:synthesize as a test sees it:
// it answers each request with pcm and keeps the voice each one named.
func startCloudTTS(t *testing.T, pcm []byte) (url string, asked func() []string) {
	t.Helper()
	var mu sync.Mutex
	var voices []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Voice struct{ Name string } }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		voices = append(voices, body.Voice.Name)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"audioContent":%q}`, base64.StdEncoding.EncodeToString(pcmWAV(pcm)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(voices)
	}
}

// startAura is Deepgram Aura-2's /v1/speak socket as a test sees it: on each
// Flush it sends pcm (no audio message when pcm is empty), then Flushed, and
// it keeps the model each socket asked for.
func startAura(t *testing.T, pcm []byte) (url string, asked func() []string) {
	t.Helper()
	var mu sync.Mutex
	var models []string
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		models = append(models, r.URL.Query().Get("model"))
		mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m struct{ Type string }
			_ = json.Unmarshal(data, &m)
			switch m.Type {
			case "Flush":
				if len(pcm) > 0 {
					_ = ws.WriteMessage(websocket.BinaryMessage, pcm)
				}
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"Flushed","sequence_id":0}`))
			case "Close":
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(models)
	}
}

// TestTheSpeakStatusReportsTheVoiceThatIsTheModel fails if a request that
// names a voice of Deepgram Aura or Cloud Text-to-Speech, each of whose
// voices is a model, does not report that voice as the model that spoke,
// instead of the tts role's model, or a request that names none does not
// report the role's model, as the provider was asked for each.
func TestTheSpeakStatusReportsTheVoiceThatIsTheModel(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 0}, 480)
	cloudURL, cloudAsked := startCloudTTS(t, pcm)
	auraURL, auraAsked := startAura(t, pcm)
	for _, tc := range []struct {
		api         string
		p           config.Provider
		model       string
		asked       func() []string
		voice, want string
	}{
		{"google-tts", config.Provider{API: config.APIGoogleTTS, URL: cloudURL, Auth: speechKey}, "en-US-Chirp3-HD-Charon", cloudAsked, "en-GB-Chirp3-HD-Kore", "en-GB-Chirp3-HD-Kore"},
		{"google-tts", config.Provider{API: config.APIGoogleTTS, URL: cloudURL, Auth: speechKey}, "en-US-Chirp3-HD-Charon", cloudAsked, "", "en-US-Chirp3-HD-Charon"},
		{"deepgram", config.Provider{API: config.APIDeepgram, URL: auraURL, Auth: speechKey}, "aura-2-harmonia-en", auraAsked, "aura-2-thalia-en", "aura-2-thalia-en"},
		{"deepgram", config.Provider{API: config.APIDeepgram, URL: auraURL, Auth: speechKey}, "aura-2-harmonia-en", auraAsked, "", "aura-2-harmonia-en"},
	} {
		e, err := NewEngineProvider(speechProvider(t, &tc.p), tc.model, "", 0)
		if err != nil {
			t.Fatalf("%s: NewEngineProvider: %v", tc.api, err)
		}
		status := spokenStatus(t, e, "hello", tc.voice)
		if status.Status == StatusFailed || status.Model != tc.want || status.Voice != tc.want {
			t.Errorf("%s asked for voice %q: status %q (%q) reports %q in %q, want %q in %q", tc.api, tc.voice, status.Status, status.Error, status.Model, status.Voice, tc.want, tc.want)
		}
		if asked := tc.asked(); len(asked) == 0 || asked[len(asked)-1] != tc.want {
			t.Errorf("%s asked for voice %q: the provider was asked for %q, want %q last", tc.api, tc.voice, asked, tc.want)
		}
	}
}

// TestEngineProviderRefusesSpeechWithNoWholeSample fails if the adapter
// returns as spoken a synthesis that gave no audio, as Deepgram Aura does
// when it answers a Flush with Flushed alone, or less than one whole sample,
// as GPT-6.1 Sol's review of 743fe02 found: POST /api/tts/speak would report
// speech that plays nothing. Each must fail the synthesis, and the speak
// request's status; one whole sample is speech (the control).
func TestEngineProviderRefusesSpeechWithNoWholeSample(t *testing.T) {
	for _, tc := range []struct {
		name string
		pcm  []byte
		ok   bool
	}{
		{"Flushed with no audio", nil, false},
		{"one byte, no whole sample", []byte{7}, false},
		{"control: one whole sample", []byte{7, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auraURL, _ := startAura(t, tc.pcm)
			e, err := NewEngineProvider(speechProvider(t, &config.Provider{API: config.APIDeepgram, URL: auraURL, Auth: speechKey}), "aura-2-harmonia-en", "", 0)
			if err != nil {
				t.Fatalf("NewEngineProvider: %v", err)
			}
			resp, err := e.Synthesize(context.Background(), SpeechRequest{Text: "hello"})
			if tc.ok {
				if err != nil || resp == nil || !bytes.Equal(resp.Audio, tc.pcm) {
					t.Fatalf("Synthesize = %+v, %v; want the whole sample spoken", resp, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Synthesize = %d bytes of audio, %d ms, no error; want a failure: no whole sample was spoken", len(resp.Audio), resp.DurationMs)
			}
			if !strings.Contains(err.Error(), "no audio") {
				t.Errorf("Synthesize error %q; want it to say the engine gave no audio", err)
			}
			if status := spokenStatus(t, e, "hello", ""); status.Status != StatusFailed {
				t.Errorf("the speak request's status = %q, want %q", status.Status, StatusFailed)
			}
		})
	}
}
