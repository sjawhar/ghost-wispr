package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// examplePi is a config in the office Pi's shape: a model gateway signed in by
// a Google ID token, Vertex AI in two locations by Google workload identity
// with billing labels, OpenAI by OpenAI workload identity and Deepgram by a
// key.
const examplePi = `
google_credentials_file: /etc/example/google-credentials.json
providers:
  gateway:
    api: openai
    url: https://gateway.example.test/v1
    auth: {kind: google_id_token, audience: example-gateway}
  vertex:
    api: vertex
    project: example-project
    location: global
    auth: {kind: google}
    labels: {owner: example-owner, stream: example-stream}
  vertex-us:
    api: vertex
    project: example-project
    location: us
    auth: {kind: google}
    labels: {owner: example-owner, stream: example-stream}
  openai:
    api: openai
    auth: {kind: openai_wif, identity_provider_id: idp-example, service_account_id: sa-example}
  deepgram:
    api: deepgram
    auth: {kind: api_key, env: GHOST_WISPR_DEEPGRAM_API_KEY}
transcription:
  model: deepgram/nova-3
batch_transcription:
  model: deepgram/nova-3
summarization:
  model: vertex/gemini-example
embedding:
  model: gateway/gemini-embedding-example
concierge:
  model: vertex/gemini-example-lite
tts:
  model: vertex-us/gemini-example-tts
  voice: Kore
voice:
  gptlive:
    model: openai/gpt-live-example
  dgagent:
    provider: deepgram
`

// TestProvidersAreNamedOnceInTheConfigFile fails if the providers the file
// names are not read as written, if the defaults are kept beside them, or if
// a role does not resolve to the provider and model its provider/model names.
func TestProvidersAreNamedOnceInTheConfigFile(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-example")
	cfg, warnings, err := Load(writeConfig(t, examplePi))
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(cfg.Providers)); !slices.Equal(got, []string{"deepgram", "gateway", "openai", "vertex", "vertex-us"}) {
		t.Fatalf("providers %q, want the five the file names and no default beside them", got)
	}
	want := Provider{API: APIOpenAI, URL: "https://gateway.example.test/v1", Auth: ProviderAuth{Kind: AuthGoogleIDToken, Audience: "example-gateway"}}
	if got := cfg.Providers["gateway"]; !reflect.DeepEqual(got, want) {
		t.Errorf("gateway = %+v, want %+v", got, want)
	}
	if got := cfg.Providers["openai"].Auth; got.Kind != AuthOpenAIWIF || got.IdentityProviderID != "idp-example" || got.ServiceAccountID != "sa-example" {
		t.Errorf("openai auth = %+v, want OpenAI workload identity with its two ids", got)
	}
	for _, c := range []struct {
		role                 Role
		ref, provider, model string
		api                  string
	}{
		{RoleSummarization, cfg.Summarization.Model, "vertex", "gemini-example", APIVertex},
		{RoleEmbedding, cfg.Embedding.Model, "gateway", "gemini-embedding-example", APIOpenAI},
		{RoleConcierge, cfg.Concierge.Model, "vertex", "gemini-example-lite", APIVertex},
		{RoleTTS, cfg.TTS.Model, "vertex-us", "gemini-example-tts", APIVertex},
		{RoleTranscription, cfg.Transcription.Model, "deepgram", "nova-3", APIDeepgram},
		{RoleBatchTranscription, cfg.BatchTranscription.Model, "deepgram", "nova-3", APIDeepgram},
		{RoleGPTLive, cfg.Voice.GPTLive.Model, "openai", "gpt-live-example", APIOpenAI},
	} {
		m, err := cfg.ResolveModel(c.role, c.ref)
		if err != nil || m.Provider != c.provider || m.Model != c.model || m.Config.API != c.api {
			t.Errorf("%s %q resolved to %+v, %v; want provider %s, model %s on the %s API", c.role.Key, c.ref, m, err, c.provider, c.model, c.api)
		}
	}
	if m, err := cfg.ResolveProvider(RoleVoiceAgent, cfg.Voice.DGAgent.Provider); err != nil || m.Provider != "deepgram" {
		t.Errorf("voice agent provider = %+v, %v; want deepgram", m, err)
	}
	if len(warnings) != 0 {
		t.Errorf("a whole config warned: %q", warnings)
	}
	alone, _, err := Load(writeConfig(t, "providers:\n  vertex: {api: vertex, project: example-project, auth: {kind: google}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(alone.Providers)); !slices.Equal(got, []string{"vertex"}) {
		t.Errorf("a file naming vertex alone has providers %q; want vertex and no default beside it", got)
	}
	if alone.Summarization.Model != "" || alone.Transcription.Model != "" {
		t.Errorf("a file with providers of its own kept the default roles %q and %q, which name providers it does not have", alone.Summarization.Model, alone.Transcription.Model)
	}
}

