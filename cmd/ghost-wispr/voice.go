package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
	voicetts "github.com/sjawhar/ghost-wispr/internal/voice/tts"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// voiceControl is the voice as the rest of the unit drives it; the
// voice.Supervisor is one.
type voiceControl interface {
	SessionEnded(sessionID string)
	EnabledChanged()
	SessionsDeleted(sessionIDs []string)
	SegmentsDeleted(sessionID string)
	GateClosed(closings int)
	Stop(reason string)
	Started()
	RedCheckFailed()
	OnTranscript(t *voice.Transcript)
	OnUtteranceEnd(closings int, gateOpen bool)
	WithFloor(ctx context.Context, start func()) error
	Close()
}

// voiceLink is where the capture gate, the Jabra button, the transcript
// reader, the speech queue and shutdown reach the voice. They are wired to it
// at start-up, before the voice exists (the voice needs the speaker output,
// which is built after the microphone starts); until set, and with the voice
// off, it does nothing.
type voiceLink struct {
	mu sync.RWMutex
	v  voiceControl
}

func (l *voiceLink) set(v voiceControl) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.v = v
}

func (l *voiceLink) get() voiceControl {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.v
}

// GateClosed stops the voice on every capture gate closing: a pause or a
// light hold.
func (l *voiceLink) GateClosed(closings int) {
	if v := l.get(); v != nil {
		v.GateClosed(closings)
	}
}

// Stop stops the voice, as a Jabra press does.
func (l *voiceLink) Stop(reason string) {
	if v := l.get(); v != nil {
		v.Stop(reason)
	}
}

// EnabledChanged tells the voice a settings change of voice.enabled; turned
// off, it stops at once.
func (l *voiceLink) EnabledChanged() {
	if v := l.get(); v != nil {
		v.EnabledChanged()
	}
}

// followVoiceEnabled is the settings hook of voice.enabled: turned off, the
// voice stops at once, then its Concierge (R13).
func followVoiceEnabled(link *voiceLink, conc *lazyConcierge) func(*config.Config) {
	return func(cfg *config.Config) {
		link.EnabledChanged()
		conc.followEnabled(cfg.Voice.Enabled)
	}
}

func (l *voiceLink) OnTranscript(t *voice.Transcript) {
	if v := l.get(); v != nil {
		v.OnTranscript(t)
	}
}

func (l *voiceLink) OnUtteranceEnd(closings int, gateOpen bool) {
	if v := l.get(); v != nil {
		v.OnUtteranceEnd(closings, gateOpen)
	}
}

// WithFloor begins other speech (POST /api/tts/speak) only while no exchange
// holds the floor; with the voice off, at once.
func (l *voiceLink) WithFloor(ctx context.Context, start func()) error {
	if v := l.get(); v != nil {
		return v.WithFloor(ctx, start)
	}
	start()
	return nil
}

// Close stops the voice for good, at shutdown.
func (l *voiceLink) Close() {
	if v := l.get(); v != nil {
		v.Close()
	}
}

// SessionEnded drops the held answers of an ended session.
func (l *voiceLink) SessionEnded(sessionID string) {
	if v := l.get(); v != nil {
		v.SessionEnded(sessionID)
	}
}

// SessionsDeleted forgets deleted sessions.
func (l *voiceLink) SessionsDeleted(sessionIDs []string) {
	if v := l.get(); v != nil {
		v.SessionsDeleted(sessionIDs)
	}
}

// SegmentsDeleted forgets the recent transcript after a segment delete.
func (l *voiceLink) SegmentsDeleted(sessionID string) {
	if v := l.get(); v != nil {
		v.SegmentsDeleted(sessionID)
	}
}

// Started plays the start sound: capture moved into recording.
func (l *voiceLink) Started() {
	if v := l.get(); v != nil {
		v.Started()
	}
}

// RedCheckFailed plays the error sound of a failed red check.
func (l *voiceLink) RedCheckFailed() {
	if v := l.get(); v != nil {
		v.RedCheckFailed()
	}
}

// wireVoiceToCapture stops the voice on every capture gate closing, and plays
// the start sound at the first audio each opening into recording records, so
// it never holds the Jabra's light before the microphone opens, and the error
// sound on every failed red check (R10). It must run before anything can
// close the gate.
func wireVoiceToCapture(capture *captureGate, link *voiceLink, conc *conciergeHooks) {
	capture.onClosed = func(closings int) {
		link.GateClosed(closings)
		conc.GateClosed() // what the Concierge read before a session existed goes too
	}
	capture.onRecording = link.Started
	capture.onRedCheckFailed = link.RedCheckFailed
}

