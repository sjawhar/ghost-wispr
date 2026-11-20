package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/tts"
)

// TestTheSpeakAPIWarnsOfAProviderRefusalUntilASpeechWorks fails if a speak a
// provider refuses (Aura asked for the voice of another model; OpenAI's 400)
// does not put a tts warning on /api/status that names tts.model and the
// provider's status and code, and says what to check (the model and voice; a
// 401's sign-in); if the warning keeps a word of what the provider said; if a
// failure a retry can mend (a 503) warns; or if the warning stays after a
// speak that works.
func TestTheSpeakAPIWarnsOfAProviderRefusalUntilASpeechWorks(t *testing.T) {
	const (
		keyEnv  = "EXAMPLE_SPEECH_KEY"
		planted = "quokkaplantedroomword"
		check   = "; check that tts.model names a speech model and tts.voice one of its voices"
	)
	aura := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("dg-error", "No such model "+planted)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(aura.Close)
	var mu sync.Mutex
	answer := http.StatusOK
	speech := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		status := answer
		mu.Unlock()
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_value","message":"Cannot speak `+planted+`"}}`)
			return
		}
		_, _ = w.Write(make([]byte, 480))
	}))
	t.Cleanup(speech.Close)
	held := &heldConfig{cfg: config.Config{
		Providers: map[string]config.Provider{
			"dg":     {API: config.APIDeepgram, URL: aura.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
			"speech": {API: config.APIOpenAI, URL: speech.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		},
		ProviderKeys: map[string]string{keyEnv: "k-1"},
		TTS:          config.TTS{Model: "dg/aura-2-example-en", Voice: "Kore"},
	}}
	warnings := newWarningsRegistry()
	source := &speechSource{store: held, warnings: warnings}
	speak := func(step string, fails bool, want ...string) {
		t.Helper()
		_, err := source.Synthesize(t.Context(), tts.SpeechRequest{Text: "hello"})
		if (err != nil) != fails {
			t.Fatalf("%s: Synthesize = %v, want failed %v", step, err, fails)
		}
		got := statusWarnings(t, warnings)
		if !slices.Equal(got, want) {
			t.Errorf("%s: /api/status warnings %q, want %q", step, got, want)
		}
		if strings.Contains(strings.Join(got, "\n"), planted) {
			t.Errorf("%s: a warning quotes the provider: %q", step, got)
		}
	}

	speak("Aura asked for Kore", true, "Text-to-speech with dg/aura-2-example-en failed: dg answered HTTP 400"+check)
	held.change(func(c *config.Config) { c.TTS.Model, c.TTS.Voice = "speech/tts-a", "" })
	speak("a model that speaks", false)
	mu.Lock()
	answer = http.StatusBadRequest
	mu.Unlock()
	speak("OpenAI refuses", true, "Text-to-speech with speech/tts-a failed: speech answered HTTP 400 (invalid_value)"+check)
	mu.Lock()
	answer = http.StatusOK
	mu.Unlock()
	speak("OpenAI speaks again", false)
	mu.Lock()
	answer = http.StatusServiceUnavailable
	mu.Unlock()
	speak("OpenAI is unavailable", true)
	mu.Lock()
	answer = http.StatusUnauthorized
	mu.Unlock()
	speak("OpenAI refuses the key", true, "Text-to-speech with speech/tts-a failed: speech answered HTTP 401 (invalid_value); check its provider's sign-in under Model providers")

	// The speak API's provider for a test that holds no warnings.
	if _, err := (&speechSource{store: held}).Synthesize(t.Context(), tts.SpeechRequest{Text: "hello"}); err == nil {
		t.Error("a speak OpenAI answered 401 worked")
	}
}

// emptyAura is Deepgram Aura's /v1/speak socket that answers every Flush
// with Flushed and no audio before it, as the re-proof of 4e85fcf's fake did.
func emptyAura(t *testing.T) string {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"Flushed","sequence_id":0}`))
			case "Close":
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestTheSpeakAPIWarnsOfAnEmptySpeechUntilASpeechWorks fails if a speak
// whose provider returned no audio (Aura's Flushed with nothing before it;
// an OpenAI answer of no bytes, or of one byte, less than a sample), which
// POST /api/tts/speak answers 502, puts no tts warning on /api/status naming
// tts.model and that its provider returned no audio, as the re-proof of
// 4e85fcf found; or if the warning stays after a speak that works.
func TestTheSpeakAPIWarnsOfAnEmptySpeechUntilASpeechWorks(t *testing.T) {
	const (
		keyEnv = "EXAMPLE_SPEECH_KEY"
		check  = "; check that tts.model names a speech model and tts.voice one of its voices"
	)
	var mu sync.Mutex
	body := []byte{}
	speech := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		answer := slices.Clone(body)
		mu.Unlock()
		_, _ = w.Write(answer)
	}))
	t.Cleanup(speech.Close)
	answer := func(b []byte) {
		mu.Lock()
		body = b
		mu.Unlock()
	}
	held := &heldConfig{cfg: config.Config{
		Providers: map[string]config.Provider{
			"dg":     {API: config.APIDeepgram, URL: emptyAura(t), Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
			"speech": {API: config.APIOpenAI, URL: speech.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		},
		ProviderKeys: map[string]string{keyEnv: "k-1"},
		TTS:          config.TTS{Model: "dg/aura-2-example-en"},
	}}
	warnings := newWarningsRegistry()
	source := &speechSource{store: held, warnings: warnings}
	speak := func(step string, fails bool, want ...string) {
		t.Helper()
		_, err := source.Synthesize(t.Context(), tts.SpeechRequest{Text: "hello"})
		if (err != nil) != fails {
			t.Fatalf("%s: Synthesize = %v, want failed %v", step, err, fails)
		}
		if got := statusWarnings(t, warnings); !slices.Equal(got, want) {
			t.Errorf("%s: /api/status warnings %q, want %q", step, got, want)
		}
	}

	speak("Aura flushes with no audio", true, "Text-to-speech with dg/aura-2-example-en failed: dg returned no audio"+check)
	held.change(func(c *config.Config) { c.TTS.Model = "speech/tts-a" })
	answer(make([]byte, 480))
	speak("a model that speaks", false)
	answer(nil)
	speak("OpenAI answers no bytes", true, "Text-to-speech with speech/tts-a failed: speech returned no audio"+check)
	answer(make([]byte, 480))
	speak("OpenAI speaks again", false)
	answer([]byte{7})
	speak("OpenAI answers one byte", true, "Text-to-speech with speech/tts-a failed: speech returned no audio"+check)
}