// TestAModelSplitsAtTheFirstSlash fails if a model name that holds slashes,
// as a gateway's openai/gpt-… does, loses part of the model to its provider.
func TestAModelSplitsAtTheFirstSlash(t *testing.T) {
	provider, model, err := ParseModelRef(" gateway/openai/gpt-example ")
	if err != nil || provider != "gateway" || model != "openai/gpt-example" {
		t.Fatalf("ParseModelRef = %q, %q, %v; want gateway, openai/gpt-example", provider, model, err)
	}
	for _, bad := range []string{"", "nova-3", "/nova-3", "deepgram/", "deep gram/nova-3"} {
		if _, _, err := ParseModelRef(bad); err == nil {
			t.Errorf("ParseModelRef(%q) took it; want a refusal", bad)
		}
	}
}

// TestARoleRefusesAProviderItCannotCall fails if a role resolves a provider the
// file does not name, or one whose API the role has no client for: the
// Concierge has no client for Deepgram's API. It fails too if the Concierge
// cannot run on the gateway's OpenAI API, which serves Claude, OpenAI and
// Gemini models.
func TestARoleRefusesAProviderItCannotCall(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load(writeConfig(t, examplePi))
	if err != nil {
		t.Fatal(err)
	}
	if m, err := cfg.ResolveModel(RoleConcierge, "gateway/claude-example"); err != nil || m.Provider != "gateway" || m.Model != "claude-example" || m.Config.API != APIOpenAI {
		t.Errorf("the Concierge on the gateway's OpenAI API resolved to %+v, %v; want the gateway and claude-example", m, err)
	}
	if _, err := cfg.ResolveModel(RoleConcierge, "deepgram/nova-3"); err == nil || !strings.Contains(err.Error(), "deepgram") {
		t.Errorf("the Concierge on Deepgram's API: err = %v, want a refusal naming the provider and its API", err)
	}
	if _, err := cfg.ResolveModel(RoleSummarization, "elsewhere/model"); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("a provider the file does not name: err = %v, want a refusal naming it", err)
	}
	if _, err := cfg.ResolveModel(RoleTranscription, "vertex/nova-3"); err == nil {
		t.Error("live transcription on Vertex AI was taken; only Deepgram's API transcribes live")
	}
}

// TestARoleThatCannotResolveWarns fails if a role whose provider/model names
// no provider, or a provider whose API the role cannot call, starts with no
// warning: the role is off, and the operator must learn why.
func TestARoleThatCannotResolveWarns(t *testing.T) {
	clearVoiceEnv(t)
	_, warnings, err := Load(writeConfig(t, strings.Replace(examplePi, "gateway/gemini-embedding-example", "nowhere/gemini-embedding-example", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "embedding.model") && strings.Contains(w, "nowhere") }) {
		t.Errorf("warnings %q name no unknown embedding provider", warnings)
	}
}

