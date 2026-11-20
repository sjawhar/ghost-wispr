package tts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
)

// Option changes how an engine NewEngine builds reaches its provider.
type Option func(*options)

type options struct {
	base   *http.Client
	dialed func(*url.URL)
}

// WithHTTPClient makes an engine that speaks over HTTP send its requests
// through base, which the provider's sign-in wraps (Provider.HTTPClient). A
// Session's warm-up goes through base itself, so the connection it opens is
// in the pool the first request draws from. The default is a plain client.
func WithHTTPClient(base *http.Client) Option {
	return func(o *options) { o.base = base }
}

// WithDialed tells dialed the URL of each socket's handshake request, as it
// went to the provider, once the provider accepted it: ghost-wispr
// tts-first-audio reads from Aura's whether it asked with mip_opt_out=true.
// Engines that speak over HTTP open no socket.
func WithDialed(dialed func(*url.URL)) Option {
	return func(o *options) { o.dialed = dialed }
}

// Voiced is an Engine that names the voice it speaks in and the model it
// asks its provider for. NewEngine builds one.
type Voiced interface {
	Engine
	// Voice is the voice it was given, or its API's default.
	Voice() string
	// Model is the model it was given, or for Deepgram Aura and Cloud
	// Text-to-Speech, whose voices are each a model, the voice that
	// replaced it.
	Model() string
}

// ModelReporter is an Engine whose answer names the model that spoke, which
// can differ from the model it asked for: Gemini TTS's modelVersion.
// StreamModel is its Stream, and returns that model too: "" when the answer
// named none, and "unknown" when it named one that is no plain token
// (providererr.Token), since an answer's text can be anything and the model
// reaches a log and a request's status.
type ModelReporter interface {
	Engine
	StreamModel(ctx context.Context, text string, emit func(pcm []byte) error) (model string, err error)
}

// NewEngine returns the engine that speaks model, in voice, through p: the
// tts role's provider/model and tts.voice. Its requests sign in only through
// p. voice "" is the API's default voice; for Deepgram Aura and Cloud
// Text-to-Speech, whose voices are each a model, it is the model, and a
// voice replaces the model.
//
// Each API's engine is one file and one case here.
func NewEngine(p *providers.Provider, model, voice string, opts ...Option) (Voiced, error) {
	if p == nil {
		return nil, errors.New("tts: no provider")
	}
	model, voice = strings.TrimSpace(model), strings.TrimSpace(voice)
	if model == "" {
		return nil, fmt.Errorf("tts: provider %q: a model is required", p.Name)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	switch p.API {
	case config.APIVertex:
		return newVertex(p, model, voice, &o)
	case config.APIGemini:
		return newGeminiAPI(p, model, voice, &o), nil
	case config.APIOpenAI:
		return newOpenAI(p, model, voice, &o), nil
	case config.APIDeepgram:
		return newAura(p, model, voice, &o)
	case config.APIElevenLabs:
		return newElevenLabs(p, model, voice, &o), nil
	case config.APIGoogleTTS:
		return newGoogleTTS(p, model, voice, &o), nil
	default:
		return nil, fmt.Errorf("tts: provider %q speaks the %s API, which has no text-to-speech engine", p.Name, p.API)
	}
}
