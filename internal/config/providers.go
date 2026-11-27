package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// The wire APIs a provider speaks: providers.<name>.api's values. A provider
// is one endpoint that speaks one of them, such as a model gateway that speaks
// the OpenAI API, or Vertex AI in one location.
const (
	APIOpenAI     = "openai"     // the OpenAI API: chat, embeddings, text-to-speech, Live
	APIAnthropic  = "anthropic"  // the Anthropic Messages API
	APIVertex     = "vertex"     // Google Vertex AI
	APIGemini     = "gemini"     // Google's Gemini API
	APIDeepgram   = "deepgram"   // Deepgram: speech-to-text, Aura text-to-speech, the Voice Agent
	APIElevenLabs = "elevenlabs" // ElevenLabs text-to-speech
	APIGoogleTTS  = "google-tts" // Google Cloud Text-to-Speech
)

// APIs are the wire APIs Ghost Wispr has a client for.
var APIs = []string{APIOpenAI, APIAnthropic, APIVertex, APIGemini, APIDeepgram, APIElevenLabs, APIGoogleTTS}

// How a provider signs in: providers.<name>.auth.kind's values. No kind puts
// a secret in the YAML file.
const (
	// AuthAPIKey sends a key read from the environment variable auth.env.
	AuthAPIKey = "api_key"
	// AuthGoogle sends a Google access token from google_credentials_file
	// (workload identity federation) or Application Default Credentials.
	AuthGoogle = "google"
	// AuthGoogleIDToken sends a Google ID token for auth.audience, minted from
	// google_credentials_file, as a model gateway that checks Google ID
	// tokens admits its callers.
	AuthGoogleIDToken = "google_id_token"
	// AuthOpenAIWIF sends the OpenAI access token OpenAI gives in exchange
	// for the unit's Google ID token (OpenAI's workload identity
	// federation), with the ids auth.identity_provider_id and
	// auth.service_account_id.
	AuthOpenAIWIF = "openai_wif"
)

// AuthKinds are the sign-in kinds, in the order the docs list them.
var AuthKinds = []string{AuthAPIKey, AuthGoogle, AuthGoogleIDToken, AuthOpenAIWIF}

// Provider is one entry of providers: an endpoint, the wire API it speaks, how
// Ghost Wispr signs in to it and, on Vertex AI, the labels its generate calls
// carry. It names no secret.
type Provider struct {
	API string `yaml:"api" json:"api"`
	// URL is the API's base URL; empty is the API's public address.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Project and Location are Vertex AI's: the Google Cloud project, and
	// the location ("global", "us", "us-central1", …); empty is global.
	Project  string       `yaml:"project,omitempty" json:"project,omitempty"`
	Location string       `yaml:"location,omitempty" json:"location,omitempty"`
	Auth     ProviderAuth `yaml:"auth" json:"auth"`
	// Labels are Vertex AI's: request labels, such as the owner and the
	// stream a deployment's spend is attributed to, that each generate call
	// (generateContent, streamGenerateContent) carries. Vertex AI copies
	// them into Cloud Billing for pay-as-you-go traffic.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// GenerateLabels are the labels a generate call on the provider carries:
// its labels on Vertex AI, and none on another API. Only Vertex AI bills by
// them; genai would send them to the Gemini API as well, and a Gemini API
// provider can be a model gateway's Google routes, which must get each
// request as before. Labels outside Google's rules are sent as written:
// Vertex AI answers a call that carries them, and validateProviders warns
// about them. The map is the provider's own: a caller only reads it.
func (p *Provider) GenerateLabels() map[string]string {
	if p.API != APIVertex || len(p.Labels) == 0 {
		return nil
	}
	return p.Labels
}

// maxLabels is the most labels Google's rules allow on one call.
const maxLabels = 64

// labelKey and labelValue are a key and a value Google's rules for labels
// allow: 1 to 63 (a value 0 to 63) lowercase letters, other letters (Unicode
// Lo, such as 名), digits, underscores or dashes, and a key starts with a
// letter.
var (
	labelKey   = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValue = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
)

// labelProblems says what of labels Google's rules do not allow, in key
// order.
func labelProblems(labels map[string]string) []string {
	var problems []string
	if len(labels) > maxLabels {
		problems = append(problems, fmt.Sprintf("has %d labels, more than %d", len(labels), maxLabels))
	}
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		if !labelKey.MatchString(key) {
			problems = append(problems, fmt.Sprintf("key %q is not 1 to 63 lowercase or international letters, digits, _ or - that starts with a letter", key))
		}
		if value := labels[key]; !labelValue.MatchString(value) {
			problems = append(problems, fmt.Sprintf("value %q of %q is not up to 63 lowercase or international letters, digits, _ or -", value, key))
		}
	}
	return problems
}