// TestNoProvidersInTheFileMeansTheDefaultProviders fails if a config that names
// no provider has none, so that a Deepgram key alone no longer transcribes, or
// if the defaults ask for any key but Deepgram's and OpenAI's.
func TestNoProvidersInTheFileMeansTheDefaultProviders(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-example")
	cfg, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Provider{
		"deepgram": {API: APIDeepgram, Auth: ProviderAuth{Kind: AuthAPIKey, Env: EnvPrefix + "DEEPGRAM_API_KEY"}},
		"openai":   {API: APIOpenAI, Auth: ProviderAuth{Kind: AuthAPIKey, Env: EnvPrefix + "OPENAI_API_KEY"}},
	}
	if !reflect.DeepEqual(cfg.Providers, want) {
		t.Fatalf("default providers %+v, want %+v", cfg.Providers, want)
	}
	m, err := cfg.ResolveModel(RoleTranscription, cfg.Transcription.Model)
	if err != nil || cfg.APIKey(m.Provider) != "dg-example" {
		t.Errorf("live transcription resolved %+v, %v with key %q; want Deepgram with the environment's key", m, err, cfg.APIKey(m.Provider))
	}
}

// TestAProviderKeyComesFromTheVariableItsAuthNames fails if a provider that
// signs in with a key does not read it from the variable its auth.env names,
// reads one from the YAML file, or reads a key for a provider that signs in
// another way.
func TestAProviderKeyComesFromTheVariableItsAuthNames(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv("EXAMPLE_GATEWAY_KEY", "gw-example")
	cfg, _, err := Load(writeConfig(t, `
providers:
  keyed:
    api: anthropic
    url: https://anthropic.example.test
    auth: {kind: api_key, env: EXAMPLE_GATEWAY_KEY}
    key: from-the-file
  vertex:
    api: vertex
    project: example-project
    auth: {kind: google}
summarization:
  model: keyed/claude-example
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.APIKey("keyed"); got != "gw-example" {
		t.Errorf("APIKey(keyed) = %q, want the environment's", got)
	}
	if got := cfg.APIKey("vertex"); got != "" {
		t.Errorf("APIKey(vertex) = %q, want none for Google sign-in", got)
	}
}

// TestAProviderConfigThatCannotSignInWarns fails if a provider with no API, an
// unknown API or sign-in, a sign-in its API cannot use, or a missing part of
// its sign-in starts without a warning that names the provider.
func TestAProviderConfigThatCannotSignInWarns(t *testing.T) {
	cases := map[string]string{
		"no api":              "{auth: {kind: api_key, env: EXAMPLE_KEY}}",
		"unknown api":         "{api: telepathy, auth: {kind: api_key, env: EXAMPLE_KEY}}",
		"no sign-in":          "{api: openai}",
		"unknown sign-in":     "{api: openai, auth: {kind: handshake}}",
		"key with no env":     "{api: openai, auth: {kind: api_key}}",
		"key env not a name":  "{api: openai, auth: {kind: api_key, env: 'not a name'}}",
		"vertex no project":   "{api: vertex, auth: {kind: google}}",
		"id token no url":     "{api: openai, auth: {kind: google_id_token, audience: example-gateway}}",
		"id token no aud":     "{api: openai, url: https://gateway.example.test/v1, auth: {kind: google_id_token}}",
		"openai wif no ids":   "{api: openai, auth: {kind: openai_wif}}",
		"openai wif on other": "{api: anthropic, auth: {kind: openai_wif, identity_provider_id: i, service_account_id: s}}",
		"google on openai":    "{api: openai, auth: {kind: google}}",
		"url not absolute":    "{api: openai, url: gateway.example.test, auth: {kind: api_key, env: EXAMPLE_KEY}}",
	}
	for name, provider := range cases {
		t.Run(name, func(t *testing.T) {
			clearVoiceEnv(t)
			_, warnings, err := Load(writeConfig(t, "providers:\n  broken: "+provider+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, `providers.broken`) }) {
				t.Errorf("warnings %q name no providers.broken", warnings)
			}
		})
	}
	t.Run("a whole provider", func(t *testing.T) {
		clearVoiceEnv(t)
		_, warnings, err := Load(writeConfig(t, "providers:\n  whole: {api: openai, auth: {kind: api_key, env: EXAMPLE_KEY}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "providers.") }) {
			t.Errorf("a whole provider warned: %q", warnings)
		}
	})
	t.Run("a name a model cannot hold", func(t *testing.T) {
		clearVoiceEnv(t)
		_, warnings, err := Load(writeConfig(t, "providers:\n  'a/b': {api: openai, auth: {kind: api_key, env: EXAMPLE_KEY}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, `"a/b"`) }) {
			t.Errorf("warnings %q name no provider a/b", warnings)
		}
	})
}

// TestRemovedSettingsWarn fails if a config file or environment that still
// sets a per-vendor setting this build removed starts without a warning that
// names the setting and what replaced it: the unit would quietly lose a role.
func TestRemovedSettingsWarn(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"GCP_PROJECT", "example-project")
	t.Setenv(EnvPrefix+"EMBEDDING_LOCATION", "global")
	_, warnings, err := Load(writeConfig(t, `
gcp_project: example-project
genai_backend: vertex
embedding_model: gemini/gemini-embedding-example
deepgram_model: nova-3
tts_provider: gemini
summarization:
  model: openai/gpt-example
  base_url: https://openai.example.test/v1
batch_transcription:
  provider: deepgram
voice:
  tts_engine: gemini
  tts_location: us
  gptlive:
    openai_identity_provider_id: idp-example
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"gcp_project", "genai_backend", "embedding_model", "deepgram_model", "tts_provider", "summarization.base_url",
		"batch_transcription.provider", "voice.tts_engine", "voice.tts_location", "voice.gptlive.openai_identity_provider_id",
		EnvPrefix + "GCP_PROJECT", EnvPrefix + "EMBEDDING_LOCATION",
	} {
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.HasPrefix(w, key+" has been removed") }) {
			t.Errorf("no warning says %s has been removed; warnings: %q", key, warnings)
		}
	}
	// A removed setting that turned a role on says the role is off.
	for key, off := range map[string]string{"embedding_model": "search is off until then", "tts_provider": "text-to-speech is off until then"} {
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.HasPrefix(w, key+" has been removed") && strings.Contains(w, off) }) {
			t.Errorf("the warning for %s does not say %q; warnings: %q", key, off, warnings)
		}
	}
}