// voiceKeyterms are the keyterms the live Deepgram request carries while a
// voice can run, which is while the unit has a speaker: a person turns the
// voice on with no restart, and the live request is not redialled for it.
// They are the wake phrases, then Dispatch's project keys, so "infra" is
// not heard as "in for" (R4). Only Nova-3 takes keyterms.
func voiceKeyterms(cfg *config.Config, projects []string) []string {
	if !cfg.SpeakerEnabled || !strings.HasPrefix(cfg.DeepgramModel, "nova-3") {
		return nil
	}
	return transcript.Keyterms(cfg.Voice.WakePhrases, projects)
}

// voiceDeps is what the voice needs from the rest of the unit.
type voiceDeps struct {
	output      *audio.Output // the speaker output; nil without a speaker
	tap         *voice.Tap
	concierge   voice.Concierge
	link        *voiceLink
	voices      func() (voice.Voice, error) // the voice for the next call
	enabled     func() bool                 // voice.enabled, live
	wakePhrases []string
	timing      func(event, sessionID string, at time.Time, kv ...any)
	session     func() string // the recording session now
	red         func() bool   // the light reads red and audio passes the gate
	notify      func(kind, sessionID string)
	projects    func() []string // Dispatch's project keys, for spoken keys
}

// startVoice starts the Supervisor over the speaker output, and sets it on
// deps.link. voice.enabled and the voice are read at each call, so it starts
// whatever they say; it returns why it cannot start.
func startVoice(deps *voiceDeps) error {
	if deps.output == nil {
		return errors.New("the voice needs the speaker: set speaker_enabled")
	}
	if deps.concierge == nil {
		return errors.New("the voice has no Concierge")
	}
	sup := voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:       deps.voices,
		Concierge:   deps.concierge,
		Speaker:     voice.SpeakerOf(deps.output),
		WakePhrases: deps.wakePhrases,
		Session:     deps.session,
		Red:         deps.red,
		Enabled:     deps.enabled,
		Notify:      deps.notify,
		ProjectKeys: deps.projects,
		Tap:         deps.tap,
		Timing:      deps.timing,
		Logf:        log.Printf,
	})
	deps.link.set(sup)
	log.Printf("voice: ready (wake phrases %q; voice.enabled and voice.engine are read at each call)", deps.wakePhrases)
	return nil
}

// voiceSource holds the voice each call uses, built from the settings saved
// last. It builds off the Supervisor's path: apply runs at start-up and after
// each settings change, and Voice only reads what apply built.
type voiceSource struct {
	mu    sync.Mutex
	key   string
	built voice.Voice
	err   error
}

// apply builds the voice cfg names, unless the voice settings did not change.
func (v *voiceSource) apply(ctx context.Context, cfg *config.Config) {
	if cfg.Voice.Engine != config.VoiceEngineTTS {
		v.set("", nil, fmt.Errorf("voice.engine %q is not in this build; use %q", cfg.Voice.Engine, config.VoiceEngineTTS))
		return
	}
	key := strings.Join([]string{cfg.Voice.TTSEngine, cfg.Voice.TTSModel, cfg.Voice.TTSVoice, cfg.Voice.TTSLocation, cfg.GCPProject, cfg.GoogleCredentialsFile, cfg.DeepgramAPIKey}, "\x00")
	v.mu.Lock()
	same := v.key == key && (v.built != nil || v.err != nil)
	v.mu.Unlock()
	if same {
		return
	}
	built, err := newVoiceA(ctx, cfg)
	v.set(key, built, err)
}

func (v *voiceSource) set(key string, built voice.Voice, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.key, v.built, v.err = key, built, err
}

// Voice returns the voice for the next call, or why there is none.
func (v *voiceSource) Voice() (voice.Voice, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.built, v.err
}

// warnBuild puts why the voice the settings name cannot be built on the
// status page while the voice is on, so the operator need not wait for a
// call's error sound to learn it; it clears the warning otherwise.
func (v *voiceSource) warnBuild(enabled bool, warnings *warningsRegistry) {
	if _, err := v.Voice(); enabled && err != nil {
		warnings.Set("voice_engine", "The meeting voice cannot take a call: "+err.Error())
		return
	}
	warnings.Clear("voice_engine")
}