// ProviderAuth is how a provider signs in.
type ProviderAuth struct {
	Kind string `yaml:"kind" json:"kind"`
	// Env is the environment variable that holds an api_key provider's key.
	Env string `yaml:"env,omitempty" json:"env,omitempty"`
	// ReadOnly leaves an api_key provider's deployment-owned key out of
	// settings changes. Another kind holds no key, so there it does nothing,
	// and validateProviders warns.
	ReadOnly bool `yaml:"read_only,omitempty" json:"read_only,omitempty"`
	// Audience is a google_id_token provider's ID token audience.
	Audience string `yaml:"audience,omitempty" json:"audience,omitempty"`
	// IdentityProviderID and ServiceAccountID are an openai_wif provider's
	// OpenAI workload identity ids. They are not secrets.
	IdentityProviderID string `yaml:"identity_provider_id,omitempty" json:"identity_provider_id,omitempty"`
	ServiceAccountID   string `yaml:"service_account_id,omitempty" json:"service_account_id,omitempty"`
}

// DefaultVertexLocation is a Vertex AI provider's location when it names none.
const DefaultVertexLocation = "global"

// defaultProviders are the providers of a config file that names none: the
// Deepgram and OpenAI APIs, each with a key from the environment, so a
// Deepgram key alone transcribes, as before providers existed. A file that
// names providers has only its own.
func defaultProviders() map[string]Provider {
	return map[string]Provider{
		"deepgram": {API: APIDeepgram, Auth: ProviderAuth{Kind: AuthAPIKey, Env: EnvPrefix + "DEEPGRAM_API_KEY"}},
		"openai":   {API: APIOpenAI, Auth: ProviderAuth{Kind: AuthAPIKey, Env: EnvPrefix + "OPENAI_API_KEY"}},
	}
}

// namesProviders says whether a config file has a providers section.
func namesProviders(data []byte) bool {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return false
	}
	_, ok := values["providers"]
	return ok
}

// clearQuickstartModels drops the role defaults that name defaultProviders:
// a file with providers of its own names each role it uses.
func clearQuickstartModels(cfg *Config) {
	cfg.Summarization.Model = ""
	cfg.Transcription.Model = ""
	cfg.Voice.GPTLive.Model = ""
	cfg.Voice.DGAgent.Provider = ""
}

// Role is a use Ghost Wispr makes of a model, by the config key that names its
// provider/model, and the APIs it has a client for.
type Role struct {
	Key  string
	APIs []string
}

// The roles. A role's provider must speak one of its APIs.
var (
	RoleSummarization      = Role{"summarization.model", []string{APIOpenAI, APIAnthropic, APIVertex, APIGemini}}
	RoleConcierge          = Role{"concierge.model", []string{APIVertex, APIGemini, APIOpenAI}}
	RoleEmbedding          = Role{"embedding.model", []string{APIOpenAI, APIVertex, APIGemini}}
	RoleTTS                = Role{"tts.model", []string{APIVertex, APIGemini, APIOpenAI, APIDeepgram, APIElevenLabs, APIGoogleTTS}}
	RoleTranscription      = Role{"transcription.model", []string{APIDeepgram}}
	RoleBatchTranscription = Role{"batch_transcription.model", []string{APIDeepgram}}
	RoleGPTLive            = Role{"voice.gptlive.model", []string{APIOpenAI}}
	// RoleVoiceAgent names a provider alone (voice.dgagent.provider): its
	// think model is Deepgram's to call, by Deepgram's own names.
	RoleVoiceAgent = Role{"voice.dgagent.provider", []string{APIDeepgram}}
)

