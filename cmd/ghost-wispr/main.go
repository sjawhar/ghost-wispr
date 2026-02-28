package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"
	"github.com/gordonklaus/portaudio"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

//go:embed static/*
var staticFiles embed.FS

type recorderState struct {
	mic    *audio.Mic
	mu     sync.RWMutex
	paused bool
}

func (r *recorderState) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = true
}

func (r *recorderState) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = false
}

func (r *recorderState) IsPaused() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.paused
}

func (r *recorderState) SetMic(mic *audio.Mic) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mic = mic
}

type transcriptCallback struct {
	manager session.LifecycleManager
}

func (c transcriptCallback) Message(mr *api.MessageResponse) error {
	if c.manager == nil {
		return nil
	}
	return c.manager.Message(mr)
}

func (c transcriptCallback) Open(*api.OpenResponse) error {
	log.Println("connected to Deepgram")
	return nil
}

func (c transcriptCallback) Metadata(*api.MetadataResponse) error { return nil }

func (c transcriptCallback) SpeechStarted(*api.SpeechStartedResponse) error { return nil }

func (c transcriptCallback) UtteranceEnd(ur *api.UtteranceEndResponse) error {
	if c.manager == nil {
		return nil
	}
	return c.manager.UtteranceEnd(ur)
}

func (c transcriptCallback) Close(*api.CloseResponse) error {
	log.Println("disconnected from Deepgram")
	return nil
}

func (c transcriptCallback) Error(er *api.ErrorResponse) error {
	log.Printf("deepgram error %s: %s", er.ErrCode, er.Description)
	return nil
}

func (c transcriptCallback) UnhandledEvent([]byte) error { return nil }

