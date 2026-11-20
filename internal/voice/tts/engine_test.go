package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// The model each HTTP engine of httpEngines speaks with.
var httpModels = map[string]string{
	config.APIVertex:     "gemini-3.8-flash-tts",
	config.APIGemini:     "gemini-3.8-flash-tts",
	config.APIOpenAI:     "gpt-4o-mini-tts",
	config.APIElevenLabs: "eleven_multilingual_v2",
	config.APIGoogleTTS:  "en-GB-Chirp3-HD-Charon",
}

// httpProviders are a provider of each API whose engine speaks over HTTP, at
// url: Vertex AI and Cloud Text-to-Speech signed in with Google, the others
// with testKey. OpenAI's url ends in /v1, as its public base does.
func httpProviders(url string) map[string]*config.Provider {
	return map[string]*config.Provider{
		config.APIVertex:     vertexAt(url),
		config.APIGemini:     {API: config.APIGemini, URL: url, Auth: keyAuth()},
		config.APIOpenAI:     {API: config.APIOpenAI, URL: url + "/v1", Auth: keyAuth()},
		config.APIElevenLabs: {API: config.APIElevenLabs, URL: url, Auth: keyAuth()},
		config.APIGoogleTTS:  {API: config.APIGoogleTTS, URL: url, Auth: googleAuth()},
	}
}

// httpEngines are the engine of each provider of httpProviders, by API.
func httpEngines(t *testing.T, url string, opts ...Option) map[string]Engine {
	t.Helper()
	engines := map[string]Engine{}
	for api, p := range httpProviders(url) {
		e, err := NewEngine(testProvider(t, p, nil), httpModels[api], "", opts...)
		if err != nil {
			t.Fatalf("%s: NewEngine: %v", api, err)
		}
		engines[api] = e
	}
	return engines
}

// collect streams text through e and returns the PCM it emitted.
func collect(t *testing.T, e Engine, text string) []byte {
	t.Helper()
	var got []byte
	if err := e.Stream(context.Background(), text, func(pcm []byte) error {
		got = append(got, pcm...)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return got
}

// TestOpenAIEngineStreamsPCMAsItArrives fails if the engine of an OpenAI
// provider does not POST {url}/audio/speech with the model, the text as
// input, the voice ("marin" when none) and response_format pcm, signed in
// with the provider's key as a bearer token, or does not emit each part of
// the 24 kHz PCM as it arrives: the fake holds the rest of the answer until
// the first part is emitted.
func TestOpenAIEngineStreamsPCMAsItArrives(t *testing.T) {
	first, rest := ramp(0, 1200), ramp(30, 2400)
	emittedFirst := make(chan struct{})
	var gotPath, gotAuth string
	var gotBody map[string]any
	var heldBack atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(first)
		w.(http.Flusher).Flush()
		select {
		case <-emittedFirst:
		case <-time.After(2 * time.Second):
			heldBack.Store(true)
		}
		_, _ = w.Write(rest)
	}))
	t.Cleanup(srv.Close)
	e, err := NewEngine(testProvider(t, httpProviders(srv.URL)[config.APIOpenAI], nil), "gpt-4o-mini-tts", "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if e.Format() != mono24k {
		t.Fatalf("Format = %+v, want PCM16 mono at 24 kHz", e.Format())
	}
	var got []byte
	if err := e.Stream(context.Background(), "Twelve open issues.", func(pcm []byte) error {
		if len(got) == 0 {
			close(emittedFirst)
		}
		got = append(got, pcm...)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if heldBack.Load() {
		t.Fatal("the first PCM was emitted only after the answer ended, want it emitted as it arrives")
	}
	if want := append(bytes.Clone(first), rest...); !bytes.Equal(got, want) {
		t.Fatalf("emitted %d bytes, want the answer's %d in order", len(got), len(want))
	}
	if gotPath != "/v1/audio/speech" {
		t.Errorf("path = %s, want /v1/audio/speech", gotPath)
	}
	if gotAuth != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the provider's key as a bearer token", gotAuth)
	}
	for field, want := range map[string]string{"model": "gpt-4o-mini-tts", "input": "Twelve open issues.", "voice": "marin", "response_format": "pcm"} {
		if gotBody[field] != want {
			t.Errorf("body %s = %v, want %q (body %v)", field, gotBody[field], want, gotBody)
		}
	}
}

