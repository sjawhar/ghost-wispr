package tts

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	voicetts "github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

// maxEngines bounds the engines an EngineProvider holds: a request may name
// any voice, and each one it names builds an engine.
const maxEngines = 16

// EngineProvider is the tts role as a Provider: it speaks the role's model
// through the streaming engines voice A speaks through (voicetts.NewEngine),
// collecting an engine's PCM into one SpeechResponse. It builds one engine
// for each voice a request names, once, and speaks a request that names
// none in tts.voice.
type EngineProvider struct {
	provider *providers.Provider
	model    string
	voice    string // tts.voice
	maxLen   int
	opts     []voicetts.Option

	mu      sync.Mutex
	engines map[string]voicetts.Voiced // by the voice asked for
}

// NewEngineProvider returns the Provider of the tts role: model, as p names
// it, spoken in voice ("" for the engine's default), refusing a text longer
// than maxLength characters (tts.max_length; DefaultMaxTextLength when 0).
// It builds the engine of voice at once, so a provider no engine speaks
// with fails here, at start, not at the first request.
func NewEngineProvider(p *providers.Provider, model, voice string, maxLength int, opts ...voicetts.Option) (*EngineProvider, error) {
	e := &EngineProvider{
		provider: p,
		model:    strings.TrimSpace(model),
		voice:    strings.TrimSpace(voice),
		maxLen:   DefaultMaxTextLength,
		opts:     opts,
		engines:  map[string]voicetts.Voiced{},
	}
	if maxLength > 0 {
		e.maxLen = maxLength
	}
	if _, err := e.engine(e.voice); err != nil {
		return nil, err
	}
	return e, nil
}

// Name is the provider's name in the config.
func (e *EngineProvider) Name() string { return e.provider.Name }

// Synthesize speaks req.Text in req.Voice, or tts.voice when it names none,
// and returns the whole speech, with the voice the engine spoke in and the
// model that spoke: the one the answer names (Gemini's modelVersion;
// llm.ModelNotReportedByProvider when it names none), else the one the
// engine asked for: for Deepgram Aura and Cloud Text-to-Speech, each of
// whose voices is a model, the voice. Speech is its whole sample frames: a
// synthesis that gave none (Aura's Flushed with no audio before it, or less
// than one frame) fails, as voice A fails it, and a byte past the last whole
// frame is no audio.
func (e *EngineProvider) Synthesize(ctx context.Context, req SpeechRequest) (*SpeechResponse, error) {
	if err := validateText(req.Text, e.maxLen); err != nil {
		return nil, fmt.Errorf("tts: %w", err)
	}
	engine, err := e.engine(cmp.Or(strings.TrimSpace(req.Voice), e.voice))
	if err != nil {
		return nil, err
	}
	var pcm []byte
	collect := func(chunk []byte) error {
		pcm = append(pcm, chunk...)
		return nil
	}
	model := engine.Model()
	if reporter, ok := engine.(voicetts.ModelReporter); ok {
		reported, err := reporter.StreamModel(ctx, req.Text, collect)
		if err != nil {
			return nil, err
		}
		model = cmp.Or(reported, llm.ModelNotReportedByProvider)
	} else if err := engine.Stream(ctx, req.Text, collect); err != nil {
		return nil, err
	}
	format := engine.Format()
	frame := 2 * format.Channels
	if frame <= 0 || format.SampleRate <= 0 {
		return nil, fmt.Errorf("tts: %s speaks at %d Hz with %d channels", e.provider.Name, format.SampleRate, format.Channels)
	}
	pcm = pcm[:len(pcm)-len(pcm)%frame]
	if len(pcm) == 0 {
		return nil, fmt.Errorf("tts: %s: %w", e.provider.Name, voicetts.ErrNoAudio)
	}
	samples := int64(len(pcm) / frame)
	return &SpeechResponse{
		Audio:      pcm,
		Format:     format,
		DurationMs: samples * 1000 / int64(format.SampleRate),
		Model:      model,
		Voice:      engine.Voice(),
	}, nil
}

// engine returns the engine of voice, built the first time it is asked for.
// When maxEngines are held, it drops one, never tts.voice's.
func (e *EngineProvider) engine(voice string) (voicetts.Voiced, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if engine, ok := e.engines[voice]; ok {
		return engine, nil
	}
	engine, err := voicetts.NewEngine(e.provider, e.model, voice, e.opts...)
	if err != nil {
		return nil, err
	}
	if len(e.engines) >= maxEngines {
		for held := range e.engines {
			if held != e.voice {
				delete(e.engines, held)
				break
			}
		}
	}
	e.engines[voice] = engine
	return engine, nil
}
