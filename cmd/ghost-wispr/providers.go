package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"weak"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// providerSets holds the providers.Set of the providers saved last. A
// settings save that changes only keys keeps the Set and gives it the new
// keys (providers.Set.SetKeys), so every client built from it before the
// save, such as the search indexer built at start, sends the new key. One
// that changes a provider, the credential file, a role's model, or which
// role calls a model, builds a new Set, whose checks call that model as that
// role calls it (providers.New takes cfg.ProviderModelUses). A client built
// from an earlier Set still signs in with it, so each Set built before gets
// the keys saved after it too, for as long as a client holds it.
//
// Once it follows the unit's config store (follow), every Set takes its
// keys from the config the store holds when of runs, read under of's own
// lock, whatever config its caller passes: a caller's snapshot can be older
// than a save already applied (the settings page's check, a summary's
// client, a speech, each reads the store before it reaches of), and its keys
// would give the clients built before the save the key the save replaced.
// The store's config only moves forward, and of reads it one call at a
// time, so the keys every Set holds never go back. The caller's config
// still says which providers, models and credential file the Set is of.
type providerSets struct {
	mu      sync.Mutex
	key     string
	set     *providers.Set
	earlier []weak.Pointer[providers.Set] // the Sets built before set
	opts    providers.Options
	latest  interface{ Get() config.Config } // the store whose keys every Set holds; nil: the caller's
}

// unitProviders are the unit's providers; a test replaces its options.
var unitProviders = &providerSets{}

// follow makes every Set take its keys from store's config (nil: from the
// config each caller passes).
func (s *providerSets) follow(store interface{ Get() config.Config }) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = store
}