// TestAKeyNoProviderNamesWarns fails if a model key left in the environment,
// such as the OpenAI key of a unit that now signs in to OpenAI by workload
// identity, starts with no warning: nothing reads it any more.
func TestAKeyNoProviderNamesWarns(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"OPENAI_API_KEY", "sk-example")
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-example")
	_, warnings, err := Load(writeConfig(t, examplePi))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.HasPrefix(w, EnvPrefix+"OPENAI_API_KEY is set") }) {
		t.Errorf("warnings %q do not say the OpenAI key is read by no provider", warnings)
	}
	if slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "DEEPGRAM_API_KEY is set") }) {
		t.Errorf("the Deepgram key, which deepgram's auth names, warned: %q", warnings)
	}
}

// TestAVariableOnlyTheEnvFileSetsWarnsAsInTheEnvironment fails if a key or a
// removed variable that only the .env file beside the config sets, such as
// the key of a provider since taken out of the config, starts with no
// warning, or with other warnings than the same variables in the environment
// get: a unit started by hand reads .env itself, and nothing reads them any
// more. No warning may hold a value. The Deepgram key, which deepgram's auth
// names, is the control that must not warn.
func TestAVariableOnlyTheEnvFileSetsWarnsAsInTheEnvironment(t *testing.T) {
	clearVoiceEnv(t)
	dir := t.TempDir()
	path, envPath := filepath.Join(dir, "ghost-wispr.yaml"), filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(examplePi), 0o644); err != nil {
		t.Fatal(err)
	}
	unread := map[string]string{EnvPrefix + "GEMINI_API_KEY": "gm-1", EnvPrefix + "EMBEDDING_LOCATION": "europe-example"}
	set := maps.Clone(unread)
	set[EnvPrefix+"DEEPGRAM_API_KEY"] = "dg-1"
	var lines strings.Builder
	for _, name := range slices.Sorted(maps.Keys(set)) {
		lines.WriteString(name + "=" + set[name] + "\n")
	}
	if err := os.WriteFile(envPath, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fromFile, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range set {
		t.Setenv(name, value)
	}
	_, fromEnvironment, err := NewStoreWithEnv(path, filepath.Join(dir, "absent.env"))
	if err != nil {
		t.Fatal(err)
	}
	for name := range unread {
		if !slices.ContainsFunc(fromFile, func(w string) bool { return strings.HasPrefix(w, name+" ") }) {
			t.Errorf("%s only in .env: no warning names it; warnings %q", name, fromFile)
		}
	}
	if !slices.Equal(fromFile, fromEnvironment) {
		t.Errorf("warnings with the variables only in .env:\n%q\nwant those with them in the environment:\n%q", fromFile, fromEnvironment)
	}
	for _, w := range fromFile {
		if strings.Contains(w, "DEEPGRAM_API_KEY") {
			t.Errorf("the Deepgram key, which deepgram's auth names, warned: %q", w)
		}
		for _, value := range set {
			if strings.Contains(w, value) {
				t.Errorf("a warning holds the value %q: %q", value, w)
			}
		}
	}
}

