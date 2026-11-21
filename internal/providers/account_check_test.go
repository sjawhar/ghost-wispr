package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestAnAccountReadProvesTheSignInNotTheModel fails if a provider whose check
// reads only its account (Deepgram's projects, the ElevenLabs user, Cloud
// Text-to-Speech's voices) reads Works when a role calls a model on it, as
// GPT-6.1 Sol's review of 743fe02 found: the read proves the credential and
// no model, so the provider is only Signed in, naming the model no call
// proves. It also fails if such a provider that no role calls does not read
// Works once the read accepts the credential (there is no model to prove), or
// if a refusal of the read does not fail the check.
func TestAnAccountReadProvesTheSignInNotTheModel(t *testing.T) {
	for _, tc := range []struct {
		api, path string
		roles     func(cfg *config.Config)
		model     string
	}{
		{config.APIDeepgram, "/v1/projects", func(cfg *config.Config) {
			cfg.Transcription.Model = "speech/nova-3"
		}, "nova-3"},
		{config.APIDeepgram, "/v1/projects", func(cfg *config.Config) {
			cfg.TTS.Model = "speech/aura-2-thalia-en"
		}, "aura-2-thalia-en"},
		{config.APIElevenLabs, "/v1/user", func(cfg *config.Config) {
			cfg.TTS.Model = "speech/eleven_flash_v2_5"
		}, "eleven_flash_v2_5"},
		{config.APIGoogleTTS, "/v1/voices?languageCode=en-US", func(cfg *config.Config) {
			cfg.TTS.Model = "speech/en-US-Chirp3-HD-Charon"
		}, "en-US-Chirp3-HD-Charon"},
	} {
		t.Run(tc.api+" "+tc.model, func(t *testing.T) {
			var refuse atomic.Bool
			var mu sync.Mutex
			var asked []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				asked = append(asked, r.Method+" "+r.URL.RequestURI())
				mu.Unlock()
				if refuse.Load() {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer srv.Close()
			checked := func(withRoles bool) Status {
				cfg := &config.Config{
					Providers:    map[string]config.Provider{"speech": {API: tc.api, URL: srv.URL, Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: "EXAMPLE_SPEECH_KEY"}}},
					ProviderKeys: map[string]string{"EXAMPLE_SPEECH_KEY": "k-1"},
				}
				if withRoles {
					tc.roles(cfg)
				}
				p, err := New(cfg, Options{}).Get("speech")
				if err != nil {
					t.Fatal(err)
				}
				return p.Check(context.Background())
			}

			s := checked(true)
			mu.Lock()
			requests := strings.Join(asked, "\n")
			mu.Unlock()
			if requests != "GET "+tc.path {
				t.Fatalf("the check asked %q; want only its account read, GET %s", requests, tc.path)
			}
			if !s.OK || s.Asked || !strings.Contains(s.Message, "makes no call of "+tc.model) {
				t.Errorf("a role calls %s on the provider and the check read only its account: %+v; want Signed in (OK, not asked), naming %s", tc.model, s, tc.model)
			}
			if s := checked(false); !s.OK || !s.Asked {
				t.Errorf("the control, no role calls the provider: %+v; want Works (OK and asked) once the read accepted the credential", s)
			}
			refuse.Store(true)
			if s := checked(true); s.OK || !s.Asked || !strings.Contains(s.Message, "HTTP 403") {
				t.Errorf("a refused account read: %+v; want Fails (not OK, asked), saying HTTP 403", s)
			}
		})
	}
}