// of returns the Set of cfg's providers, holding the keys of the config
// saved last (follow), or cfg's when it follows no store.
func (s *providerSets) of(cfg *config.Config) *providers.Set {
	data, _ := json.Marshal(struct {
		Providers map[string]config.Provider
		File      string
		Uses      map[string][]config.ModelUse
	}{cfg.Providers, cfg.GoogleCredentialsFile, cfg.ProviderModelUses()})
	key := string(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := cfg
	if s.latest != nil {
		saved := s.latest.Get()
		keys = &saved
	}
	if s.set == nil || s.key != key {
		if s.set != nil {
			s.earlier = append(s.earlier, weak.Make(s.set))
		}
		s.set, s.key = providers.New(cfg, s.opts), key
	}
	s.set.SetKeys(keys)
	held := s.earlier[:0]
	for _, w := range s.earlier {
		if set := w.Value(); set != nil {
			set.SetKeys(keys)
			held = append(held, w)
		}
	}
	clear(s.earlier[len(held):])
	s.earlier = held
	return s.set
}

// providersKey is what a provider signs in with: the providers, their keys
// and the credential file. A voice or the speak API's engine is built again
// when it changes, not when another role's model does.
func providersKey(cfg *config.Config) string {
	data, _ := json.Marshal(struct {
		Providers map[string]config.Provider
		Keys      map[string]string
		File      string
	}{cfg.Providers, cfg.ProviderKeys, cfg.GoogleCredentialsFile})
	return string(data)
}

// providerStatuses is the settings page's providers check: each provider of
// the config saved last, checked once in maxAge.
func providerStatuses(store interface{ Get() config.Config }) func(ctx context.Context, maxAge time.Duration) []providers.Status {
	return func(ctx context.Context, maxAge time.Duration) []providers.Status {
		cfg := store.Get()
		return unitProviders.of(&cfg).Statuses(ctx, maxAge)
	}
}

// signsIn says whether a role's provider/model resolves to a provider that
// can sign in as configured: one with a key when it signs in with one.
func signsIn(cfg *config.Config, role config.Role, ref string) bool {
	m, err := cfg.ResolveModel(role, ref)
	if err != nil {
		return false
	}
	return m.Config.Auth.Kind != config.AuthAPIKey || cfg.APIKey(m.Provider) != ""
}

// canSummarize says whether summarization.model, or a preset's model, names
// a provider that can sign in.
func canSummarize(cfg *config.Config) bool {
	if signsIn(cfg, config.RoleSummarization, cfg.Summarization.Model) {
		return true
	}
	for _, preset := range cfg.Summarization.Presets {
		if preset.Model != "" && signsIn(cfg, config.RoleSummarization, preset.Model) {
			return true
		}
	}
	return false
}

// summaryClients is the summarizer's client factory: the provider of the
// config saved last, by name, with the model it names.
func summaryClients(store interface{ Get() config.Config }) summary.ClientFactory {
	return func(provider, model string) (llm.Client, error) {
		cfg := store.Get()
		if _, err := cfg.ResolveProvider(config.RoleSummarization, provider); err != nil {
			return nil, err
		}
		p, err := unitProviders.of(&cfg).Get(provider)
		if err != nil {
			return nil, err
		}
		return llm.NewClient(p, model)
	}
}

// errNoBatchProvider is why a backlog is not sent as a summary batch job:
// summarization.model's provider takes none. The backlog is summarized
// online instead (summarizeBacklog).
var errNoBatchProvider = errors.New("summary batch jobs run on the Gemini API only")

// batchClientConfig is the genai client configuration the summary batch
// operations use: summarization.model's provider, which must be the Gemini
// API. genai sends Vertex AI no batch of inlined requests (its Batches.Create
// refuses them before sending, wanting a Cloud Storage or BigQuery source),
// so a Vertex AI provider takes no batch job either; nor does another API.
func batchClientConfig(cfg *config.Config) (*genai.ClientConfig, string, error) {
	p, model, err := unitProviders.of(cfg).Model(cfg, config.RoleSummarization, cfg.Summarization.Model)
	if err != nil {
		return nil, "", err
	}
	if p.API != config.APIGemini {
		return nil, "", fmt.Errorf("%w: summarization.model's provider %q speaks the %s API", errNoBatchProvider, p.Name, p.API)
	}
	cc, err := p.GenAIConfig()
	return cc, model, err
}

// errBatchProviderGone is why a summary batch job cannot be polled: the
// provider that took it is no longer configured, no longer speaks the
// Gemini API, or has no key. Its sessions fail, and a backlog takes them.
var errBatchProviderGone = errors.New("the batch job's provider cannot be asked about it")

// batchPollClientConfig is the genai client configuration a poll of a batch
// job uses: provider's, the provider that took the job, in cfg, the config
// saved last, whatever summarization.model names now; summarization.model's
// (batchClientConfig) for a job recorded before its provider was kept
// (provider ""). It returns the provider's name, which names its refusals.
// A provider that is gone from cfg, speaks another API now, or has no key
// is errBatchProviderGone, with the reason.
func batchPollClientConfig(cfg *config.Config, provider string) (*genai.ClientConfig, string, error) {
	if provider == "" {
		clientConfig, _, err := batchClientConfig(cfg)
		name, _, _ := config.ParseModelRef(cfg.Summarization.Model)
		return clientConfig, name, err
	}
	p, ok := cfg.Providers[provider]
	switch {
	case !ok:
		return nil, provider, fmt.Errorf("%w: the batch job's provider %s is no longer configured", errBatchProviderGone, provider)
	case p.API != config.APIGemini:
		return nil, provider, fmt.Errorf("%w: the batch job's provider %s speaks the %s API now, not the Gemini API", errBatchProviderGone, provider, p.API)
	case p.Auth.Kind == config.AuthAPIKey && cfg.APIKey(provider) == "":
		return nil, provider, fmt.Errorf("%w: the batch job's provider %s has no key", errBatchProviderGone, provider)
	}
	signed, err := unitProviders.of(cfg).Get(provider)
	if err != nil {
		return nil, provider, err
	}
	clientConfig, err := signed.GenAIConfig()
	return clientConfig, provider, err
}

// newEmbeddingClient builds the embedding client embedding.model names, with
// the model as the provider names it (the stored vectors are the model's,
// whichever provider serves it), or nil when embeddings are off.
func newEmbeddingClient(cfg *config.Config) (embedding.Client, string, error) {
	if strings.TrimSpace(cfg.Embedding.Model) == "" {
		return nil, "", nil
	}
	p, model, err := unitProviders.of(cfg).Model(cfg, config.RoleEmbedding, cfg.Embedding.Model)
	if err != nil {
		return nil, "", err
	}
	client, err := embedding.NewClient(p, model)
	return client, model, err
}

// errDeepgramKeyMissing is why batch transcription cannot run: its
// provider's key is not configured.
var errDeepgramKeyMissing = errors.New("deepgram API key not configured")

func makeBatchTranscriber(cfg *config.Config) (transcribe.BatchTranscriber, error) {
	p, model, err := unitProviders.of(cfg).Model(cfg, config.RoleBatchTranscription, cfg.BatchTranscription.Model)
	if err != nil {
		return nil, err
	}
	key, err := p.Credential(context.Background())
	if errors.Is(err, providers.ErrNoKey) {
		return nil, errDeepgramKeyMissing
	}
	if err != nil {
		return nil, err
	}
	// The provider's client: the key and the recording go only to its origin.
	return transcribe.NewDeepgramBatchTranscriber(transcribe.DeepgramBatchConfig{
		APIKey:     key,
		Model:      model,
		BaseURL:    p.BaseURL(),
		HTTPClient: p.HTTPClient(nil),
	}), nil
}

// liveTranscriptionModel is transcription.model's model, as Deepgram names
// it, or "" when it resolves to no provider.
func liveTranscriptionModel(cfg *config.Config) string {
	m, err := cfg.ResolveModel(config.RoleTranscription, cfg.Transcription.Model)
	if err != nil {
		return ""
	}
	return m.Model
}

// liveTranscriptionClientOptions are the live Deepgram client's options, as
// main dials it: keep-alive on, and the host of transcription.model's
// provider (liveTranscriptionHost).
func liveTranscriptionClientOptions(cfg *config.Config) *interfaces.ClientOptions {
	return &interfaces.ClientOptions{EnableKeepAlive: true, Host: liveTranscriptionHost(cfg)}
}

// liveTranscriptionHost is the socket host of transcription.model's
// provider when it names a url, as the Deepgram SDK takes one
// (wss://host); "" is Deepgram's own.
func liveTranscriptionHost(cfg *config.Config) string {
	p, _, err := unitProviders.of(cfg).Model(cfg, config.RoleTranscription, cfg.Transcription.Model)
	if err != nil || p.URL == "" {
		return ""
	}
	u, err := url.Parse(p.SocketURL())
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
