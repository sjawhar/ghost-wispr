package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

//go:embed static/*
var staticFiles embed.FS

func buildTranscript(segments []transcribe.Segment) string {
	var b strings.Builder
	for _, seg := range segments {
		if strings.TrimSpace(seg.Text) != "" {
			b.WriteString(seg.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func buildCanonicalTranscript(store *storage.SQLiteStore, sessionID string, segments []transcribe.Segment) string {
	streamingTranscript := buildTranscript(segments)
	refined, status, err := store.GetRefinement(sessionID)
	if err != nil {
		return streamingTranscript
	}
	if status == "completed" && strings.TrimSpace(refined) != "" {
		return refined
	}

	return streamingTranscript
}

func makeBatchTranscriber(cfg config.Config) (transcribe.BatchTranscriber, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.BatchTranscription.Provider)) {
	case "", "deepgram":
		if strings.TrimSpace(cfg.DeepgramAPIKey) == "" {
			return nil, fmt.Errorf("Deepgram API key not configured")
		}
		return transcribe.NewDeepgramBatchTranscriber(transcribe.DeepgramBatchConfig{
			APIKey: cfg.DeepgramAPIKey,
			Model:  cfg.BatchTranscription.Model,
		}), nil
	case "groq", "openai":
		return nil, fmt.Errorf("batch transcription provider %q is not implemented yet", cfg.BatchTranscription.Provider)
	default:
		return nil, fmt.Errorf("unsupported batch transcription provider %q", cfg.BatchTranscription.Provider)
	}
}

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
	manager   session.LifecycleManager
	resilient *transcribe.ResilientClient
}

func (c transcriptCallback) Message(mr *api.MessageResponse) error {
	if c.manager == nil {
		return nil
	}
	return c.manager.Message(mr)
}

func (c transcriptCallback) Open(*api.OpenResponse) error {
	log.Println("connected to Deepgram")
	if c.resilient != nil {
		c.resilient.SetConnected()
	}
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
	if mgr, ok := c.manager.(*session.Manager); ok {
		mgr.OnTranscriptionDisconnect()
	}
	return nil
}

func (c transcriptCallback) Error(er *api.ErrorResponse) error {
	log.Printf("deepgram error %s: %s", er.ErrCode, er.Description)
	return nil
}

func (c transcriptCallback) UnhandledEvent([]byte) error { return nil }

type deepgramWriter struct {
	client interface {
		io.Writer
		Finalize() error
	}
}

func (dw *deepgramWriter) Write(p []byte) (int, error) {
	return dw.client.Write(p)
}

func (dw *deepgramWriter) Close() error {
	return dw.client.Finalize()
}

func makeDeepgramClientFactory(ctx context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, callback transcriptCallback) transcribe.ClientFactory {
	return func(factoryCtx context.Context) (io.WriteCloser, error) {
		dgClient, err := client.NewWSUsingCallback(factoryCtx, apiKey, cOptions, tOptions, callback)
		if err != nil {
			return nil, err
		}
		if !dgClient.Connect() {
			return nil, fmt.Errorf("deepgram connect failed")
		}
		return &deepgramWriter{client: dgClient}, nil
	}
}

func main() {
	log.Println("ghost-wispr: starting")

	configPath := os.Getenv(config.EnvPrefix + "CONFIG")
	if configPath == "" {
		configPath = "ghost-wispr.yaml"
	}

	envPath := filepath.Join(filepath.Dir(configPath), ".env")
	cfgStore, cfgWarnings, err := config.NewStoreWithEnv(configPath, envPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg := cfgStore.Get()
	logSink := logging.NewLogSink(1000)
	appLogger := logging.New(io.MultiWriter(os.Stdout, logSink), cfg.LogLevel)
	slog.SetDefault(appLogger)
	for _, w := range cfgWarnings {
		log.Printf("config: %s", w)
	}

	// Startup validation: check component readiness from config.
	startupStatus := config.ValidateStartup(cfg)

	store, err := storage.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("storage init failed: %v", err)
	}

	// Recover any sessions left as 'active' from a previous crash/restart.
	recoveredIDs, err := store.RecoverStaleSessions()
	if err != nil {
		log.Printf("warning: failed to recover stale sessions: %v", err)
	} else if len(recoveredIDs) > 0 {
		log.Printf("recovered %d stale session(s): %v", len(recoveredIDs), recoveredIDs)
	}

	// Backfill sessions with empty titles.
	emptyTitleSessions, err := store.GetSessionsWithEmptyTitles()
	if err != nil {
		log.Printf("warning: failed to query sessions with empty titles: %v", err)
	} else if len(emptyTitleSessions) > 0 {
		for _, sess := range emptyTitleSessions {
			segments, _ := store.GetSegments(sess.ID)
			var transcript string
			for _, seg := range segments {
				if strings.TrimSpace(seg.Text) != "" {
					transcript += seg.Text + "\n"
				}
			}
			title := summary.GenerateTitle("", transcript, segments, sess.StartedAt)
			if err := store.UpdateTitle(sess.ID, title); err != nil {
				log.Printf("warning: failed to backfill title for session %s: %v", sess.ID, err)
			}
		}
		log.Printf("backfilled %d session(s) with empty titles", len(emptyTitleSessions))
	}

	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static assets init failed: %v", err)
	}

	hub := server.NewHub(appLogger)
	detector := session.NewDetector(cfg.ParsedSilenceTimeout())
	audioRecorder := audio.NewRecorder(cfg.AudioDir)

	clientFactory := func(provider, model string) (llm.Client, error) {
		latestCfg := cfgStore.Get()
		keys := map[string]string{
			"openai":    latestCfg.OpenAIAPIKey,
			"anthropic": latestCfg.AnthropicAPIKey,
			"gemini":    latestCfg.GeminiAPIKey,
		}
		key := keys[provider]
		if key == "" {
			return nil, fmt.Errorf("no API key for provider %q", provider)
		}
		var opts []llm.Option
		if provider == "openai" && latestCfg.Summarization.BaseURL != "" {
			opts = append(opts, llm.WithBaseURL(latestCfg.Summarization.BaseURL))
		}
		return llm.NewClient(provider, key, model, opts...)
	}

	apiKeys := map[string]string{
		"openai":    cfg.OpenAIAPIKey,
		"anthropic": cfg.AnthropicAPIKey,
		"gemini":    cfg.GeminiAPIKey,
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

	manager := session.NewManager(store, audioRecorder, sessionSummarizer, hub, detector, cfg.MinSessionSegments, appLogger)

	// Summarize recovered sessions (and any with pending/empty summaries).
	// Launched after ctx is created so SIGTERM cancels in-flight LLM calls.
	var startupSummarizer = summarizer // capture for goroutine below

	recState := &recorderState{}
	warnings := append([]string{}, cfgWarnings...)
	if batchTranscriber, err := makeBatchTranscriber(cfg); err != nil {
		log.Printf("warning: batch transcription disabled: %v", err)
		warnings = append(warnings, "Batch transcription unavailable — using streaming transcript fallback")
	} else {
		manager.SetBatchTranscriber(batchTranscriber)
	}
	authToken := os.Getenv("GHOST_WISPR_AUTH_TOKEN")

	server.SetVersionInfo(server.VersionInfo{Version: Version, Commit: Commit, BuildTime: BuildTime})
	var syncOrchestrator *gdrive.Orchestrator

	controlHooks := &server.ControlHooks{
		Pause:         recState.Pause,
		Resume:        recState.Resume,
		IsPaused:      recState.IsPaused,
		ActiveSession: manager.ActiveSession,
		OnStatusChanged: func(paused bool) {
			hub.BroadcastStatusChanged(paused)
		},
		Warnings: func() []string { return warnings },
		GetLogs:  logSink.GetLogs,
		RetrySync: func(ctx context.Context, sessionID string) error {
			if syncOrchestrator == nil {
				return fmt.Errorf("gdrive sync not configured")
			}
			return syncOrchestrator.SyncSession(ctx, sessionID)
		},
		RetryRefinement: func(ctx context.Context, sessionID string) error {
			// Placeholder for T10 batch refinement
			return nil
		},
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

			transcript := buildCanonicalTranscript(store, sessionID, segments)

			_ = store.UpdateSummary(sessionID, "", "", storage.SummaryRunning, "")
			hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryRunning, "")

			var title string
			var summaryText string
			var presetUsed string
			if preset != "" {
				presetUsed = preset
				title, summaryText, err = summarizer.SummarizeWithPreset(ctx, sessionID, transcript, preset)
			} else {
				title, summaryText, presetUsed, err = summarizer.Summarize(ctx, sessionID, transcript)
			}

			status := storage.SummaryCompleted
			if err != nil {
				status = storage.SummaryFailed
			}
			_ = store.UpdateSummary(sessionID, title, summaryText, status, presetUsed)
			hub.BroadcastSummaryReady(sessionID, title, summaryText, status, presetUsed)
			return err
		},
		OnSessionMerged: func(ctx context.Context, sessionID string) {
			if summarizer == nil {
				return
			}

			segments, err := store.GetSegments(sessionID)
			if err != nil {
				log.Printf("warning: failed to get segments for merged session %s: %v", sessionID, err)
				return
			}

			transcript := buildCanonicalTranscript(store, sessionID, segments)
			if transcript == "" {
				_ = store.UpdateSummary(sessionID, "", "", storage.SummaryCompleted, "")
				return
			}

			_ = store.UpdateSummary(sessionID, "", "", storage.SummaryRunning, "")
			hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryRunning, "")

			title, summaryText, preset, err := summarizer.Summarize(ctx, sessionID, transcript)
			if err != nil {
				log.Printf("warning: summarization failed for merged session %s: %v", sessionID, err)
				_ = store.UpdateSummary(sessionID, "", "", storage.SummaryFailed, preset)
				hub.BroadcastSummaryReady(sessionID, "", "", storage.SummaryFailed, preset)
				return
			}

			_ = store.UpdateSummary(sessionID, title, summaryText, storage.SummaryCompleted, preset)
			hub.BroadcastSummaryReady(sessionID, title, summaryText, storage.SummaryCompleted, preset)
		},
		EndSession: func(ctx context.Context) error {
			return manager.ForceEndSession(ctx)
		},
		StartSession: func(ctx context.Context, titleHint string) (string, error) {
			return manager.ManualStartSession(ctx, titleHint)
		},
		StopSession: func(ctx context.Context, sessionID string) error {
			return manager.StopSession(ctx, sessionID)
		},
		TestPreset: func(ctx context.Context, presetName, sessionID string) (string, error) {
			latestCfg := cfgStore.Get()
			preset, ok := latestCfg.Summarization.Presets[presetName]
			if !ok {
				return "", fmt.Errorf("preset %q not found", presetName)
			}

			segments, err := store.GetSegments(sessionID)
			if err != nil {
				return "", err
			}

			transcript := buildCanonicalTranscript(store, sessionID, segments)

			modelStr := preset.Model
			if modelStr == "" {
				modelStr = latestCfg.Summarization.Model
			}

			provider, model, err := llm.ParseModel(modelStr)
			if err != nil {
				return "", err
			}

			client, err := clientFactory(provider, model)
			if err != nil {
				return "", err
			}

			userContent := strings.ReplaceAll(preset.UserTemplate, "{{transcript}}", transcript)
			msgs := []llm.Message{
				{Role: "system", Content: preset.SystemPrompt},
				{Role: "user", Content: userContent},
			}

			return client.Complete(ctx, msgs)
		},
		GeneratePreset: func(ctx context.Context, description string) (config.Preset, error) {
			latestCfg := cfgStore.Get()
			modelStr := latestCfg.Summarization.Model
			provider, model, err := llm.ParseModel(modelStr)
			if err != nil {
				return config.Preset{}, fmt.Errorf("no summarization model configured")
			}

			client, err := clientFactory(provider, model)
			if err != nil {
				return config.Preset{}, err
			}

			prompt := `You are designing a transcript summarization preset for a meeting transcription tool.
The user will describe what kind of summary they want. Generate a JSON object with exactly these fields:
- "system_prompt": Instructions for the summarizer (what to extract, how to format, tone)
- "user_template": The template that wraps the transcript. MUST contain {{transcript}} as a placeholder.

Return ONLY valid JSON, no markdown fences, no explanation.`

			userMsg := fmt.Sprintf("Generate a summarization preset for: %s", description)
			msgs := []llm.Message{
				{Role: "system", Content: prompt},
				{Role: "user", Content: userMsg},
			}

			result, err := client.Complete(ctx, msgs)
			if err != nil {
				return config.Preset{}, err
			}

			// Extract JSON from response (handles markdown fences and preamble).
			result = strings.TrimSpace(result)
			if start := strings.Index(result, "{"); start != -1 {
				if end := strings.LastIndex(result, "}"); end > start {
					result = result[start : end+1]
				}
			}

			var generated struct {
				SystemPrompt string `json:"system_prompt"`
				UserTemplate string `json:"user_template"`
			}
			if err := json.Unmarshal([]byte(result), &generated); err != nil {
				return config.Preset{}, fmt.Errorf("LLM returned invalid JSON: %w", err)
			}

			return config.Preset{
				Description:  description,
				SystemPrompt: generated.SystemPrompt,
				UserTemplate: generated.UserTemplate,
			}, nil
		},
		RefinePreset: func(ctx context.Context, current config.Preset, feedback string) (config.Preset, error) {
			latestCfg := cfgStore.Get()
			modelStr := latestCfg.Summarization.Model
			provider, model, err := llm.ParseModel(modelStr)
			if err != nil {
				return config.Preset{}, fmt.Errorf("no summarization model configured")
			}

			client, err := clientFactory(provider, model)
			if err != nil {
				return config.Preset{}, err
			}

			prompt := `You are refining a transcript summarization preset for a meeting transcription tool.
The user will provide their current preset configuration and feedback about what to change.
Return a revised JSON object with exactly these fields:
- "description": Updated description of what this preset does
- "system_prompt": Revised instructions for the summarizer
- "user_template": Revised template that wraps the transcript. MUST contain {{transcript}} as a placeholder.

Return ONLY valid JSON, no markdown fences, no explanation.`

			userMsg := fmt.Sprintf(`Current preset:
- Description: %s
- System Prompt: %s
- User Template: %s

User feedback: %s`, current.Description, current.SystemPrompt, current.UserTemplate, feedback)

			msgs := []llm.Message{
				{Role: "system", Content: prompt},
				{Role: "user", Content: userMsg},
			}

			result, err := client.Complete(ctx, msgs)
			if err != nil {
				return config.Preset{}, err
			}

			// Extract JSON from response (handles markdown fences and preamble).
			result = strings.TrimSpace(result)
			if start := strings.Index(result, "{"); start != -1 {
				if end := strings.LastIndex(result, "}"); end > start {
					result = result[start : end+1]
				}
			}

			var refined struct {
				Description  string `json:"description"`
				SystemPrompt string `json:"system_prompt"`
				UserTemplate string `json:"user_template"`
			}
			if err := json.Unmarshal([]byte(result), &refined); err != nil {
				return config.Preset{}, fmt.Errorf("LLM returned invalid JSON: %w", err)
			}

			return config.Preset{
				Description:  refined.Description,
				SystemPrompt: refined.SystemPrompt,
				UserTemplate: refined.UserTemplate,
				Model:        current.Model,
			}, nil
		},
		RestoreFromGDrive: func(ctx context.Context) (map[string]any, error) {
			latestCfg := cfgStore.Get()
			if latestCfg.GDriveFolderID == "" {
				return nil, fmt.Errorf("google drive folder ID not configured")
			}
			syncer, err := gdrive.NewSyncer(ctx, latestCfg.GoogleCredentialsFile, latestCfg.GDriveFolderID)
			if err != nil {
				return nil, fmt.Errorf("create syncer: %w", err)
			}
			result, err := syncer.RestoreFromDrive(ctx, func(rs gdrive.RestoredSession) error {
				summaryStatus := storage.SummaryPending
				if rs.Summary != "" {
					summaryStatus = storage.SummaryCompleted
				}
				sess := storage.Session{
					ID:             rs.ID,
					Title:          rs.Title,
					StartedAt:      rs.StartedAt,
					EndedAt:        rs.EndedAt,
					Status:         storage.SessionEnded,
					Summary:        rs.Summary,
					SummaryStatus:  summaryStatus,
					SummaryPreset:  rs.SummaryPreset,
					SyncStatus:     storage.SyncSynced,
					GDriveFolderID: rs.DriveFolderID,
				}
				return store.ImportSession(&sess, rs.Segments)
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"restored": result.Restored,
				"skipped":  result.Skipped,
				"errors":   result.Errors,
			}, nil
		},
	}
	handler, err := server.HandlerWithLogger(assets, hub, store, controlHooks, authToken, nil, appLogger, cfgStore)
	if err != nil {
		log.Fatalf("build http handler failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register config change callback.
	cfgStore.OnChange(func(newCfg config.Config) {
		appLogger = logging.New(os.Stdout, newCfg.LogLevel)
		slog.SetDefault(appLogger)

		detector.SetTimeout(newCfg.ParsedSilenceTimeout())
		manager.SetMinSessionSegments(newCfg.MinSessionSegments)
		log.Printf("config: reloaded (silence_timeout=%s)", newCfg.SilenceTimeout)

		// Recreate summarizer if API keys or model changed.
		newAPIKeys := map[string]string{
			"openai":    newCfg.OpenAIAPIKey,
			"anthropic": newCfg.AnthropicAPIKey,
			"gemini":    newCfg.GeminiAPIKey,
		}
		if provider, _, err := llm.ParseModel(newCfg.Summarization.Model); err == nil && newAPIKeys[provider] != "" {
			newSummarizer := summary.New(newCfg.Summarization, clientFactory)
			manager.SetSummarizer(newSummarizer)
			log.Printf("config: summarizer updated for provider %s", provider)
		}

		if batchTranscriber, err := makeBatchTranscriber(newCfg); err != nil {
			manager.SetBatchTranscriber(nil)
			log.Printf("config: batch transcriber disabled: %v", err)
		} else {
			manager.SetBatchTranscriber(batchTranscriber)
			log.Printf("config: batch transcriber updated for provider %s", newCfg.BatchTranscription.Provider)
		}

		if newCfg.GDriveSyncEnabled && newCfg.GDriveFolderID != "" && syncOrchestrator == nil {
			if syncer, err := gdrive.NewSyncer(ctx, newCfg.GoogleCredentialsFile, newCfg.GDriveFolderID, appLogger); err == nil {
				syncOrchestrator = gdrive.NewOrchestrator(syncer, store, appLogger)
				manager.SetSyncer(syncOrchestrator)
				log.Printf("config: gdrive sync enabled")
			}
		} else if !newCfg.GDriveSyncEnabled && syncOrchestrator != nil {
			manager.SetSyncer(nil)
			syncOrchestrator = nil
			log.Printf("config: gdrive sync disabled")
		}
	})

	// Summarize recovered sessions (and any with pending/empty summaries).
	if startupSummarizer != nil {
		go func() {
			pendingIDs, err := store.GetSessionsNeedingSummary()
			if err != nil {
				log.Printf("warning: failed to query sessions needing summary: %v", err)
				return
			}
			for _, id := range pendingIDs {
				log.Printf("summarizing session %s", id)
				segments, err := store.GetSegments(id)
				if err != nil {
					log.Printf("warning: failed to get segments for %s: %v", id, err)
					continue
				}
				transcript := buildCanonicalTranscript(store, id, segments)
				if transcript == "" {
					_ = store.UpdateSummary(id, "", "", storage.SummaryCompleted, "")
					continue
				}
				_ = store.UpdateSummary(id, "", "", storage.SummaryRunning, "")
				title, summaryText, preset, err := startupSummarizer.Summarize(ctx, id, transcript)
				if err != nil {
					log.Printf("warning: summarization failed for %s: %v", id, err)
					_ = store.UpdateSummary(id, "", "", storage.SummaryFailed, preset)
					continue
				}
				_ = store.UpdateSummary(id, title, summaryText, storage.SummaryCompleted, preset)
				log.Printf("summarized session %s with preset %s", id, preset)
			}
		}()
	}
	defer func() { _ = store.Close() }()

	if cfg.GDriveFolderID != "" && cfg.GDriveSyncEnabled {
		syncer, syncErr := gdrive.NewSyncer(ctx, cfg.GoogleCredentialsFile, cfg.GDriveFolderID, appLogger)
		if syncErr != nil {
			log.Printf("warning: gdrive sync disabled: %v", syncErr)
			warnings = append(warnings, "Google Drive sync failed to initialize — backups are disabled")
		} else {
			syncOrchestrator = gdrive.NewOrchestrator(syncer, store, appLogger)
			manager.SetSyncer(syncOrchestrator)

			go func() {
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						ids, err := store.GetSessionsNeedingSync()
						if err != nil {
							log.Printf("gdrive sweep error: %v", err)
							continue
						}

						retryScheduled, err := store.GetSessionsBySyncState(storage.SyncStateRetryScheduled)
						if err != nil {
							log.Printf("gdrive retry sweep error: %v", err)
							continue
						}

						now := time.Now().UTC()
						seen := make(map[string]struct{}, len(ids)+len(retryScheduled))
						for _, id := range ids {
							seen[id] = struct{}{}
						}

						for _, sess := range retryScheduled {
							if !gdrive.IsRetryReady(now, sess.RetryCount, sess.LastSyncAttempt) {
								continue
							}
							if _, ok := seen[sess.ID]; ok {
								continue
							}
							ids = append(ids, sess.ID)
							seen[sess.ID] = struct{}{}
						}

						for _, id := range ids {
							if err := syncOrchestrator.SyncSession(ctx, id); err != nil {
								log.Printf("gdrive sweep: session %s: %v", id, err)
							}
						}
					}
				}
			}()
		}
	}

	if cfg.GCEnabled {
		go func() {
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					latestCfg := cfgStore.Get()
					if !latestCfg.GCEnabled {
						continue
					}
					syncGated := latestCfg.GDriveSyncEnabled && syncOrchestrator != nil
					collector := gc.New(store, gc.Config{
						MaxAgeDays:     latestCfg.GCMaxAgeDays,
						MaxAudioSizeMB: latestCfg.GCMaxAudioSizeMB,
						SyncGated:      syncGated,
						AudioDir:       latestCfg.AudioDir,
					}, appLogger)
					deleted, err := collector.Run()
					if err != nil {
						log.Printf("gc error: %v", err)
					} else if len(deleted) > 0 {
						log.Printf("gc: deleted %d sessions", len(deleted))
					}
				}
			}
		}()
	}

	var mic *audio.Mic
	var dgWriter io.Writer
	var dgStop func()
	selectedSampleRate := cfg.MicSampleRate

	paErr := portaudio.Initialize()
	//nolint:errcheck // Terminate is best-effort cleanup
	defer portaudio.Terminate()
	paInitFailed := false
	if paErr != nil {
		log.Printf("warning: portaudio init failed: %v — microphone unavailable", paErr)
		paInitFailed = true
	}

	client.Init(client.InitLib{LogLevel: client.LogLevelDefault})

	if !paInitFailed {
		for _, rate := range cfg.SampleRateCandidates() {
			mic, err = audio.NewMic(rate, rate/4) // 250ms buffer
			if err != nil {
				log.Printf("warning: microphone open failed at %d Hz: %v", rate, err)
				continue
			}
			selectedSampleRate = rate
			break
		}
	}

	if mic == nil {
		slog.Warn("microphone unavailable, running API/UI only")
		warnings = append(warnings, "Microphone unavailable \u2014 recording and live transcription are disabled")
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "Microphone unavailable"}
	} else {
		audioRecorder.SetSampleRate(selectedSampleRate)
		recState.SetMic(mic)
		if err := mic.Start(); err != nil {
			slog.Warn("microphone start failed, running API/UI only", "sample_rate", selectedSampleRate, "error", err)
			mic = nil
			recState.SetMic(nil)
			warnings = append(warnings, "Microphone failed to start \u2014 recording and live transcription are disabled")
			startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "Microphone failed to start"}
		} else {
			log.Printf("microphone started at %d Hz", selectedSampleRate)
			startupStatus.Mic = config.ComponentState{Name: "mic", Status: "connected", Message: fmt.Sprintf("Microphone open at %d Hz", selectedSampleRate)}
		}
	}

	// Wire up mic diagnostic hook now that mic is initialized.
	controlHooks.DiagnoseMic = func(ctx context.Context) (map[string]any, error) {
		if mic == nil {
			return nil, fmt.Errorf("microphone not available")
		}
		return map[string]any{
			"mic_open":    mic.IsOpen(),
			"sample_rate": selectedSampleRate,
			"status":      "operational",
			"message":     fmt.Sprintf("Microphone open at %d Hz", selectedSampleRate),
		}, nil
	}

	if mic != nil && cfg.DeepgramAPIKey != "" {
		cOptions := &interfaces.ClientOptions{EnableKeepAlive: true}
		tOptions := &interfaces.LiveTranscriptionOptions{
			Model:          cfg.DeepgramModel,
			Language:       "en-US",
			Diarize:        true,
			Punctuate:      true,
			SmartFormat:    true,
			Encoding:       "linear16",
			SampleRate:     selectedSampleRate,
			Channels:       1,
			Endpointing:    cfg.Transcription.Endpointing,
			InterimResults: true,
			UtteranceEndMs: cfg.Transcription.UtteranceEndMs,
			VadEvents:      true,
		}

		callback := transcriptCallback{manager: manager}
		factory := makeDeepgramClientFactory(ctx, cfg.DeepgramAPIKey, cOptions, tOptions, callback)

		resilientConfig := transcribe.ResilientConfig{
			BufferSize:            cfg.DeepgramBufferSize,
			InitialReconnectDelay: cfg.ParsedDeepgramReconnectInitialDelay(),
			MaxReconnectBackoff:   cfg.ParsedDeepgramReconnectMaxBackoff(),
		}

		resillientStateCallback := func(state transcribe.ConnectionState) {
			hub.BroadcastComponentStatus("deepgram", state.String(), fmt.Sprintf("Deepgram %s", state.String()))
		}
		resilientClient := transcribe.NewResilientClient(ctx, factory, resilientConfig, log.Printf, resillientStateCallback)
		callback.resilient = resilientClient
		controlHooks.FaultDeepgramDisconnect = resilientClient.Close

		initialClient, err := factory(ctx)
		if err != nil {
			log.Printf("warning: deepgram client unavailable, running API/UI only: %v", err)
			warnings = append(warnings, "Deepgram initialization failed — live transcription is disabled")
		} else {
			resilientClient.Client = initialClient
			dgWriter = resilientClient
			dgStop = func() {
				if err := resilientClient.Close(); err != nil {
					log.Printf("warning: close resilient deepgram client failed: %v", err)
				}
			}
			go func() {
				streamMicWithRetry(ctx, mic, audioRecorder.Writer(dgWriter), time.Sleep, log.Printf)
			}()
		}
	}

	addr := os.Getenv("GHOST_WISPR_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	httpServer := &http.Server{Addr: addr, Handler: handler}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server error: %v", err)
		}
	}()

	// Log structured startup banner.
	config.LogStartupBanner(appLogger, startupStatus, Version, Commit, BuildTime)
	slog.Info("deepgram API key", "status", config.MaskAPIKey(cfg.DeepgramAPIKey))
	log.Printf("ghost-wispr %s (commit=%s built=%s): web UI on http://127.0.0.1%s", Version, Commit, BuildTime, addr)

	// Broadcast initial component status to any connected WebSocket clients.
	for _, c := range startupStatus.Components() {
		hub.BroadcastComponentStatus(c.Name, c.Status, c.Message)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("ghost-wispr: shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	Reopen() error
}

func streamMicWithRetry(
	ctx context.Context,
	streamer micStreamer,
	writer io.Writer,
	wait func(time.Duration),
	logf func(string, ...any),
) {
	const (
		overflowWait = 250 * time.Millisecond
		baseBackoff  = time.Second
		maxBackoff   = 30 * time.Second
	)

	backoff := baseBackoff

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
			wait(overflowWait)
			continue
		}

		logf("mic stream error (retrying in %v): %v", backoff, err)
		wait(backoff)

		if ctx.Err() != nil {
			return
		}

		if reopenErr := streamer.Reopen(); reopenErr != nil {
			logf("mic reopen failed: %v", reopenErr)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		logf("mic reopened successfully")
		backoff = baseBackoff
	}
}