// TestElevenLabsEngineStreamsPCM fails if the engine of an ElevenLabs
// provider does not POST {url}/v1/text-to-speech/{voice}/stream with
// output_format pcm_24000 and the text and model_id, signed in with the
// provider's key in xi-api-key, or does not emit the PCM streamed back.
func TestElevenLabsEngineStreamsPCM(t *testing.T) {
	pcm := ramp(9, 2880)
	var gotPath, gotFormat, gotKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotFormat, gotKey = r.URL.Path, r.URL.Query().Get("output_format"), r.Header.Get("Xi-Api-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write(pcm[:1000])
		w.(http.Flusher).Flush()
		_, _ = w.Write(pcm[1000:])
	}))
	t.Cleanup(srv.Close)
	e, err := NewEngine(testProvider(t, httpProviders(srv.URL)[config.APIElevenLabs], nil), "eleven_flash_v2_5", "voice-1")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if got := collect(t, e, "Three are blocked."); !bytes.Equal(got, pcm) {
		t.Fatalf("emitted %d bytes, want the streamed %d", len(got), len(pcm))
	}
	if gotPath != "/v1/text-to-speech/voice-1/stream" || gotFormat != "pcm_24000" {
		t.Errorf("request %s?output_format=%s, want /v1/text-to-speech/voice-1/stream?output_format=pcm_24000", gotPath, gotFormat)
	}
	if gotKey != testKey {
		t.Errorf("xi-api-key = %q, want the provider's key", gotKey)
	}
	if gotBody["text"] != "Three are blocked." || gotBody["model_id"] != "eleven_flash_v2_5" {
		t.Errorf("body = %v, want the text and model_id eleven_flash_v2_5", gotBody)
	}
}

// wav is a 24 kHz mono PCM16 WAV of pcm, with a LIST chunk before its data
// as Cloud Text-to-Speech can send.
func wav(pcm []byte) []byte {
	var b bytes.Buffer
	chunk := func(id string, body []byte) {
		b.WriteString(id)
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
		b.Write(body)
		if len(body)%2 == 1 {
			b.WriteByte(0)
		}
	}
	format := make([]byte, 16)
	binary.LittleEndian.PutUint16(format[0:], 1)
	binary.LittleEndian.PutUint16(format[2:], 1)
	binary.LittleEndian.PutUint32(format[4:], 24000)
	binary.LittleEndian.PutUint32(format[8:], 48000)
	binary.LittleEndian.PutUint16(format[12:], 2)
	binary.LittleEndian.PutUint16(format[14:], 16)
	b.WriteString("WAVE")
	chunk("fmt ", format)
	chunk("LIST", []byte("INFOxyz"))
	chunk("data", pcm)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(b.Len()))...), b.Bytes()...)
}