// voiceTiming writes the Supervisor's timing events to the journal, as the
// acceptance driver reads them: msg "voice timing", with event, session_id
// and at_unix_ns.
func voiceTiming(logger *slog.Logger) func(event, sessionID string, at time.Time, kv ...any) {
	return func(event, sessionID string, at time.Time, kv ...any) {
		attrs := append([]any{"event", event, "session_id", sessionID, "at_unix_ns", at.UnixNano()}, kv...)
		logger.Info("voice timing", attrs...)
	}
}

// newVoiceA builds voice A on the text-to-speech engine cfg names.
func newVoiceA(ctx context.Context, cfg *config.Config) (voice.Voice, error) {
	engine, err := voiceAEngine(ctx, cfg, nil)
	if err != nil {
		return nil, err
	}
	return voicetts.New(engine), nil
}

// voiceAOptions is what ghost-wispr tts-first-audio adds to voice A's engine.
// The unit's voice adds nothing (nil).
type voiceAOptions struct {
	deepgramBaseURL string         // replaces Deepgram's address, for tests
	vertexBaseURL   string         // replaces Vertex AI's host, for tests
	auraDialed      func(*url.URL) // told the URL of each Aura socket
}

// voiceAEngine builds the text-to-speech engine of voice A that cfg names.
func voiceAEngine(ctx context.Context, cfg *config.Config, opts *voiceAOptions) (voicetts.Engine, error) {
	if opts == nil {
		opts = &voiceAOptions{}
	}
	switch cfg.Voice.TTSEngine {
	case config.TTSEngineAura:
		model := cfg.Voice.TTSVoice
		if model == "" {
			model = cfg.Voice.TTSModel
		}
		engine, err := voicetts.NewAura(voicetts.AuraConfig{APIKey: cfg.DeepgramAPIKey, Model: model, BaseURL: opts.deepgramBaseURL, Dialed: opts.auraDialed})
		if err != nil {
			return nil, fmt.Errorf("voice A on Aura: %w", err)
		}
		return engine, nil
	case config.TTSEngineGemini:
		credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("voice A on Gemini: detect Google credentials: %w", err)
		}
		engine, err := voicetts.NewGemini(&voicetts.GeminiConfig{
			Project:     cfg.GCPProject,
			Location:    cfg.Voice.TTSLocation,
			Model:       cfg.Voice.TTSModel,
			Voice:       cfg.Voice.TTSVoice,
			Credentials: credentials,
			BaseURL:     opts.vertexBaseURL,
		})
		if err != nil {
			return nil, fmt.Errorf("voice A on Gemini: %w", err)
		}
		return engine, nil
	default:
		return nil, fmt.Errorf("voice.tts_engine %q is not one of %q, %q", cfg.Voice.TTSEngine, config.TTSEngineGemini, config.TTSEngineAura)
	}
}

// wireVoiceSessionHooks makes every session end reach the Concierge and the
// voice, and returns what a session delete (the API's and the GC's) calls to
// make them forget it.
func wireVoiceSessionHooks(manager interface{ SetSessionEndedHook(func(string)) }, concierge *conciergeHooks, link *voiceLink) func([]string) {
	manager.SetSessionEndedHook(func(sessionID string) {
		concierge.SessionEnded(sessionID)
		link.SessionEnded(sessionID)
	})
	return func(sessionIDs []string) {
		concierge.SessionsDeleted(sessionIDs)
		link.SessionsDeleted(sessionIDs)
	}
}

// wireVoiceSegmentDeletion makes the voice forget its recent transcript after
// a segment delete or cut, which may have removed words it holds.
func wireVoiceSegmentDeletion(hooks *server.ControlHooks, link *voiceLink) {
	for _, hook := range []*func(context.Context, string, int64, int) error{&hooks.DeleteSegment, &hooks.DeleteSegmentText} {
		deleteSegment := *hook
		if deleteSegment == nil {
			continue
		}
		*hook = func(ctx context.Context, sessionID string, segmentID int64, revision int) error {
			err := deleteSegment(ctx, sessionID, segmentID, revision)
			link.SegmentsDeleted(sessionID)
			return err
		}
	}
}