func main() {
	log.Println("ghost-wispr: starting")

	configPath := os.Getenv(config.EnvPrefix + "CONFIG")
	if configPath == "" {
		configPath = "ghost-wispr.yaml"
	}

	cfg, cfgWarnings, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	for _, w := range cfgWarnings {
		log.Printf("config: %s", w)
	}

	store, err := storage.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("storage init failed: %v", err)
	}

	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static assets init failed: %v", err)
	}

	hub := server.NewHub()
	detector := session.NewDetector(cfg.ParsedSilenceTimeout())
	audioRecorder := audio.NewRecorder(cfg.AudioDir)

	apiKeys := map[string]string{
		"openai":    cfg.OpenAIAPIKey,
		"anthropic": cfg.AnthropicAPIKey,
		"gemini":    cfg.GeminiAPIKey,
	}

	clientFactory := func(provider, model string) (llm.Client, error) {
		key := apiKeys[provider]
		if key == "" {
			return nil, fmt.Errorf("no API key for provider %q", provider)
		}
		var opts []llm.Option
		if provider == "openai" && cfg.Summarization.BaseURL != "" {
			opts = append(opts, llm.WithBaseURL(cfg.Summarization.BaseURL))
		}
		return llm.NewClient(provider, key, model, opts...)
	}

	var summarizer *summary.Summarizer
	canSummarize := false
	if provider, _, err := llm.ParseModel(cfg.Summarization.Model); err == nil && apiKeys[provider] != "" {
		canSummarize = true
	}
	if !canSummarize {
		for _, preset := range cfg.Summarization.Presets {
			if preset.Model == "" {
				continue
			}
			if provider, _, err := llm.ParseModel(preset.Model); err == nil && apiKeys[provider] != "" {
				canSummarize = true
				break
			}
		}
	}
	if canSummarize {
		summarizer = summary.New(cfg.Summarization, clientFactory)
	}

	var sessionSummarizer session.Summarizer
	if summarizer != nil {
		sessionSummarizer = summarizer
	}

	manager := session.NewManager(store, audioRecorder, sessionSummarizer, hub, detector)

	recState := &recorderState{}
	warnings := append([]string{}, cfgWarnings...)

	handler, err := server.Handler(assets, hub, store, server.ControlHooks{
		Pause:    recState.Pause,
		Resume:   recState.Resume,
		IsPaused: recState.IsPaused,
		OnStatusChanged: func(paused bool) {
			hub.BroadcastStatusChanged(paused)
		},
		Warnings: func() []string { return warnings },
		Presets: func() map[string]config.Preset {
			if summarizer == nil {
				return nil
			}
			return summarizer.Presets()
		},
		Resummarize: func(ctx context.Context, sessionID, preset string) error {
			if summarizer == nil {
				return fmt.Errorf("summarization not configured")
			}

			segments, err := store.GetSegments(sessionID)
			if err != nil {
				return err
			}

			var b strings.Builder
			for _, seg := range segments {
				if strings.TrimSpace(seg.Text) != "" {
					b.WriteString(seg.Text)
					b.WriteString("\n")
				}
			}
			transcript := b.String()

			_ = store.UpdateSummary(sessionID, "", storage.SummaryRunning, "")
			hub.BroadcastSummaryReady(sessionID, "", storage.SummaryRunning, "")

			var summaryText string
			var presetUsed string
			if preset != "" {
				presetUsed = preset
				summaryText, err = summarizer.SummarizeWithPreset(ctx, sessionID, transcript, preset)
			} else {
				summaryText, presetUsed, err = summarizer.Summarize(ctx, sessionID, transcript)
			}

			status := storage.SummaryCompleted
			if err != nil {
				status = storage.SummaryFailed
			}
			_ = store.UpdateSummary(sessionID, summaryText, status, presetUsed)
			hub.BroadcastSummaryReady(sessionID, summaryText, status, presetUsed)
			return err
		},
	})
	if err != nil {
		log.Fatalf("build http handler failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = store.Close() }()

	if cfg.GDriveFolderID != "" {
		syncer, syncErr := gdrive.NewSyncer(ctx, cfg.GoogleCredentialsFile, cfg.GDriveFolderID)
		if syncErr != nil {
			log.Printf("warning: gdrive sync disabled: %v", syncErr)
			warnings = append(warnings, "Google Drive sync failed to initialize \u2014 backups are disabled")
		} else {
			go func() {
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						date := time.Now().UTC().Format("2006-01-02")
						if err := syncer.Sync(cfg.DBPath, date); err != nil {
							log.Printf("gdrive sync error: %v", err)
						}
					}
				}
			}()
		}
	}

	var mic *audio.Mic
	var dgWriter io.Writer
	var dgStop func()
	selectedSampleRate := cfg.MicSampleRate

	paErr := portaudio.Initialize()
	//nolint:errcheck // Terminate is best-effort cleanup
	defer portaudio.Terminate()
	if paErr != nil {
		log.Fatalf("portaudio init failed: %v", paErr) //nolint:gocritic // Terminate is no-op if Initialize failed
	}

	client.Init(client.InitLib{LogLevel: client.LogLevelDefault})

	for _, rate := range cfg.SampleRateCandidates() {
		mic, err = audio.NewMic(rate, rate/4) // 250ms buffer
		if err != nil {
			log.Printf("warning: microphone open failed at %d Hz: %v", rate, err)
			continue
		}
		selectedSampleRate = rate
		break
	}

	if mic == nil {
		log.Printf("warning: microphone unavailable, running API/UI only")
		warnings = append(warnings, "Microphone unavailable \u2014 recording and live transcription are disabled")
	} else {
		audioRecorder.SetSampleRate(selectedSampleRate)
		recState.SetMic(mic)
		if err := mic.Start(); err != nil {
			log.Printf("warning: microphone start failed at %d Hz, running API/UI only: %v", selectedSampleRate, err)
			mic = nil
			recState.SetMic(nil)
			warnings = append(warnings, "Microphone failed to start \u2014 recording and live transcription are disabled")
		} else {
			log.Printf("microphone started at %d Hz", selectedSampleRate)
		}
	}

	if mic != nil && cfg.DeepgramAPIKey != "" {
		cOptions := &interfaces.ClientOptions{EnableKeepAlive: true}
		tOptions := &interfaces.LiveTranscriptionOptions{
			Model:       "nova-2",
			Language:    "en-US",
			Diarize:     true,
			Punctuate:   true,
			SmartFormat: true,
			Encoding:    "linear16",
			SampleRate:  selectedSampleRate,
			Channels:    1,
		}

		dgClient, err := client.NewWSUsingCallback(ctx, cfg.DeepgramAPIKey, cOptions, tOptions, transcriptCallback{manager: manager})
		if err != nil {
			log.Printf("warning: deepgram client unavailable, running API/UI only: %v", err)
			warnings = append(warnings, "Deepgram initialization failed \u2014 live transcription is disabled")
		} else if ok := dgClient.Connect(); !ok {
			log.Printf("warning: deepgram connect failed, running API/UI only")
			warnings = append(warnings, "Deepgram connection failed \u2014 live transcription is disabled")
		} else {
			dgWriter = dgClient
			dgStop = func() {
				dgClient.Stop()
			}
			go func() {
				streamMicWithRetry(ctx, mic, audioRecorder.Writer(dgWriter), time.Sleep, log.Printf)
			}()
		}
	}

	httpServer := &http.Server{Addr: ":8080", Handler: handler}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server error: %v", err)
		}
	}()

	log.Println("ghost-wispr: web UI on http://127.0.0.1:8080")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("ghost-wispr: shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := manager.ForceEndSession(shutdownCtx); err != nil {
		log.Printf("warning: force end session failed: %v", err)
	}

	if dgStop != nil {
		dgStop()
	}
	if mic != nil {
		_ = mic.Stop()
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("warning: http shutdown failed: %v", err)
	}
}

type micStreamer interface {
	Stream(writer io.Writer) error
}

func streamMicWithRetry(
	ctx context.Context,
	streamer micStreamer,
	writer io.Writer,
	wait func(time.Duration),
	logf func(string, ...any),
) {
	for {
		if ctx.Err() != nil {
			return
		}

		err := streamer.Stream(writer)
		if err == nil || ctx.Err() != nil {
			return
		}

		if strings.Contains(strings.ToLower(err.Error()), "overflow") {
			logf("warning: mic input overflow, restarting stream")
			wait(250 * time.Millisecond)
			continue
		}

		logf("mic stream error: %v", err)
		return
	}
}