// TestGoogleTTSEngineEmitsThePCMWithoutTheWAVHeader fails if the engine of a
// Cloud Text-to-Speech provider does not POST {url}/v1/text:synthesize for
// LINEAR16 at 24 kHz in the voice the model names, in the voice's language,
// signed in with the provider's Google token, or emits the WAV header Cloud
// Text-to-Speech puts before the PCM.
func TestGoogleTTSEngineEmitsThePCMWithoutTheWAVHeader(t *testing.T) {
	pcm := ramp(11, 2400)
	var gotPath, gotAuth string
	var gotBody struct {
		Input struct {
			Text string `json:"text"`
		} `json:"input"`
		Voice struct {
			LanguageCode string `json:"languageCode"`
			Name         string `json:"name"`
		} `json:"voice"`
		AudioConfig struct {
			AudioEncoding   string `json:"audioEncoding"`
			SampleRateHertz int    `json:"sampleRateHertz"`
		} `json:"audioConfig"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"audioContent":%q}`, base64.StdEncoding.EncodeToString(wav(pcm)))
	}))
	t.Cleanup(srv.Close)
	e := httpEngines(t, srv.URL)[config.APIGoogleTTS]
	if got := collect(t, e, "The build passed."); !bytes.Equal(got, pcm) {
		t.Fatalf("emitted %d bytes, want the WAV's %d bytes of PCM alone", len(got), len(pcm))
	}
	if gotPath != "/v1/text:synthesize" {
		t.Errorf("path = %s, want /v1/text:synthesize", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the provider's Google token", gotAuth)
	}
	if gotBody.Input.Text != "The build passed." || gotBody.Voice.Name != "en-GB-Chirp3-HD-Charon" || gotBody.Voice.LanguageCode != "en-GB" {
		t.Errorf("body = %+v, want the text in voice en-GB-Chirp3-HD-Charon, language en-GB", gotBody)
	}
	if gotBody.AudioConfig.AudioEncoding != "LINEAR16" || gotBody.AudioConfig.SampleRateHertz != 24000 {
		t.Errorf("audioConfig = %+v, want LINEAR16 at 24000 Hz", gotBody.AudioConfig)
	}
}

// TestGoogleTTSEngineSignsInWithItsKeyInXGoogApiKey fails if the engine of a
// Cloud Text-to-Speech provider that signs in with a key does not send the
// key in X-Goog-Api-Key, the header Google's APIs read a key from, or sends
// any credential as a bearer token, which Cloud Text-to-Speech refuses.
func TestGoogleTTSEngineSignsInWithItsKeyInXGoogApiKey(t *testing.T) {
	pcm := ramp(5, 960)
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAuth = r.Header.Get("X-Goog-Api-Key"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"audioContent":%q}`, base64.StdEncoding.EncodeToString(wav(pcm)))
	}))
	t.Cleanup(srv.Close)
	e, err := NewEngine(testProvider(t, &config.Provider{API: config.APIGoogleTTS, URL: srv.URL, Auth: keyAuth()}, nil), httpModels[config.APIGoogleTTS], "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if got := collect(t, e, "The build passed."); !bytes.Equal(got, pcm) {
		t.Fatalf("emitted %d bytes, want the WAV's %d bytes of PCM", len(got), len(pcm))
	}
	if gotKey != testKey || gotAuth != "" {
		t.Fatalf("X-Goog-Api-Key = %q and Authorization = %q, want the provider's key in X-Goog-Api-Key alone", gotKey, gotAuth)
	}
}

// TestGoogleTTSEngineFailsAnAudioContentThatIsNotBase64 fails if an answer
// whose audioContent is not base64 emits any audio, or fails without saying
// that its audioContent could not be decoded.
func TestGoogleTTSEngineFailsAnAudioContentThatIsNotBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"audioContent":"not-valid-base64!!!"}`)
	}))
	t.Cleanup(srv.Close)
	e, err := NewEngine(testProvider(t, httpProviders(srv.URL)[config.APIGoogleTTS], nil), httpModels[config.APIGoogleTTS], "")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	emitted := 0
	err = e.Stream(context.Background(), "hello", func(pcm []byte) error {
		emitted += len(pcm)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "decode audioContent") {
		t.Errorf("Stream = %v, want an error saying the audioContent could not be decoded", err)
	}
	if emitted != 0 {
		t.Errorf("emitted %d bytes of an answer whose audio could not be decoded, want none", emitted)
	}
}