// TestRoleModelsComeFromTheEnvironment fails if a role's GHOST_WISPR_ variable
// does not override the file's provider/model.
func TestRoleModelsComeFromTheEnvironment(t *testing.T) {
	clearVoiceEnv(t)
	for key, value := range map[string]string{
		"TRANSCRIPTION_MODEL": "deepgram/nova-example", "BATCH_TRANSCRIPTION_MODEL": "deepgram/nova-batch",
		"SUMMARIZATION_MODEL": "gateway/openai/gpt-example", "EMBEDDING_MODEL": "vertex/gemini-embedding-example",
		"CONCIERGE_MODEL": "vertex/gemini-other", "TTS_MODEL": "deepgram/aura-example", "TTS_VOICE": "Puck",
	} {
		t.Setenv(EnvPrefix+key, value)
	}
	cfg, _, err := Load(writeConfig(t, examplePi))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{cfg.Transcription.Model, cfg.BatchTranscription.Model, cfg.Summarization.Model, cfg.Embedding.Model, cfg.Concierge.Model, cfg.TTS.Model, cfg.TTS.Voice}
	want := []string{"deepgram/nova-example", "deepgram/nova-batch", "gateway/openai/gpt-example", "vertex/gemini-embedding-example", "vertex/gemini-other", "deepgram/aura-example", "Puck"}
	if !slices.Equal(got, want) {
		t.Errorf("roles %q, want the environment's %q", got, want)
	}
}

// TestASettingsSaveKeepsTheProviders fails if a settings save drops a provider,
// its sign-in or its labels after a restart, or writes a key into the local
// settings file.
func TestASettingsSaveKeepsTheProviders(t *testing.T) {
	clearVoiceEnv(t)
	t.Setenv(EnvPrefix+"DEEPGRAM_API_KEY", "dg-secret-example")
	path := writeConfig(t, examplePi)
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.SilenceTimeout = "45s" }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(LocalSettingsPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dg-secret-example") {
		t.Fatalf("the local settings file holds the Deepgram key:\n%s", data)
	}
	reloaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Providers, store.Get().Providers) || len(reloaded.Providers) != 5 {
		t.Errorf("providers after a save %+v, want the five saved %+v", reloaded.Providers, store.Get().Providers)
	}
	for _, name := range []string{"vertex", "vertex-us"} {
		if got, want := reloaded.Providers[name].Labels, map[string]string{"owner": "example-owner", "stream": "example-stream"}; !maps.Equal(got, want) {
			t.Errorf("%s's labels after a save = %v, want %v", name, got, want)
		}
	}
}

// TestASettingsChangeRefusesAModelItsProviderCannotServe fails if a settings
// change may name a provider the file does not name, or one the role cannot
// call: the change would turn the role off.
func TestASettingsChangeRefusesAModelItsProviderCannotServe(t *testing.T) {
	clearVoiceEnv(t)
	store, _, err := NewStore(writeConfig(t, examplePi))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Summarization.Model = "nowhere/gemini-example" },
		func(c *Config) { c.TTS.Model = "nowhere/voice" },
		func(c *Config) { c.TTS.Model = "gateway" },
		func(c *Config) {
			c.Summarization.Presets = map[string]Preset{"default": {UserTemplate: "{{transcript}}", Model: "deepgram/nova-3"}}
		},
	} {
		if err := store.Update(change); err == nil {
			t.Errorf("a change to a model no provider serves was taken: %+v", store.Get())
		}
	}
	if err := store.Update(func(c *Config) { c.TTS.Model = "deepgram/aura-example" }); err != nil {
		t.Errorf("a text-to-speech model on Deepgram was refused: %v", err)
	}
}