// ParseModelRef splits a role's provider/model at the first slash: the model
// may hold slashes, as a gateway's openai/gpt-… does.
func ParseModelRef(ref string) (provider, model string, err error) {
	ref = strings.TrimSpace(ref)
	provider, model, _ = strings.Cut(ref, "/")
	if provider == "" || model == "" || strings.ContainsFunc(ref, unicode.IsSpace) {
		return "", "", fmt.Errorf("%q is not provider/model, such as vertex/gemini-3.5-flash-lite", ref)
	}
	return provider, model, nil
}

// ResolvedModel is a role's provider/model resolved against providers.
type ResolvedModel struct {
	Provider string   // the provider's name
	Config   Provider // the provider's entry
	Model    string   // the model, as the provider names it; "" for a provider alone
}

// ResolveModel resolves a role's provider/model, or says why the role cannot
// call it: the name is not provider/model, no provider has that name, or the
// provider's API is not one the role has a client for. On an error the
// ResolvedModel is the zero value.
func (c *Config) ResolveModel(role Role, ref string) (ResolvedModel, error) {
	name, model, err := ParseModelRef(ref)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("%s: %w", role.Key, err)
	}
	resolved, err := c.ResolveProvider(role, name)
	if err != nil {
		return ResolvedModel{}, err
	}
	resolved.Model = model
	return resolved, nil
}

// ResolveProvider resolves a provider a role names alone. On an error the
// ResolvedModel is the zero value.
func (c *Config) ResolveProvider(role Role, name string) (ResolvedModel, error) {
	name = strings.TrimSpace(name)
	p, ok := c.Providers[name]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("%s names provider %q, which providers does not define", role.Key, name)
	}
	if !slices.Contains(role.APIs, p.API) {
		return ResolvedModel{}, fmt.Errorf("%s names provider %q, which speaks the %s API; %s needs one of: %s", role.Key, name, p.API, role.Key, strings.Join(role.APIs, ", "))
	}
	return ResolvedModel{Provider: name, Config: p}, nil
}

// APIKey returns the key of provider name when it signs in with one, from the
// environment variable its auth.env names (or .env, for a store with one);
// "" when it has none or signs in another way.
func (c *Config) APIKey(name string) string {
	if env, ok := c.keyEnv(name); ok {
		return c.ProviderKeys[env]
	}
	return ""
}

// SetAPIKey sets the key of provider name, which a store with a .env file
// writes there under the variable its auth.env names; a key it did not set is
// not written. It refuses a provider whose deployment owns its key, one that
// does not sign in with a key, or one that names no variable to hold one.
func (c *Config) SetAPIKey(name, key string) error {
	p, ok := c.Providers[name]
	if !ok {
		return fmt.Errorf("no provider %q", name)
	}
	if p.Auth.Kind != AuthAPIKey {
		return fmt.Errorf("provider %q signs in with %s, not a key", name, p.Auth.Kind)
	}
	if p.Auth.ReadOnly {
		return fmt.Errorf("provider %q has auth.read_only, so its deployment owns its key", name)
	}
	if !envName.MatchString(p.Auth.Env) {
		return fmt.Errorf("provider %q names no variable to hold its key — set its auth.env to one", name)
	}
	key = strings.TrimSpace(key)
	if strings.ContainsFunc(key, unicode.IsControl) {
		return fmt.Errorf("provider %q: a key cannot contain control characters", name)
	}
	if c.ProviderKeys == nil {
		c.ProviderKeys = map[string]string{}
	}
	c.ProviderKeys[p.Auth.Env] = key
	if c.keysSet == nil {
		c.keysSet = map[string]bool{}
	}
	c.keysSet[p.Auth.Env] = true
	return nil
}

// ProviderRoles names, for each provider, the roles in use that call it, by
// their config keys: what the settings page shows beside it.
func (c *Config) ProviderRoles() map[string][]string {
	roles := map[string][]string{}
	uses := rolesInUse(c)
	for i := range uses {
		u := &uses[i]
		m, err := u.resolve(c)
		if err == nil && !slices.Contains(roles[m.Provider], u.role.Key) {
			roles[m.Provider] = append(roles[m.Provider], u.role.Key)
		}
	}
	return roles
}