// TestWavPCMRefusesAnythingButPCM16MonoAt24kHz fails if a Cloud
// Text-to-Speech clip in another encoding, channel count, rate or depth is
// emitted as if it were the engine's format, or one cut short is read past
// its end.
func TestWavPCMRefusesAnythingButPCM16MonoAt24kHz(t *testing.T) {
	pcm := ramp(0, 480)
	if got, err := wavPCM(wav(pcm)); err != nil || !bytes.Equal(got, pcm) {
		t.Fatalf("wavPCM of a PCM16 mono 24 kHz WAV = %d bytes, %v; want its %d bytes", len(got), err, len(pcm))
	}
	for name, offset := range map[string]struct {
		at    int
		value uint32
		size  int
	}{
		"float encoding": {at: 20, value: 3, size: 2},
		"stereo":         {at: 22, value: 2, size: 2},
		"16 kHz":         {at: 24, value: 16000, size: 4},
		"8 bits":         {at: 34, value: 8, size: 2},
		"cut short":      {at: 4, value: 1 << 20, size: 4},
	} {
		clip := wav(pcm)
		if offset.size == 2 {
			binary.LittleEndian.PutUint16(clip[offset.at:], uint16(offset.value))
		} else {
			binary.LittleEndian.PutUint32(clip[offset.at:], offset.value)
		}
		if got, err := wavPCM(clip); err == nil {
			t.Errorf("%s: wavPCM = %d bytes, want a refusal", name, len(got))
		}
	}
}

// TestNewEngineRefusesWhatItCannotSpeakWith fails if NewEngine builds an
// engine with no provider, with no model, or for an API that has no
// text-to-speech.
func TestNewEngineRefusesWhatItCannotSpeakWith(t *testing.T) {
	if _, err := NewEngine(nil, "gpt-4o-mini-tts", ""); err == nil {
		t.Error("NewEngine with no provider succeeded")
	}
	openai := testProvider(t, &config.Provider{API: config.APIOpenAI, Auth: keyAuth()}, nil)
	if _, err := NewEngine(openai, " ", ""); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("NewEngine with no model = %v, want an error naming the model", err)
	}
	anthropic := testProvider(t, &config.Provider{API: config.APIAnthropic, Auth: keyAuth()}, nil)
	if _, err := NewEngine(anthropic, "claude-x", ""); err == nil || !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("NewEngine for the Anthropic API = %v, want an error naming the API", err)
	}
}

// TestEveryEngineNamesTheVoiceItSpeaksIn fails if an engine NewEngine builds
// does not name, as POST /api/tts/speak reports it, the voice it was given,
// or its API's default voice when it was given none: Gemini's Kore, OpenAI's
// marin, ElevenLabs' Rachel, and for Aura and Cloud Text-to-Speech the voice
// the model names.
func TestEveryEngineNamesTheVoiceItSpeaksIn(t *testing.T) {
	ps := httpProviders("http://tts.example")
	ps[config.APIDeepgram] = deepgramAt("http://tts.example")
	models := map[string]string{config.APIDeepgram: "aura-2-harmonia-en"}
	for api, model := range httpModels {
		models[api] = model
	}
	defaults := map[string]string{
		config.APIVertex:     "Kore",
		config.APIGemini:     "Kore",
		config.APIOpenAI:     "marin",
		config.APIElevenLabs: "21m00Tcm4TlvDq8ikWAM",
		config.APIGoogleTTS:  "en-GB-Chirp3-HD-Charon",
		config.APIDeepgram:   "aura-2-harmonia-en",
	}
	for api, p := range ps {
		for _, voice := range []string{"", "voice-2"} {
			e, err := NewEngine(testProvider(t, p, nil), models[api], voice)
			if err != nil {
				t.Fatalf("%s: NewEngine: %v", api, err)
			}
			want := voice
			if want == "" {
				want = defaults[api]
			}
			if got := e.Voice(); got != want {
				t.Errorf("%s given voice %q: Voice = %q, want %q", api, voice, got, want)
			}
		}
	}
}