// TestASavedProviderKeyGoesToTheEnvFile fails if a key saved for a provider
// that signs in with one is not written to .env under the variable its
// auth.env names, or is written into the release config or the local
// settings file.
func TestASavedProviderKeyGoesToTheEnvFile(t *testing.T) {
	clearVoiceEnv(t)
	dir := t.TempDir()
	path, envPath := filepath.Join(dir, "ghost-wispr.yaml"), filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(examplePi), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("GHOST_WISPR_DISPATCH_TOKEN=kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { _ = c.SetAPIKey("deepgram", "dg-saved") }); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(envPath)
	if !strings.Contains(string(env), "GHOST_WISPR_DEEPGRAM_API_KEY=dg-saved\n") || !strings.Contains(string(env), "GHOST_WISPR_DISPATCH_TOKEN=kept\n") {
		t.Errorf(".env after the save:\n%s\nwant the Deepgram key under its variable and the other entries kept", env)
	}
	for _, yamlPath := range []string{path, LocalSettingsPath(path)} {
		yamlData, err := os.ReadFile(yamlPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(yamlData), "dg-saved") {
			t.Errorf("the key reached %s:\n%s", yamlPath, yamlData)
		}
	}
	reopened, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := reopened.Get(); cfg.APIKey("deepgram") != "dg-saved" {
		t.Errorf("after a restart APIKey(deepgram) = %q, want the saved key from .env", cfg.APIKey("deepgram"))
	}
}

// TestAKeyIsRefusedForAProviderThatNamesNoVariable fails if a key saved for
// an api_key provider whose auth.env is empty or not a variable's name is
// taken: the save would answer as saved, yet the key would be held in memory
// only (no variable to write it under) or written to .env as a line no
// variable reads.
func TestAKeyIsRefusedForAProviderThatNamesNoVariable(t *testing.T) {
	clearVoiceEnv(t)
	dir := t.TempDir()
	path, envPath := filepath.Join(dir, "ghost-wispr.yaml"), filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(`
providers:
  noenv: {api: deepgram, auth: {kind: api_key}}
  badenv: {api: deepgram, auth: {kind: api_key, env: "NOT A NAME"}}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	const original = "EXAMPLE_SETTING=kept\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStoreWithEnv(path, envPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"noenv", "badenv"} {
		if err := store.TryUpdate(func(c *Config) error { return c.SetAPIKey(name, "dg-1") }); err == nil || !strings.Contains(err.Error(), "auth.env") {
			t.Errorf("SetAPIKey(%s) = %v, want a refusal naming auth.env", name, err)
		}
		if cfg := store.Get(); cfg.APIKey(name) != "" {
			t.Errorf("APIKey(%s) after the refused save = %q, want none", name, cfg.APIKey(name))
		}
	}
	if env, _ := os.ReadFile(envPath); string(env) != original {
		t.Errorf(".env after the refused saves:\n%s\nwant it unchanged", env)
	}
}

// TestTheVoiceAgentsProviderStandsAlone fails if voice.dgagent.provider,
// which names a provider alone, is read as provider/model: a name with a
// slash would be shown beside, and its missing key warned about for, a
// provider voice C never calls.
func TestTheVoiceAgentsProviderStandsAlone(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load(writeConfig(t, `
providers:
  deepgram: {api: deepgram, auth: {kind: api_key, env: EXAMPLE_DEEPGRAM_KEY}}
voice:
  engine: dgagent
  dgagent: {provider: deepgram/extra}
`))
	if err != nil {
		t.Fatal(err)
	}
	if roles := cfg.ProviderRoles()["deepgram"]; slices.Contains(roles, RoleVoiceAgent.Key) {
		t.Errorf("ProviderRoles[deepgram] = %v; voice.dgagent.provider names deepgram/extra, which no provider is", roles)
	}
	for _, w := range missingKeyWarnings(&cfg) {
		if strings.Contains(w, RoleVoiceAgent.Key) {
			t.Errorf("missing-key warning %q blames deepgram for voice C, which names deepgram/extra", w)
		}
	}
	cfg.Voice.DGAgent.Provider = "deepgram"
	if roles := cfg.ProviderRoles()["deepgram"]; !slices.Contains(roles, RoleVoiceAgent.Key) {
		t.Errorf("ProviderRoles[deepgram] = %v, want voice.dgagent.provider among them", roles)
	}
}

// TestEachProviderNamesTheRolesThatCallIt fails if the settings page would
// show a provider beside a role that does not call it, or miss one that does:
// a provider that only a summarization preset calls is shown beside Summaries.
func TestEachProviderNamesTheRolesThatCallIt(t *testing.T) {
	clearVoiceEnv(t)
	withPreset := strings.Replace(examplePi, "summarization:\n  model: vertex/gemini-example\n",
		"summarization:\n  model: vertex/gemini-example\n  presets:\n    brief: {model: openai/gpt-example}\n    plain: {}\n", 1)
	if withPreset == examplePi {
		t.Fatal("examplePi no longer has the summarization section this test adds a preset to")
	}
	cfg, _, err := Load(writeConfig(t, withPreset))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"deepgram":  {"transcription.model", "batch_transcription.model"},
		"vertex":    {"summarization.model", "concierge.model"},
		"openai":    {"summarization.model"},
		"gateway":   {"embedding.model"},
		"vertex-us": {"tts.model"},
	}
	if got := cfg.ProviderRoles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderRoles = %v, want %v", got, want)
	}
}

// TestEachProviderNamesTheModelsItServes fails if a provider's models in use
// miss one a role or a summarization preset calls on it, repeat one, or
// follow another order than the roles': a provider's check asks about them
// in that order.
func TestEachProviderNamesTheModelsItServes(t *testing.T) {
	clearVoiceEnv(t)
	withPreset := strings.Replace(examplePi, "summarization:\n  model: vertex/gemini-example\n",
		"summarization:\n  model: vertex/gemini-example\n  presets:\n    brief: {model: openai/gpt-example}\n    again: {model: vertex/gemini-example}\n", 1)
	if withPreset == examplePi {
		t.Fatal("examplePi no longer has the summarization section this test adds a preset to")
	}
	cfg, _, err := Load(writeConfig(t, withPreset))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"deepgram":  {"nova-3"},
		"vertex":    {"gemini-example", "gemini-example-lite"},
		"openai":    {"gpt-example"},
		"gateway":   {"gemini-embedding-example"},
		"vertex-us": {"gemini-example-tts"},
	}
	if got := cfg.ProviderModels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderModels = %v, want %v", got, want)
	}
}

// TestEachModelInUseNamesTheRoleThatCallsIt fails if a provider's models in
// use lose the role that calls each, which decides the call a provider's
// check makes of it (an embedding, a chat), or keep one model twice for one
// role.
func TestEachModelInUseNamesTheRoleThatCallsIt(t *testing.T) {
	clearVoiceEnv(t)
	withPreset := strings.Replace(examplePi, "summarization:\n  model: vertex/gemini-example\n",
		"summarization:\n  model: vertex/gemini-example\n  presets:\n    again: {model: vertex/gemini-example}\n", 1)
	cfg, _, err := Load(writeConfig(t, strings.Replace(withPreset, "concierge:\n  model: vertex/gemini-example-lite\n", "concierge:\n  model: gateway/claude-example\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for name, uses := range cfg.ProviderModelUses() {
		for _, u := range uses {
			got[name] = append(got[name], u.Role.Key+" "+u.Model)
		}
	}
	want := map[string][]string{
		"deepgram":  {"transcription.model nova-3", "batch_transcription.model nova-3"},
		"vertex":    {"summarization.model gemini-example"},
		"gateway":   {"embedding.model gemini-embedding-example", "concierge.model claude-example"},
		"vertex-us": {"tts.model gemini-example-tts"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderModelUses = %v, want %v", got, want)
	}
}
