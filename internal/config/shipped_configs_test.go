package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// piConfigPath is the office Pi's config, deploy/pi/files/ghost-wispr.yaml.
var piConfigPath = filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml")

// TestTheShippedConfigsLoadWithNoWarning fails if the office Pi's config or
// the example config, with the keys their providers name set, starts with a
// warning: a removed setting left in, a role that names no provider, or a
// provider that cannot sign in. Either file would then ship a unit with a
// role quietly off.
func TestTheShippedConfigsLoadWithNoWarning(t *testing.T) {
	for _, path := range []string{
		piConfigPath,
		filepath.Join("..", "..", "ghost-wispr.yaml.example"),
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			clearVoiceEnv(t)
			t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-1")
			t.Setenv(EnvPrefix+"OPENAI_API_KEY", "sk-1")
			cfg, warnings, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			// The OpenAI key is set for the example; the Pi signs in to
			// OpenAI by workload identity and reads none.
			var kept []string
			for _, w := range warnings {
				if cfg.Providers["openai"].Auth.Kind != AuthAPIKey && w == EnvPrefix+"OPENAI_API_KEY is set, but no provider's auth.env names it, so nothing reads it — name it in a provider's auth, or remove it." {
					continue
				}
				kept = append(kept, w)
			}
			if len(kept) > 0 {
				t.Fatalf("%s warned:\n%q", path, kept)
			}
			if len(cfg.Providers) == 0 || cfg.Summarization.Model == "" || cfg.Transcription.Model == "" {
				t.Fatalf("%s loaded no providers or roles: %+v", path, cfg.Providers)
			}
		})
	}
}

// TestThePiConfigSearchesOnVertexAIUntilTheGatewayAdmitsThePi fails if the
// office Pi's config embeds for search with anything but gemini-embedding-2 on
// Vertex AI at global, signed in by the Pi's Google identity: the model and
// location of the vectors the Pi already holds, which the indexer labels
// gemini-embedding-2 whichever provider serves the model, so the deploy embeds
// no session again. It fails too if a provider signs in with a Google ID
// token, as only the model gateway does: the gateway refuses the Pi until its
// admission applies (docs/pi-setup.md, "The model gateway"), and its row on the
// settings page would read Fails.
func TestThePiConfigSearchesOnVertexAIUntilTheGatewayAdmitsThePi(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load(piConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := cfg.ResolveModel(RoleEmbedding, cfg.Embedding.Model)
	if err != nil || m.Model != "gemini-embedding-2" || m.Config.API != APIVertex || m.Config.Location != "global" || m.Config.Auth.Kind != AuthGoogle {
		t.Errorf("search embeds with %q: %+v, %v; want gemini-embedding-2 on Vertex AI at global by Google sign-in", cfg.Embedding.Model, m, err)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		if cfg.Providers[name].Auth.Kind == AuthGoogleIDToken {
			t.Errorf("provider %s signs in with a Google ID token, as the model gateway does, which refuses the Pi until its admission applies", name)
		}
	}
}

// TestThePiConfigTurnsEnvoyOnAtTheHub fails if the office Pi's config does not
// publish to the Envoy hub it names (envoy_enabled, envoy_hub_url), or does not
// have the Concierge ask an issue's agent through that hub's listener
// (concierge.envoy_url). With either off, a provision would turn it off on the
// Pi. deploy/pi/provision_models_test.sh checks that envoy_hub_url is the hub
// pi.env's PI_NATS_SERVER names.
func TestThePiConfigTurnsEnvoyOnAtTheHub(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load(piConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EnvoyEnabled {
		t.Error("the Pi's config does not publish to Envoy (envoy_enabled: false); want it on")
	}
	if cfg.EnvoyHubURL == "" {
		t.Error("the Pi's config names no envoy_hub_url; want the hub's listener")
	}
	if cfg.Concierge.EnvoyURL != cfg.EnvoyHubURL {
		t.Errorf("the Pi's Concierge asks agents through %q; want the hub's listener, envoy_hub_url %q", cfg.Concierge.EnvoyURL, cfg.EnvoyHubURL)
	}
}

// TestThePiConfigsGatewaySwitchLoads fails if the switch the office Pi's
// config names for search once the gateway admits the Pi, its commented-out
// gateway entry uncommented and embedding.model set to
// gateway/gemini-embedding-2, does not load with no warning, or does not send
// search to that gateway's gemini-embedding-2 by a Google ID token for its
// audience, at an https address under /v1.
func TestThePiConfigsGatewaySwitchLoads(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-1")
	raw, err := os.ReadFile(piConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.Index(lines, "  # gateway:")
	if start < 0 {
		t.Fatalf("%s holds no commented-out gateway entry (a line %q)", piConfigPath, "  # gateway:")
	}
	end := start + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "  #   ") {
		end++
	}
	for i := start; i < end; i++ {
		lines[i] = "  " + strings.TrimPrefix(lines[i], "  # ")
	}
	switched := strings.Join(lines, "\n")
	const onVertex, onGateway = "\n  model: vertex/gemini-embedding-2\n", "\n  model: gateway/gemini-embedding-2\n"
	if n := strings.Count(switched, onVertex); n != 1 {
		t.Fatalf("%s sets %q %d times, want once, under embedding", piConfigPath, strings.TrimSpace(onVertex), n)
	}
	switched = strings.Replace(switched, onVertex, onGateway, 1)
	cfg, warnings, err := Load(writeConfig(t, switched))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) > 0 {
		t.Errorf("the switched config warned: %q", warnings)
	}
	m, err := cfg.ResolveModel(RoleEmbedding, cfg.Embedding.Model)
	if err != nil || m.Provider != "gateway" || m.Model != "gemini-embedding-2" || m.Config.API != APIOpenAI ||
		m.Config.Auth.Kind != AuthGoogleIDToken || m.Config.Auth.Audience == "" ||
		!strings.HasPrefix(m.Config.URL, "https://") || !strings.HasSuffix(m.Config.URL, "/v1") {
		t.Errorf("after the switch search embeds with %q: %+v, %v; want the gateway's gemini-embedding-2 by a Google ID token, at https …/v1", cfg.Embedding.Model, m, err)
	}
}