// ProviderModels names, for each provider, the models the roles in use call
// on it, each once, in the roles' order: what a provider's check can ask
// about. A role that names its provider alone (RoleVoiceAgent) adds none.
func (c *Config) ProviderModels() map[string][]string {
	models := map[string][]string{}
	for name, uses := range c.ProviderModelUses() {
		for _, u := range uses {
			if !slices.Contains(models[name], u.Model) {
				models[name] = append(models[name], u.Model)
			}
		}
	}
	return models
}

// ModelUse is a model a role in use calls on a provider, as the provider
// names it.
type ModelUse struct {
	Model string
	Role  Role
}

// ProviderModelUses names, for each provider, the models the roles in use
// call on it with the role that calls each, once a role and model, in the
// roles' order: the role says which call a provider's check makes of the
// model. A summarization preset's model is RoleSummarization's.
func (c *Config) ProviderModelUses() map[string][]ModelUse {
	models := map[string][]ModelUse{}
	uses := rolesInUse(c)
	for i := range uses {
		m, err := uses[i].resolve(c)
		if err != nil || m.Model == "" {
			continue
		}
		use := ModelUse{Model: m.Model, Role: uses[i].role}
		if !slices.ContainsFunc(models[m.Provider], func(u ModelUse) bool { return u.Model == use.Model && u.Role.Key == use.Role.Key }) {
			models[m.Provider] = append(models[m.Provider], use)
		}
	}
	return models
}

// LiveTranscriptionKey is the key of transcription.model's provider, or ""
// when the model resolves to no provider or its key is not set: live
// transcription runs only with one.
func (c *Config) LiveTranscriptionKey() string {
	m, err := c.ResolveModel(RoleTranscription, c.Transcription.Model)
	if err != nil {
		return ""
	}
	return c.APIKey(m.Provider)
}

// keyEnvNames are the variables the api_key providers read their keys from.
func (c *Config) keyEnvNames() []string {
	var names []string
	for name := range c.Providers {
		if env, ok := c.keyEnv(name); ok && !slices.Contains(names, env) {
			names = append(names, env)
		}
	}
	slices.Sort(names)
	return names
}

