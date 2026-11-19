package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	voicetts "github.com/sjawhar/ghost-wispr/internal/voice/tts"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// voiceControl is the voice as the rest of the unit drives it; the
// voice.Supervisor is one.
type voiceControl interface {
	SessionEnded(sessionID string)
	SessionsDeleted(sessionIDs []string)
	SegmentsDeleted(sessionID string)
	GateClosed(closings int)
	Stop(reason string)
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

// wireVoiceToCapture stops the voice on every capture gate closing. It must
// run before anything can close the gate.
func wireVoiceToCapture(capture *captureGate, link *voiceLink) {
	capture.onClosed = link.GateClosed
}

// voiceKeyterms are the keyterms the live Deepgram request carries while the
// voice is on: the wake phrases. Only Nova-3 takes keyterms.
func voiceKeyterms(cfg *config.Config) []string {
	if !cfg.Voice.Enabled || !strings.HasPrefix(cfg.DeepgramModel, "nova-3") {
		return nil
	}
	return transcript.Keyterms(cfg.Voice.WakePhrases, nil)
}

// voiceDeps is what the voice needs from the rest of the unit.
type voiceDeps struct {
	output    *audio.Output // the speaker output; nil without a speaker
	tap       *voice.Tap
	concierge voice.Concierge
	link      *voiceLink
	session   func() string // the recording session now
	red       func() bool   // the light reads red and audio passes the gate
	notify    func(kind, sessionID string)
}

// startVoice starts the meeting voice when the config turns it on, and sets it
// on deps.link. It returns why the voice cannot start; the unit then runs
// without it.
func startVoice(ctx context.Context, cfg *config.Config, deps *voiceDeps) error {
	if !cfg.Voice.Enabled {
		return nil
	}
	if deps.output == nil {
		return errors.New("the voice needs the speaker: set speaker_enabled")
	}
	if deps.concierge == nil {
		return errors.New("the voice has no Concierge")
	}
	if cfg.Voice.Engine != config.VoiceEngineTTS {
		return fmt.Errorf("voice.engine %q is not in this build; use %q", cfg.Voice.Engine, config.VoiceEngineTTS)
	}
	v, err := newVoiceA(ctx, cfg)
	if err != nil {
		return err
	}
	sup := voice.NewSupervisor(&voice.SupervisorConfig{
		Voice:       func() voice.Voice { return v },
		Concierge:   deps.concierge,
		Speaker:     voice.SpeakerOf(deps.output),
		WakePhrases: cfg.Voice.WakePhrases,
		Session:     deps.session,
		Red:         deps.red,
		Notify:      deps.notify,
		Tap:         deps.tap,
		Logf:        log.Printf,
	})
	deps.link.set(sup)
	log.Printf("voice: on (voice A on %s, wake phrases %q)", cfg.Voice.TTSEngine, cfg.Voice.WakePhrases)
	return nil
}

// newVoiceA builds voice A on the text-to-speech engine cfg names.
func newVoiceA(ctx context.Context, cfg *config.Config) (voice.Voice, error) {
	switch cfg.Voice.TTSEngine {
	case config.TTSEngineAura:
		model := cfg.Voice.TTSVoice
		if model == "" {
			model = cfg.Voice.TTSModel
		}
		engine, err := voicetts.NewAura(voicetts.AuraConfig{APIKey: cfg.DeepgramAPIKey, Model: model})
		if err != nil {
			return nil, fmt.Errorf("voice A on Aura: %w", err)
		}
		return voicetts.New(engine), nil
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
		})
		if err != nil {
			return nil, fmt.Errorf("voice A on Gemini: %w", err)
		}
		return voicetts.New(engine), nil
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