// TestEveryEngineNamesTheModelItAsksFor fails if an engine NewEngine builds
// does not name, as POST /api/tts/speak reports it when the answer names
// none, the model it asks its provider for: the model it was given, but for
// Aura and Cloud Text-to-Speech, each of whose voices is a model, the voice
// it was given, which replaces the model.
func TestEveryEngineNamesTheModelItAsksFor(t *testing.T) {
	ps := httpProviders("http://tts.example")
	ps[config.APIDeepgram] = deepgramAt("http://tts.example")
	models := map[string]string{config.APIDeepgram: "aura-2-harmonia-en"}
	for api, model := range httpModels {
		models[api] = model
	}
	voiceIsModel := map[string]bool{config.APIDeepgram: true, config.APIGoogleTTS: true}
	for api, p := range ps {
		for _, voice := range []string{"", "voice-2"} {
			e, err := NewEngine(testProvider(t, p, nil), models[api], voice)
			if err != nil {
				t.Fatalf("%s: NewEngine: %v", api, err)
			}
			want := models[api]
			if voiceIsModel[api] && voice != "" {
				want = voice
			}
			if got := e.Model(); got != want {
				t.Errorf("%s given model %q and voice %q: Model = %q, want %q", api, models[api], voice, got, want)
			}
		}
	}
}

// engineRefusal is a refusal of status 400 that names each provider's error
// code in its own field, and quotes the planted word in every message.
const engineRefusal = `{"error":{"code":"text_rejected","type":"invalid_request_error","status":"INVALID_ARGUMENT","message":"Rejected token ` + transportPlanted + `"},"detail":{"status":"invalid_text","message":"Rejected token ` + transportPlanted + `"}}`

// TestAnHTTPEngineRefusalKeepsOnlyItsStatusAndCode fails if a refused
// request of an HTTP engine does not fail with the HTTP status and the
// provider's error code, read from the field its provider names it in, or
// keeps any of the provider's message, which can quote the text to speak.
func TestAnHTTPEngineRefusalKeepsOnlyItsStatusAndCode(t *testing.T) {
	var quoted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, _ := io.ReadAll(r.Body); bytes.Contains(body, []byte(transportPlanted)) {
			quoted.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, engineRefusal)
	}))
	t.Cleanup(srv.Close)
	wantCode := map[string]string{
		config.APIVertex:     "INVALID_ARGUMENT",
		config.APIGemini:     "INVALID_ARGUMENT",
		config.APIGoogleTTS:  "INVALID_ARGUMENT",
		config.APIOpenAI:     "text_rejected",
		config.APIElevenLabs: "invalid_text",
	}
	engines := httpEngines(t, srv.URL)
	for api, e := range engines {
		err := e.Stream(context.Background(), "We agreed on "+transportPlanted+" today.", func([]byte) error { return nil })
		if err == nil {
			t.Fatalf("%s: no error from an answer of status 400", api)
		}
		if strings.Contains(err.Error(), transportPlanted) || strings.Contains(err.Error(), "Rejected") {
			t.Errorf("%s: the error quotes the provider's message: %v", api, err)
		}
		if want := "HTTP 400 (" + wantCode[api] + ")"; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: the error %q does not name %s", api, err, want)
		}
	}
	if n := int(quoted.Load()); n != len(engines) {
		t.Fatalf("the engines sent the planted word %d times, want %d, so the check read nothing", n, len(engines))
	}
}

// TestAnHTTPEngineRefusalDoesNotQuoteTheText fails if a refusal whose raw
// body, not JSON, quotes the text to speak reaches an HTTP engine's error.
func TestAnHTTPEngineRefusalDoesNotQuoteTheText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "bad request for: %s", body)
	}))
	t.Cleanup(srv.Close)
	for api, e := range httpEngines(t, srv.URL) {
		err := e.Stream(context.Background(), spokenText, func([]byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Fatalf("%s: Stream = %v, want the refusal's status", api, err)
		}
		if msg := strings.ToLower(err.Error()); strings.Contains(msg, "purple") || strings.Contains(msg, "elephant") {
			t.Errorf("%s: the error quotes the text to speak: %v", api, err)
		}
	}
}