// keyEnv is the variable provider name reads its key from: its auth.env,
// when it signs in with a key and that is a variable's name (validateProviders
// warns about one that is not, and SetAPIKey refuses its key).
func (c *Config) keyEnv(name string) (string, bool) {
	auth := c.Providers[name].Auth
	return auth.Env, auth.Kind == AuthAPIKey && envName.MatchString(auth.Env)
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// providerNameOK is a name a model reference can hold before its slash.
func providerNameOK(name string) bool {
	return name != "" && !strings.ContainsRune(name, '/') && !strings.ContainsFunc(name, unicode.IsSpace)
}

// validateProviders warns about each provider that cannot sign in as written.
func validateProviders(providers map[string]Provider) []string {
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		p := providers[name]
		at := "providers." + name
		if !providerNameOK(name) {
			warnings = append(warnings, fmt.Sprintf("Provider %q cannot be named in provider/model — use a name with no slash or space.", name))
			continue
		}
		switch {
		case p.API == "":
			warnings = append(warnings, at+" names no api — set it to one of: "+strings.Join(APIs, ", ")+".")
			continue
		case !slices.Contains(APIs, p.API):
			warnings = append(warnings, fmt.Sprintf("%s.api %q is not an API this build calls — use one of: %s.", at, p.API, strings.Join(APIs, ", ")))
			continue
		}
		if p.URL != "" {
			if u, err := url.Parse(p.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				warnings = append(warnings, fmt.Sprintf("%s.url %q must be an absolute http or https URL.", at, p.URL))
			}
		}
		if p.API == APIVertex && strings.TrimSpace(p.Project) == "" {
			warnings = append(warnings, at+" is Vertex AI and names no project — set "+at+".project.")
		}
		if len(p.Labels) > 0 {
			if p.API != APIVertex {
				warnings = append(warnings, fmt.Sprintf("%s.labels: only Vertex AI takes request labels, and %s calls the %s API — its calls carry none; remove them.", at, at, p.API))
			} else if problems := labelProblems(p.Labels); len(problems) > 0 {
				warnings = append(warnings, fmt.Sprintf("%s.labels %s, which Google's rules for labels do not allow — its calls carry them as written and Vertex AI answers them, but Cloud Billing may not attribute spend by them; fix them.", at, strings.Join(problems, "; ")))
			}
		}
		a := p.Auth
		switch a.Kind {
		case "":
			warnings = append(warnings, at+" names no auth.kind — set it to one of: "+strings.Join(AuthKinds, ", ")+".")
		case AuthAPIKey:
			if !envName.MatchString(a.Env) {
				warnings = append(warnings, fmt.Sprintf("%s.auth.env %q is not an environment variable's name — name the variable that holds its key.", at, a.Env))
			}
		case AuthGoogle:
			if p.API != APIVertex && p.API != APIGoogleTTS {
				warnings = append(warnings, fmt.Sprintf("%s signs in with a Google access token, which the %s API does not take — use %s or %s.", at, p.API, AuthAPIKey, AuthGoogleIDToken))
			}
		case AuthGoogleIDToken:
			if p.URL == "" {
				warnings = append(warnings, at+" signs in with a Google ID token and names no url — set the url of the endpoint that admits it, such as a model gateway's.")
			}
			if strings.TrimSpace(a.Audience) == "" {
				warnings = append(warnings, at+" signs in with a Google ID token and names no auth.audience.")
			}
		case AuthOpenAIWIF:
			if p.API != APIOpenAI {
				warnings = append(warnings, fmt.Sprintf("%s signs in with OpenAI workload identity, which only the openai API takes.", at))
			}
			if strings.TrimSpace(a.IdentityProviderID) == "" || strings.TrimSpace(a.ServiceAccountID) == "" {
				warnings = append(warnings, at+" signs in with OpenAI workload identity — set auth.identity_provider_id and auth.service_account_id.")
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%s.auth.kind %q is not a sign-in this build knows — use one of: %s.", at, a.Kind, strings.Join(AuthKinds, ", ")))
		}
		if a.ReadOnly && a.Kind != "" && a.Kind != AuthAPIKey {
			warnings = append(warnings, fmt.Sprintf("%s.auth.read_only does nothing: only an api_key provider holds a key the settings could change, and %s signs in with %s — remove it.", at, at, a.Kind))
		}
	}
	return warnings
}

// removedSettings are the per-vendor settings that providers replaced, by
// their path in ghost-wispr.yaml, each with what replaced it. A start that
// finds one says so: the role it set would otherwise be quietly off.
var removedSettings = []struct{ path, instead string }{
	{"genai_backend", "name a provider with api: vertex (and its project and location) or api: gemini"},
	{"gcp_project", "set it as a Vertex AI provider's project"},
	{"gcp_location", "set it as a Vertex AI provider's location"},
	{"embedding_model", "set embedding.model to provider/model; search is off until then"},
	{"deepgram_model", "set transcription.model to provider/model"},
	{"tts_provider", "set tts.model to provider/model; text-to-speech is off until then"},
	{"tts_model", "set tts.model to provider/model; text-to-speech is off until then"},
	{"tts_voice", "set tts.voice"},
	{"tts_max_length", "set tts.max_length"},
	{"summarization.base_url", "set the url of the provider summarization.model names"},
	{"batch_transcription.provider", "set batch_transcription.model to provider/model"},
	{"voice.tts_engine", "set tts.model to provider/model"},
	{"voice.tts_model", "set tts.model to provider/model"},
	{"voice.tts_voice", "set tts.voice"},
	{"voice.tts_location", "set the location of the Vertex AI provider tts.model names"},
	{"voice.gptlive.openai_identity_provider_id", "set it in the auth of the provider voice.gptlive.model names, with kind openai_wif"},
	{"voice.gptlive.openai_service_account_id", "set it in the auth of the provider voice.gptlive.model names, with kind openai_wif"},
}

// removedEnvironment are the variables of removed settings and keys.
var removedEnvironment = []struct{ name, instead string }{
	{"GCP_PROJECT", "set it as a Vertex AI provider's project"},
	{"GCP_LOCATION", "set it as a Vertex AI provider's location"},
	{"GENAI_BACKEND", "name a provider with api: vertex or api: gemini"},
	{"EMBEDDING_BACKEND", "name the provider in embedding.model"},
	{"EMBEDDING_LOCATION", "set the location of the Vertex AI provider embedding.model names"},
	{"DEEPGRAM_MODEL", "set " + EnvPrefix + "TRANSCRIPTION_MODEL to provider/model"},
	{"BATCH_TRANSCRIPTION_PROVIDER", "set " + EnvPrefix + "BATCH_TRANSCRIPTION_MODEL to provider/model"},
	{"TTS_PROVIDER", "set " + EnvPrefix + "TTS_MODEL to provider/model"},
	{"TTS_API_KEY", "name the key's variable in the text-to-speech provider's auth.env"},
	{"VOICE_TTS_ENGINE", "set " + EnvPrefix + "TTS_MODEL to provider/model"},
	{"VOICE_TTS_MODEL", "set " + EnvPrefix + "TTS_MODEL to provider/model"},
	{"VOICE_TTS_VOICE", "set " + EnvPrefix + "TTS_VOICE"},
	{"VOICE_TTS_LOCATION", "set the location of the Vertex AI provider tts.model names"},
}

// removedSettingWarnings warns about each removed setting the file sets.
func removedSettingWarnings(data []byte) []string {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil
	}
	var warnings []string
	for _, s := range removedSettings {
		if hasPath(values, strings.Split(s.path, ".")) {
			warnings = append(warnings, s.path+" has been removed — "+s.instead+` (README, "Model providers").`)
		}
	}
	return warnings
}

func hasPath(values map[string]any, path []string) bool {
	v, ok := values[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		return true
	}
	next, ok := v.(map[string]any)
	return ok && hasPath(next, path[1:])
}

// removedEnvironmentWarnings warns about each removed variable, and each
// GHOST_WISPR_ key no provider reads, that the environment or the .env file
// (envFile) sets: a unit started by hand reads its keys from .env alone, and
// a key left there after its provider is gone does nothing either. A
// variable set in both warns once; no warning holds a value.
func removedEnvironmentWarnings(cfg *Config, envFile map[string]string) []string {
	set := map[string]bool{}
	for _, kv := range os.Environ() {
		if name, value, _ := strings.Cut(kv, "="); value != "" {
			set[name] = true
		}
	}
	for name, value := range envFile {
		if value != "" {
			set[name] = true
		}
	}
	var warnings []string
	for _, e := range removedEnvironment {
		if set[EnvPrefix+e.name] {
			warnings = append(warnings, EnvPrefix+e.name+" has been removed — "+e.instead+` (README, "Model providers").`)
		}
	}
	read := cfg.keyEnvNames()
	for name := range set {
		if !strings.HasPrefix(name, EnvPrefix) || !strings.HasSuffix(name, "_API_KEY") || slices.Contains(read, name) {
			continue
		}
		if slices.ContainsFunc(removedEnvironment, func(e struct{ name, instead string }) bool { return EnvPrefix+e.name == name }) {
			continue
		}
		warnings = append(warnings, name+" is set, but no provider's auth.env names it, so nothing reads it — name it in a provider's auth, or remove it.")
	}
	slices.Sort(warnings)
	return warnings
}

// validateRoles warns about each role whose provider/model cannot resolve.
func validateRoles(cfg *Config) []string {
	var warnings []string
	check := func(role Role, ref string) {
		if strings.TrimSpace(ref) == "" {
			return
		}
		if _, err := cfg.ResolveModel(role, ref); err != nil {
			warnings = append(warnings, "Invalid "+err.Error()+" — it is off.")
		}
	}
	check(RoleSummarization, cfg.Summarization.Model)
	for _, name := range slices.Sorted(maps.Keys(cfg.Summarization.Presets)) {
		check(Role{presetModelKey(name), RoleSummarization.APIs}, cfg.Summarization.Presets[name].Model)
	}
	check(RoleEmbedding, cfg.Embedding.Model)
	check(RoleConcierge, cfg.Concierge.Model)
	check(RoleTTS, cfg.TTS.Model)
	check(RoleTranscription, cfg.Transcription.Model)
	check(RoleBatchTranscription, cfg.BatchTranscription.Model)
	// Voices B and C are checked when the settings choose them.
	switch cfg.Voice.Engine {
	case VoiceEngineGPTLive:
		check(RoleGPTLive, cfg.Voice.GPTLive.Model)
	case VoiceEngineDGAgent:
		if _, err := cfg.ResolveProvider(RoleVoiceAgent, cfg.Voice.DGAgent.Provider); err != nil {
			warnings = append(warnings, "Invalid "+err.Error()+" — voice C is off.")
		}
	}
	return warnings
}