// TestAnHTTPEngineFailsAnAnswerWithNoAudio fails if an answer of status 200
// that carries no audio reaches voice A as speech.
func TestAnHTTPEngineFailsAnAnswerWithNoAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":synthesize") {
			_, _ = io.WriteString(w, `{"audioContent":""}`)
		}
	}))
	t.Cleanup(srv.Close)
	for api, e := range httpEngines(t, srv.URL) {
		err := e.Stream(context.Background(), "hello", func([]byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "no audio") {
			t.Errorf("%s: Stream = %v, want an error saying the engine gave no audio", api, err)
		}
	}
}

// TestAnHTTPEngineAsksAgainOnceAfterA429 fails if an HTTP engine fails a
// text its provider refused once for its rate, instead of asking again after
// retryWait, or asks a third time.
func TestAnHTTPEngineAsksAgainOnceAfterA429(t *testing.T) {
	wait := retryWait
	retryWait = 10 * time.Millisecond
	t.Cleanup(func() { retryWait = wait })
	pcm := ramp(0, 960)
	for _, refusals := range []int{1, 2} {
		var asked atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if int(asked.Add(1)) <= refusals {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			switch {
			case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
				_, _ = io.WriteString(w, sseAudio(pcm))
			case strings.HasSuffix(r.URL.Path, ":synthesize"):
				_, _ = fmt.Fprintf(w, `{"audioContent":%q}`, base64.StdEncoding.EncodeToString(wav(pcm)))
			default:
				_, _ = w.Write(pcm)
			}
		}))
		for api, e := range httpEngines(t, srv.URL) {
			asked.Store(0)
			var got []byte
			err := e.Stream(context.Background(), "hello", func(b []byte) error {
				got = append(got, b...)
				return nil
			})
			switch refusals {
			case 1:
				if err != nil || !bytes.Equal(got, pcm) || asked.Load() != 2 {
					t.Errorf("%s after one 429: Stream = %v with %d bytes in %d requests, want the audio from the second", api, err, len(got), asked.Load())
				}
			case 2:
				if err == nil || !strings.Contains(err.Error(), "HTTP 429") || asked.Load() != 2 {
					t.Errorf("%s after two 429s: Stream = %v in %d requests, want the second 429 in 2", api, err, asked.Load())
				}
			}
		}
		srv.Close()
	}
}

// TestOpenAISlowPlaybackIsNoStall fails if the time an engine that streams
// raw PCM waits on the Output counts toward the stall timeout, or if such a
// stream that stops sending does not fail as stalled.
func TestOpenAISlowPlaybackIsNoStall(t *testing.T) {
	shortStall(t)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(ramp(0, 960))
		w.(http.Flusher).Flush()
		time.Sleep(stallTimeout / 2)
		_, _ = w.Write(ramp(5, 960))
		w.(http.Flusher).Flush()
		if r.Header.Get("X-Test-Stall") == "" {
			time.Sleep(3 * stallTimeout)
			return
		}
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) }) // first: it ends the stalled handler, which Close waits for
	e := httpEngines(t, srv.URL)[config.APIOpenAI]
	emitted := 0
	if err := e.Stream(context.Background(), "hello", func(pcm []byte) error {
		time.Sleep(3 * stallTimeout) // the Output holds 200 ms and plays on
		emitted += len(pcm)
		return nil
	}); err != nil {
		t.Fatalf("Stream held back by playback = %v, want nil", err)
	}
	if emitted != 2*960 {
		t.Fatalf("emitted %d bytes, want both parts' %d", emitted, 2*960)
	}

	stalled := httpEngines(t, srv.URL, WithHTTPClient(&http.Client{Transport: stallHeader{}}))[config.APIOpenAI]
	done := make(chan error, 1)
	go func() { done <- stalled.Stream(context.Background(), "hello", func([]byte) error { return nil }) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no stream data") {
			t.Fatalf("stalled Stream = %v, want an error saying no stream data came", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream still waiting 3 s on an answer that stopped sending")
	}
}

// stallHeader asks the fake of TestOpenAISlowPlaybackIsNoStall to stop
// sending after its second part.
type stallHeader struct{}

func (stallHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Stall", "1")
	return http.DefaultTransport.RoundTrip(r)
}