// roleUse is a role the config turns on, with its provider/model, or with
// its provider when the role names one alone (RoleVoiceAgent).
type roleUse struct {
	role  Role
	ref   string
	alone bool   // ref names a provider, not provider/model
	label string // what a warning calls the use, when not role.Key: a preset's model
}

// resolve resolves the use's provider/model, or its provider alone.
func (u *roleUse) resolve(c *Config) (ResolvedModel, error) {
	if u.alone {
		return c.ResolveProvider(u.role, u.ref)
	}
	return c.ResolveModel(u.role, u.ref)
}

// name is what a warning calls the use.
func (u *roleUse) name() string {
	if u.label != "" {
		return u.label
	}
	return u.role.Key
}

// presetModelKey is what a warning calls summarization preset name's model.
func presetModelKey(name string) string {
	return fmt.Sprintf("summarization preset %q model", name)
}

// rolesInUse are the roles the config turns on. A summarization preset's
// model is the summarization role's: the settings page shows its provider
// beside summaries, and a warning names the preset.
func rolesInUse(cfg *Config) []roleUse {
	uses := []roleUse{
		{role: RoleTranscription, ref: cfg.Transcription.Model},
		{role: RoleBatchTranscription, ref: cfg.BatchTranscription.Model},
		{role: RoleSummarization, ref: cfg.Summarization.Model},
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Summarization.Presets)) {
		uses = append(uses, roleUse{role: RoleSummarization, ref: cfg.Summarization.Presets[name].Model, label: presetModelKey(name)})
	}
	uses = append(uses,
		roleUse{role: RoleEmbedding, ref: cfg.Embedding.Model},
		roleUse{role: RoleConcierge, ref: cfg.Concierge.Model},
		roleUse{role: RoleTTS, ref: cfg.TTS.Model},
	)
	switch cfg.Voice.Engine {
	case VoiceEngineGPTLive:
		uses = append(uses, roleUse{role: RoleGPTLive, ref: cfg.Voice.GPTLive.Model})
	case VoiceEngineDGAgent:
		uses = append(uses, roleUse{role: RoleVoiceAgent, ref: cfg.Voice.DGAgent.Provider, alone: true})
	}
	return slices.DeleteFunc(uses, func(u roleUse) bool { return strings.TrimSpace(u.ref) == "" })
}

// missingKeyWarnings warns, once a provider, about each api_key provider a
// role in use names whose key is not set: those roles are off.
func missingKeyWarnings(cfg *Config) []string {
	off := map[string][]string{}
	uses := rolesInUse(cfg)
	for i := range uses {
		u := &uses[i]
		m, err := u.resolve(cfg)
		if err != nil || m.Config.Auth.Kind != AuthAPIKey || cfg.APIKey(m.Provider) != "" {
			continue
		}
		if !slices.Contains(off[m.Provider], u.name()) {
			off[m.Provider] = append(off[m.Provider], u.name())
		}
	}
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(off)) {
		env := cfg.Providers[name].Auth.Env
		warnings = append(warnings, fmt.Sprintf("Provider %q signs in with a key from %s, which is not set, so these are off: %s. Set %s.",
			name, env, strings.Join(off[name], ", "), env))
	}
	return warnings
}

// validateRoleChange refuses a settings change whose role models no provider
// serves: the fields the settings page and PATCH /api/config change.
func validateRoleChange(cfg *Config) error {
	if ref := cfg.Summarization.Model; ref != "" {
		if _, err := cfg.ResolveModel(RoleSummarization, ref); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Summarization.Presets)) {
		if ref := cfg.Summarization.Presets[name].Model; ref != "" {
			if _, err := cfg.ResolveModel(RoleSummarization, ref); err != nil {
				return fmt.Errorf("preset %q: %w", name, err)
			}
		}
	}
	if ref := cfg.TTS.Model; ref != "" {
		if _, err := cfg.ResolveModel(RoleTTS, ref); err != nil {
			return err
		}
	}
	return nil
}
