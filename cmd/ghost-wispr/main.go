package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/auth"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/tts"

	driveapi "google.golang.org/api/drive/v3"
	"google.golang.org/genai"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

const defaultListenAddress = "127.0.0.1:8080"

var vertexCredentialCache sync.Map

//go:embed static/*
var staticFiles embed.FS

func retryRefinementAfterRestoringRecording(ctx context.Context, sessionID string, restore func(context.Context, string) error, refine func(context.Context, string) error) error {
	if err := restore(ctx, sessionID); err != nil {
		return err
	}
	return refine(ctx, sessionID)
}

type refinementAudioRestoreStore interface {
	RestoreAudioForRefinement(id, beforeAudioPath, restoredAudioPath string) (bool, error)
	DriveRestoreAllowed(sessionID string) (bool, error)
}

func hasLocalRefinementRecording(session *storage.Session, audioDir string) (bool, error) {
	if session.AudioRemovedAt != "" {
		return false, nil
	}
	entries := 0
	for _, entry := range strings.Split(session.AudioPath, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entries++
		path := recording.Resolve(entry, audioDir)
		info, err := os.Stat(path)
		switch {
		case err == nil && !info.IsDir():
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return false, fmt.Errorf("check recording %s: %w", path, err)
		default:
			return false, nil
		}
	}
	return entries > 0, nil
}

func refinementRecordingAvailable(session *storage.Session, audioDir string) error {
	local, err := hasLocalRefinementRecording(session, audioDir)
	if err != nil {
		return err
	}
	if local {
		return nil
	}
	if session.AudioRemovedAt == "" {
		return errors.New("no recording is available on this unit or Drive")
	}
	if strings.TrimSpace(session.GDriveFolderID) == "" {
		return errors.New("the recording was removed from this unit, but no Drive folder is listed")
	}
	if session.SyncState == storage.SyncStateRemoteDeleted {
		return errors.New("the recording was removed from this unit and its Drive folder is deleted")
	}
	if session.SyncStatus != storage.SyncSynced {
		return errors.New("the recording was removed from this unit and its Drive copy is not synced")
	}
	return nil
}

func restoreRefinementRecording(ctx context.Context, store refinementAudioRestoreStore, session *storage.Session, audioDir string, restore func(context.Context, storage.Session) (deletionRecordingSet, error)) error {
	local, err := hasLocalRefinementRecording(session, audioDir)
	if err != nil {
		return err
	}
	if local {
		return nil
	}
	allowed, err := store.DriveRestoreAllowed(session.ID)
	if err != nil {
		return fmt.Errorf("check whether Drive restoration is allowed for session %s: %w", session.ID, err)
	}
	if !allowed {
		return fmt.Errorf("drive restoration is blocked while session %s owes a replacement", session.ID)
	}
	recordings, err := restore(ctx, *session)
	if err != nil {
		return fmt.Errorf("restore recording for refinement: %w", err)
	}
	if len(recordings.Paths) == 0 {
		return errors.New("no recording is available on this unit or Drive")
	}
	restoredAudioPath := strings.Join(recordings.Paths, ",")
	if restoredAudioPath == session.AudioPath {
		if session.AudioRemovedAt == "" {
			return nil
		}
		return errors.New("drive did not restore every recording removed from this unit")
	}
	restored, err := store.RestoreAudioForRefinement(session.ID, session.AudioPath, restoredAudioPath)
	if err != nil {
		return fmt.Errorf("record restored audio for refinement: %w", err)
	}
	if !restored {
		return fmt.Errorf("recording changed while restoring it for refinement")
	}
	return nil
}

const refinementRetryWarningKeyPrefix = "retry-refinement-"

func retryRefinementWithFailureWarning(ctx context.Context, registry *warningsRegistry, sessionID string, retry func(context.Context, string) error) error {
	err := retry(ctx, sessionID)
	if err == nil {
		registry.Clear(refinementRetryWarningKeyPrefix + sessionID)
		return nil
	}
	warning := fmt.Sprintf("Retry refinement failed for session %s: %v", sessionID, err)
	log.Printf("warning: %s", warning)
	registry.Set(refinementRetryWarningKeyPrefix+sessionID, warning)
	return err
}

func newRetryRefinementHook(store *storage.SQLiteStore, audioDir func() string, restore func(context.Context, storage.Session) (deletionRecordingSet, error), registry *warningsRegistry, refine func(context.Context, string, string, time.Time) error) func(context.Context, string) error {
	ensure := func(ctx context.Context, sessionID string) error {
		session, err := store.GetSession(sessionID)
		if err != nil {
			return err
		}
		return restoreRefinementRecording(ctx, store, &session, audioDir(), restore)
	}
	return func(ctx context.Context, sessionID string) error {
		return retryRefinementWithFailureWarning(ctx, registry, sessionID, func(ctx context.Context, sessionID string) error {
			return retryRefinementAfterRestoringRecording(ctx, sessionID, ensure, func(ctx context.Context, sessionID string) error {
				return retryRefinementWithWarningClear(ctx, registry, store, sessionID, refine)
			})
		})
	}
}

func restoredDriveSession(rs *gdrive.RestoredSession, restoredAt time.Time) storage.Session {
	summaryStatus := storage.SummaryPending
	if rs.Summary != "" {
		summaryStatus = storage.SummaryCompleted
	}
	return storage.Session{
		ID:               rs.ID,
		Title:            rs.Title,
		StartedAt:        rs.StartedAt,
		EndedAt:          rs.EndedAt,
		Status:           storage.SessionEnded,
		Summary:          rs.Summary,
		SummaryStatus:    summaryStatus,
		SummaryPreset:    rs.SummaryPreset,
		RefinementStatus: storage.RefinementNoAudio,
		AudioRemovedAt:   restoredAt.UTC().Format(time.RFC3339Nano),
		SyncStatus:       storage.SyncSynced,
		GDriveFolderID:   rs.DriveFolderID,
	}
}

func buildTranscript(segments []transcribe.Segment) string {
	var b strings.Builder
	for i := range segments {
		if strings.TrimSpace(segments[i].Text) != "" {
			b.WriteString(segments[i].Text)
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
	if status == storage.RefinementCompleted && strings.TrimSpace(refined) != "" {
		return refined
	}

	return streamingTranscript
}

func genAIOptions(cfg *config.Config, apiKey string) *genaiconfig.Options {
	return &genaiconfig.Options{
		Backend:  cfg.GenAIBackend,
		Project:  cfg.GCPProject,
		Location: cfg.GCPLocation,
		APIKey:   apiKey,
	}
}

func addVertexCredentials(ctx context.Context, cfg *config.Config, options *genaiconfig.Options) error {
	if genaiconfig.ResolveBackend(options.Backend, options.Project) != genaiconfig.BackendVertex {
		return nil
	}
	credentialPath := cfg.GoogleCredentialsFile
	if cached, ok := vertexCredentialCache.Load(credentialPath); ok {
		options.Credentials = cached.(*auth.Credentials)
		return nil
	}
	credentials, err := googleauth.Detect(ctx, credentialPath, googleauth.CloudPlatformScope)
	if err != nil {
		return fmt.Errorf("detect Google credentials: %w", err)
	}
	cached, _ := vertexCredentialCache.LoadOrStore(credentialPath, credentials)
	options.Credentials = cached.(*auth.Credentials)
	return nil
}

func providerConfigured(cfg *config.Config, provider string) bool {
	switch provider {
	case "openai":
		return cfg.OpenAIAPIKey != ""
	case "anthropic":
		return cfg.AnthropicAPIKey != ""
	case "gemini":
		return genaiconfig.CanUse(genAIOptions(cfg, cfg.GeminiAPIKey))
	default:
		return false
	}
}

type repairSpeakerMetadata struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"`
}

func parseStoredStructuredSummary(rawSummary string) (title, summaryText, speakerNames string, err error) {
	var payload struct {
		Title    string          `json:"title"`
		Summary  string          `json:"summary"`
		Speakers json.RawMessage `json:"speakers,omitempty"`
	}
	if err := json.Unmarshal([]byte(rawSummary), &payload); err != nil {
		return "", "", "", err
	}

	speakers, err := parseStoredSummarySpeakers(payload.Speakers)
	if err != nil {
		return "", "", "", err
	}
	if len(speakers) == 0 {
		return strings.TrimSpace(payload.Title), strings.TrimSpace(payload.Summary), "{}", nil
	}

	encodedSpeakers, err := json.Marshal(speakers)
	if err != nil {
		return strings.TrimSpace(payload.Title), strings.TrimSpace(payload.Summary), "{}", nil
	}

	return strings.TrimSpace(payload.Title), strings.TrimSpace(payload.Summary), string(encodedSpeakers), nil
}

func parseStoredSummarySpeakers(raw json.RawMessage) (map[string]repairSpeakerMetadata, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	var speakers map[string]repairSpeakerMetadata
	if err := json.Unmarshal(raw, &speakers); err == nil {
		return normalizeRepairSpeakers(speakers), nil
	}

	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	encoded = strings.TrimSpace(encoded)
	if encoded == "" || encoded == "null" {
		return nil, nil
	}

	if err := json.Unmarshal([]byte(encoded), &speakers); err != nil {
		return nil, err
	}
	return normalizeRepairSpeakers(speakers), nil
}

func normalizeRepairSpeakers(speakers map[string]repairSpeakerMetadata) map[string]repairSpeakerMetadata {
	if len(speakers) == 0 {
		return nil
	}
	normalized := make(map[string]repairSpeakerMetadata, len(speakers))
	for speakerID, meta := range speakers {
		meta.Name = strings.TrimSpace(meta.Name)
		meta.Confidence = strings.TrimSpace(meta.Confidence)
		if meta.Name == "" {
			continue
		}
		if meta.Confidence != "mentioned" && meta.Confidence != "inferred" {
			meta.Confidence = "inferred"
		}
		normalized[speakerID] = meta
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// errDeepgramKeyMissing is why the default batch transcription, Deepgram's,
// cannot run: no Deepgram API key is configured.
var errDeepgramKeyMissing = errors.New("deepgram API key not configured")

func makeBatchTranscriber(cfg *config.Config) (transcribe.BatchTranscriber, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.BatchTranscription.Provider)) {
	case "", "deepgram":
		if strings.TrimSpace(cfg.DeepgramAPIKey) == "" {
			return nil, errDeepgramKeyMissing
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

type micProxy struct{ ref *atomic.Pointer[audio.Mic] }

func (p *micProxy) IsOpen() bool {
	mic := p.ref.Load()
	return mic != nil && mic.IsOpen()
}

type resilientClientProxy struct {
	ref *atomic.Pointer[transcribe.ResilientClient]
}

func (p *resilientClientProxy) IsConnected() bool {
	client := p.ref.Load()
	return client != nil && client.IsConnected()
}

// micRetryEvery is how long the microphone start-up waits before it tries
// again, after an attempt failed or PortAudio failed to initialize.
const micRetryEvery = 15 * time.Second

// resumeWaiter is the part of the capture gate a microphone start waits on:
// waitResumed blocks until the gate is open, and finishResume ends a resume's
// "resuming" state once the start that brings capture back has finished.
type resumeWaiter interface {
	waitResumed(ctx context.Context) bool
	finishResume()
}

// retryMicStartup runs start every retryInterval until it succeeds or ctx
// ends. A paused instance opens no input device: a retry that comes due
// while capture is paused waits (capture.waitResumed) until capture resumes.
func retryMicStartup(
	ctx context.Context,
	retryInterval time.Duration,
	after func(time.Duration) <-chan time.Time,
	capture resumeWaiter,
	onRetry func(),
	start func() error,
	onError func(error),
	onSuccess func(),
) {
	if after == nil {
		after = time.After
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-after(retryInterval):
		}
		if !capture.waitResumed(ctx) {
			return
		}

		if onRetry != nil {
			onRetry()
		}
		// Shutdown can be ready together with the tick (select picks either) or
		// land just after it: start re-initializes PortAudio and starts a device,
		// so it must not run once shutdown has begun.
		if ctx.Err() != nil {
			return
		}

		// The attempt, whether it works or not, ends a resume's "resuming"
		// state: readiness then reports what came back.
		err := start()
		capture.finishResume()
		if err != nil {
			if onError != nil {
				onError(err)
			}
			continue
		}

		if onSuccess != nil {
			onSuccess()
		}
		return
	}
}

// startMicrophone opens the input device and starts capture with start, and
// runs retry in loops if start fails or PortAudio is not ready. A paused
// instance opens no input device until it resumes: while the capture gate is
// closed, start runs in loops once the gate opens, with atBoot false, and
// startMicrophone returns deferred true; that start, whether it works or not,
// ends the resume's "resuming" state (finishResume). Otherwise start runs at
// once with atBoot true, so it can write the start-up status.
//
// Nothing runs in loops before booted closes, once start-up has sent its
// component statuses: a power-button resume can come during start-up, and
// the statuses its start sends (microphone and Deepgram connected) must not be
// followed by the start-up ones (paused), which would then stand for every
// page that connects later.
func startMicrophone(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, booted <-chan struct{}, portAudioReady bool, start func(atBoot bool) error, retry func()) (deferred bool) {
	afterBoot := func(run func()) {
		loops.Go(func() {
			select {
			case <-booted:
				run()
			case <-ctx.Done():
			}
		})
	}
	if capture.isClosed() {
		afterBoot(func() {
			if !capture.waitResumed(ctx) {
				return
			}
			started := portAudioReady && start(false) == nil
			capture.finishResume()
			if started {
				return
			}
			retry()
		})
		return true
	}
	if portAudioReady && start(true) == nil {
		return false
	}
	afterBoot(retry)
	return false
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

// streamMicLoopStarter runs a mic loop in loops, the group shutdown waits for.
var streamMicLoopStarter = func(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, streamer micStreamer, writer io.Writer, wait func(time.Duration), logf func(string, ...any), hub *server.Hub) {
	loops.Go(func() { streamMicWithRetry(ctx, capture, streamer, writer, wait, logf, hub) })
}

func startMicMonitoring(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, streamer micStreamer, recorder *audio.Recorder, downstream io.Writer, wait func(time.Duration), logf func(string, ...any), hub *server.Hub) {
	if streamer == nil || recorder == nil {
		return
	}
	if downstream == nil {
		downstream = io.Discard
	}
	streamMicLoopStarter(ctx, loops, capture, streamer, recorder.Writer(downstream), wait, logf, hub)
}

// micShutdownWait is how long shutdown waits for the mic loops to end. A loop
// streaming from a working device ends at its next buffer (250 ms).
const micShutdownWait = 5 * time.Second

// joinMicLoops waits up to wait for the mic loops to end once shutdown has
// cancelled their context. It reports false if one has not: it is stuck in a
// read from a device that stopped delivering audio, and nothing may stop the
// device or terminate PortAudio under it.
func joinMicLoops(loops *sync.WaitGroup, wait time.Duration) bool {
	done := make(chan struct{})
	go func() {
		loops.Wait()
		close(done)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// gcConfig is the GC configuration for one pass. GC is always Drive-gated:
// it may remove audio only after Drive answers that it still holds the session.
// The current config decides whether a running syncer is usable. With sync off
// or no folder, it passes nil so the collector holds every recording.
func gcConfig(cfg *config.Config, drive sessionSyncer) gc.Config {
	var holdReason error
	if !cfg.GDriveSyncEnabled || cfg.GDriveFolderID == "" {
		drive = nil
		holdReason = gc.ErrDriveSyncOff
	}
	return gc.Config{
		MaxAgeDays:     cfg.GCMaxAgeDays,
		MaxAudioSizeMB: cfg.GCMaxAudioSizeMB,
		AudioDir:       cfg.AudioDir,
		HoldReason:     holdReason,
		// A nil sessionSyncer is a nil DriveVerifier.
		Drive: drive,
	}
}

// keptRawRefreshEvery is how often the count of raw recordings kept because
// they could not be saved is refreshed for GET /api/status.
const keptRawRefreshEvery = time.Minute

// refreshKeptRawRecordings keeps cache up to date with the raw recordings the
// recorder keeps because they could not be saved, every interval until ctx
// ends. GET /api/status reads the cache, so a stuck disk or a hung recording
// write cannot hang the status. The status warns once they take more than a
// tenth of the audio budget, which they are not counted against, or as soon
// as there is one when there is no budget (0): nothing removes them.
func refreshKeptRawRecordings(ctx context.Context, recorder *audio.Recorder, budgetMB func() int, cache *atomic.Pointer[server.KeptRawRecordings], interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		count, size := recorder.KeptRawRecordings()
		var warnBytes int64
		if budget, limited := gc.AudioBudgetBytes(budgetMB()); limited {
			warnBytes = budget / 10
		}
		cache.Store(&server.KeptRawRecordings{
			Count:     count,
			Bytes:     size,
			WarnBytes: warnBytes,
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (dw *deepgramWriter) Write(p []byte) (int, error) {
	return dw.client.Write(p)
}

func (dw *deepgramWriter) Close() error {
	return dw.client.Finalize()
}

// Finalize asks Deepgram to return final results for the audio it has.
func (dw *deepgramWriter) Finalize() error {
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
	configPath := configPathFromEnvironment()
	if len(os.Args) > 1 && os.Args[1] == "google-id-token" {
		cfg, _, err := config.Load(configPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		_, err = runGoogleIDTokenCommand(context.Background(), os.Args[1:], os.Stdout, cfg.GoogleCredentialsFile, googleauth.IDToken)
		if err != nil {
			log.Fatalf("google-id-token: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "seed-paused-state" {
		if len(os.Args) != 2 {
			log.Fatalf("seed-paused-state: expected no arguments")
		}
		if _, err := seedPausedState(configPath, os.Stdout); err != nil {
			log.Fatalf("seed-paused-state: %v", err)
		}
		return
	}
	if len(os.Args) > 1 {
		if exitCode := runLightCommand(os.Args[1:], os.Stdout, os.Stderr); exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}

	log.Println("ghost-wispr: starting")

	cfgStore, cfgWarnings, err := loadRuntimeConfig(configPath)
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
	startupStatus := config.ValidateStartup(&cfg)

	dbPath, err := databasePath(cfg.DBPath)
	if err != nil {
		log.Fatalf("storage init failed: %v", err)
	}
	store, err := storage.NewSQLiteStore(dbPath)
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
	// A refinement left running by the previous run stopped with it.
	if err := store.RecoverStaleRefinements(); err != nil {
		log.Printf("warning: failed to recover stale refinements: %v", err)
	}
	// So did a Drive sync: retry it in the folder it had.
	if ids, err := store.RecoverInterruptedSyncs(gdrive.MaxSyncRetries, time.Now().UTC()); err != nil {
		log.Printf("warning: failed to recover interrupted Drive syncs: %v", err)
	} else if len(ids) > 0 {
		log.Printf("recovered %d interrupted Drive sync(s): %v", len(ids), ids)
	}

	// Backfill sessions with empty titles.
	emptyTitleSessions, err := store.GetSessionsWithEmptyTitles()
	if err != nil {
		log.Printf("warning: failed to query sessions with empty titles: %v", err)
	} else if len(emptyTitleSessions) > 0 {
		for i := range emptyTitleSessions {
			sess := &emptyTitleSessions[i]
			segments, _ := store.GetSegments(sess.ID)
			var transcript string
			for j := range segments {
				if strings.TrimSpace(segments[j].Text) != "" {
					transcript += segments[j].Text + "\n"
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
	hub.SetEventStore(store)
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
		if provider == "gemini" && !providerConfigured(&latestCfg, provider) {
			return nil, fmt.Errorf("no Vertex AI project or Gemini API key for provider %q", provider)
		}
		if provider != "gemini" && key == "" {
			return nil, fmt.Errorf("no API key for provider %q", provider)
		}
		var opts []llm.Option
		if provider == "openai" && latestCfg.Summarization.BaseURL != "" {
			opts = append(opts, llm.WithBaseURL(latestCfg.Summarization.BaseURL))
		}
		if provider == "gemini" {
			genOptions := genAIOptions(&latestCfg, key)
			if err := addVertexCredentials(context.Background(), &latestCfg, genOptions); err != nil {
				return nil, err
			}
			opts = append(opts, llm.WithGenAIConfig(llm.GenAIConfig{
				Backend:     genOptions.Backend,
				Project:     genOptions.Project,
				Location:    genOptions.Location,
				Credentials: genOptions.Credentials,
			}))
		}
		return llm.NewClient(provider, key, model, opts...)
	}
	llmTracker := server.NewLLMHealthTracker()
	var summarizer *summary.Summarizer
	canSummarize := false
	if provider, _, err := llm.ParseModel(cfg.Summarization.Model); err == nil {
		if providerConfigured(&cfg, provider) {
			canSummarize = true
		}
	}
	if !canSummarize {
		for _, preset := range cfg.Summarization.Presets {
			if preset.Model == "" {
				continue
			}
			if provider, _, err := llm.ParseModel(preset.Model); err == nil {
				if providerConfigured(&cfg, provider) {
					canSummarize = true
					break
				}
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

	var embeddingClient embedding.Client
	var indexer *embedding.Indexer
	if strings.TrimSpace(cfg.EmbeddingModel) != "" {
		embeddingOptions := &genaiconfig.Options{
			Backend:  os.Getenv("GHOST_WISPR_EMBEDDING_BACKEND"),
			Project:  cfg.GCPProject,
			Location: os.Getenv("GHOST_WISPR_EMBEDDING_LOCATION"),
		}
		if embeddingOptions.Backend == "" {
			embeddingOptions.Backend = os.Getenv("GHOST_WISPR_GENAI_BACKEND")
		}
		if err := addVertexCredentials(context.Background(), &cfg, embeddingOptions); err != nil {
			log.Printf("warning: embedding indexer disabled: %v", err)
		} else if embeddingClient, err = embedding.NewClient(cfg.EmbeddingModel, embedding.WithGenAIConfig(embedding.GenAIConfig{
			Backend:     embeddingOptions.Backend,
			Project:     embeddingOptions.Project,
			Location:    embeddingOptions.Location,
			Credentials: embeddingOptions.Credentials,
		})); err != nil {
			log.Printf("warning: embedding indexer disabled: %v", err)
		} else {
			indexer = embedding.NewIndexer(embeddingClient, store, 500)
			indexer.SetModel(cfg.EmbeddingModel)
		}
	}

	manager := session.NewManager(store, audioRecorder, sessionSummarizer, hub, detector, cfg.MinSessionSegments, appLogger)
	if indexer != nil {
		manager.SetIndexer(indexer)
	}

	// Summarize recovered sessions (and any with pending/empty summaries).
	// Launched after ctx is created so SIGTERM cancels in-flight LLM calls.
	var startupSummarizer = summarizer // capture for goroutine below
	llmHealthOnResult := func(err error) {
		if err != nil {
			llmTracker.SetError(err.Error())
			return
		}
		llmTracker.SetOK()
	}
	if summarizer != nil {
		summarizer.OnResult = llmHealthOnResult
	}
	if startupSummarizer != nil {
		startupSummarizer.OnResult = llmHealthOnResult
	}

	// ctx ends at shutdown; the capture gate's pause and the mic loops stop with it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var resilientRef atomic.Pointer[transcribe.ResilientClient]
	var micRef atomic.Pointer[audio.Mic]
	micStatus := &micProxy{ref: &micRef}
	// capture is the one pause switch: POST /api/pause and /api/resume (and any
	// later hardware button) call its Pause and Resume.
	capture := newSessionCaptureGate(ctx, manager, audioRecorder, func() deepgramFinalizer {
		if dg := resilientRef.Load(); dg != nil {
			return dg
		}
		return nil
	}, captureFinalizeWait, captureEndRetry, log.Printf)

	restoredPause, err := restorePause(store, capture)
	var restorePauseErr error
	if err != nil {
		// The recorder must not start capturing when it cannot establish the
		// prior consent state.
		restorePauseErr = err
		log.Printf("warning: restore capture pause: %v; starting paused", err)
		capture.Pause()
		restoredPause = true
	}
	warningRegistry := newWarningsRegistry()
	for i, warning := range cfgWarnings {
		warningRegistry.Set(fmt.Sprintf("config-%03d", i), warning)
	}
	if err := refreshRawEraseReservationWarnings(store, warningRegistry); err != nil {
		log.Printf("warning: raw erase reservations: %v", err)
	}
	startCertificateExpiryChecks(ctx, cfgStore, warningRegistry, certificateExpiryCheckEvery, googleauth.CertExpiry)
	if restorePauseErr != nil {
		warningRegistry.Set("capture-state", "Capture pause state could not be restored; the recorder started paused")
	}
	if batchTranscriber, err := makeBatchTranscriber(&cfg); err != nil {
		log.Printf("warning: batch transcription disabled: %v", err)
		warningRegistry.Set("batch-transcription", "Batch transcription unavailable — using streaming transcript fallback")
	} else {
		manager.SetBatchTranscriber(batchTranscriber)
	}

	authToken := os.Getenv("GHOST_WISPR_AUTH_TOKEN")

	server.SetVersionInfo(server.VersionInfo{Version: Version, Commit: Commit, BuildTime: BuildTime})
	drive := &driveSync{}
	var summaryPublisher atomic.Pointer[envoy.Publisher]
	publishSummaryReady := func(sessionID string) {
		pub := summaryPublisher.Load()
		if pub == nil {
			return
		}
		go func() {
			if err := pub.PublishSummaryReady(context.Background(), sessionID); err != nil {
				log.Printf("warning: envoy publish failed for %s: %v", sessionID, err)
			}
		}()
	}
	submitSummaryBatch := func(ctx context.Context, sessionIDs []string) (string, int, error) {
		if summarizer == nil {
			return "", 0, fmt.Errorf("summarization not configured")
		}
		latestCfg := cfgStore.Get()
		genOptions := genAIOptions(&latestCfg, latestCfg.GeminiAPIKey)
		if err := addVertexCredentials(ctx, &latestCfg, genOptions); err != nil {
			return "", 0, err
		}
		clientConfig, err := genaiconfig.BuildClientConfig(genOptions)
		if err != nil {
			return "", 0, fmt.Errorf("genai backend not configured for batch operations: %w", err)
		}

		sessions, generations, emptyTranscripts, err := readBatchSummarySessions(store, sessionIDs)
		if err != nil {
			return "", 0, err
		}
		completeEmptySummaries(store, emptyTranscripts)
		if len(sessions) == 0 {
			return "", 0, fmt.Errorf("no sessions with transcripts found for backfill")
		}
		for _, session := range sessions {
			if err := store.StartSummary(session.ID); err != nil {
				return "", 0, fmt.Errorf("start batch summary for %s: %w", session.ID, err)
			}
		}

		requests, err := summarizer.BuildBatchRequests(ctx, sessions)
		if err != nil {
			return "", 0, fmt.Errorf("build batch requests: %w", err)
		}

		genaiClient, err := genai.NewClient(ctx, clientConfig)
		if err != nil {
			return "", 0, fmt.Errorf("create genai client: %w", err)
		}

		_, model, err := llm.ParseModel(latestCfg.Summarization.Model)
		if err != nil {
			return "", 0, fmt.Errorf("parse model: %w", err)
		}

		job, err := genaiClient.Batches.Create(ctx, model, &genai.BatchJobSource{
			InlinedRequests: requests,
		}, nil)
		if err != nil {
			return "", 0, fmt.Errorf("create batch job: %w", err)
		}
		if err := store.RecordSummaryBatch(job.Name, generations); err != nil {
			return "", 0, fmt.Errorf("persist summary batch %s: %w", job.Name, err)
		}

		return job.Name, len(sessions), nil
	}
	pollSummaryBatch := func(ctx context.Context, batchName string) (map[string]any, error) {
		latestCfg := cfgStore.Get()
		genOptions := genAIOptions(&latestCfg, latestCfg.GeminiAPIKey)
		if err := addVertexCredentials(ctx, &latestCfg, genOptions); err != nil {
			return nil, err
		}
		clientConfig, err := genaiconfig.BuildClientConfig(genOptions)
		if err != nil {
			return nil, fmt.Errorf("genai backend not configured for batch operations: %w", err)
		}

		genaiClient, err := genai.NewClient(ctx, clientConfig)
		if err != nil {
			return nil, fmt.Errorf("create genai client: %w", err)
		}

		job, err := genaiClient.Batches.Get(ctx, batchName, nil)
		if err != nil {
			return nil, fmt.Errorf("get batch job: %w", err)
		}

		result := map[string]any{
			"name":  job.Name,
			"state": string(job.State),
		}

		if job.State == genai.JobStateSucceeded || job.State == genai.JobStatePartiallySucceeded {
			var sessions []summary.BatchSession
			if job.Dest != nil {
				for _, resp := range job.Dest.InlinedResponses {
					id := ""
					if resp.Metadata != nil {
						id = resp.Metadata["session_id"]
					}
					transcript := ""
					if id != "" {
						if loaded, ok := loadSessionTranscript(store, id); ok {
							transcript = loaded
						}
					}
					sessions = append(sessions, summary.BatchSession{ID: id, Transcript: transcript})
				}
			}
			batchResults := summarizer.ProcessBatchResults(job, sessions)
			if len(batchResults) == 0 {
				log.Printf("warning: summary batch %s completed without inline results; marking submitted sessions failed", batchName)
			}
			succeeded, failed, err := applySummaryBatchResults(store, hub, publishSummaryReady, batchName, batchResults)
			if err != nil {
				return nil, err
			}
			result["succeeded"] = succeeded
			result["failed"] = failed
			result["total"] = succeeded + failed
		}

		if job.State == genai.JobStateFailed {
			errMsg := "unknown error"
			if job.Error != nil {
				errMsg = job.Error.Message
			}
			_, failed, err := applySummaryBatchResults(store, hub, nil, batchName, nil)
			if err != nil {
				return nil, err
			}
			result["failed"] = failed
			result["total"] = failed
			result["error"] = errMsg
		}

		return result, nil
	}

	var keptRaw atomic.Pointer[server.KeptRawRecordings]
	go refreshKeptRawRecordings(ctx, audioRecorder, func() int { return cfgStore.Get().GCMaxAudioSizeMB }, &keptRaw, keptRawRefreshEvery)
	gcHealthState := &gcHealth{}
	gcHealthState.countUnverified(store)

	// deletionRunner makes the occurrence-based deletion actions use the
	// configuration as it is now. Batch transcription has already persisted
	// their exact word spans, so deleting never creates a locator.
	deletionRunner := func() *segmentDeleteRunner {
		latestCfg := cfgStore.Get()
		return &segmentDeleteRunner{
			store:    store,
			audioDir: latestCfg.AudioDir,
			ensureRecordings: func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
				var source deletionRecordingSource
				if session.GDriveFolderID != "" {
					if latestCfg.GDriveFolderID == "" {
						return deletionRecordingSet{}, fmt.Errorf("google Drive root folder is not configured")
					}
					credentials, err := googleauth.Detect(ctx, latestCfg.GoogleCredentialsFile, driveapi.DriveFileScope)
					if err != nil {
						return deletionRecordingSet{}, fmt.Errorf("detect Google credentials: %w", err)
					}
					syncer, err := gdrive.NewSyncer(ctx, credentials, latestCfg.GDriveFolderID, appLogger)
					if err != nil {
						return deletionRecordingSet{}, fmt.Errorf("create Drive syncer: %w", err)
					}
					source = syncer
				}
				return restoreDeletionRecordings(ctx, &session, latestCfg.AudioDir, source)
			},
			duration:  audio.Duration,
			splice:    audio.Splice,
			refine:    manager.RefineSession,
			summarize: manager.SummarizeSession,
		}
	}

	checkRefinementRecording := func(_ context.Context, sessionID string) error {
		sess, err := store.GetSession(sessionID)
		if err != nil {
			return err
		}
		return refinementRecordingAvailable(&sess, cfgStore.Get().AudioDir)
	}

	controlHooks := &server.ControlHooks{
		IsPaused:    capture.IsPaused,
		IsCapturing: func() bool { return capture.capturing(micStatus) },
		RefreshRawEraseWarnings: func() {
			if err := refreshRawEraseReservationWarnings(store, warningRegistry); err != nil {
				log.Printf("warning: raw erase reservations after delete failure: %v", err)
			}
		},
		CaptureState:  func() string { return capture.captureState(micStatus) },
		ActiveSession: manager.ActiveSession,
		Warnings:      warningRegistry.List,
		OnSessionDeleted: func(sessionID string) {
			warningRegistry.WithRecordingMismatch(func() {
				warningRegistry.Clear(recordingMismatchWarningPrefix + sessionID)
			})
			if err := refreshRawEraseReservationWarnings(store, warningRegistry); err != nil {
				log.Printf("warning: raw erase reservations after deleting session %s: %v", sessionID, err)
			}
		},
		OnSessionsMerged: func(sessionIDs []string) {
			warningRegistry.WithRecordingMismatch(func() {
				for _, sessionID := range sessionIDs {
					warningRegistry.Clear(recordingMismatchWarningPrefix + sessionID)
				}
			})
		},
		GCHold:             gcHealthState.Hold,
		UnverifiedSessions: gcHealthState.Unverified,
		UnitOnlySessions:   unitOnlyCounter(store),
		KeptRawRecordings: func() server.KeptRawRecordings {
			if kept := keptRaw.Load(); kept != nil {
				return *kept
			}
			return server.KeptRawRecordings{}
		},
		GetLogs: logSink.GetLogs,
		// RetrySync starts a sync of the session in the background and says
		// at once if none can start (for a session whose Drive folder was
		// deleted, while Drive says it is not back), so the API does not
		// report a retry that does nothing.
		RetrySync: func(ctx context.Context, sessionID string) error {
			return retrySync(ctx, drive.Get(), sessionID)
		},
		RefinementRecordingAvailable: checkRefinementRecording,
		RetryRefinement: newRetryRefinementHook(
			store,
			func() string { return cfgStore.Get().AudioDir },
			func(ctx context.Context, session storage.Session) (deletionRecordingSet, error) {
				return deletionRunner().ensureRecordings(ctx, session)
			},
			warningRegistry,
			manager.RefineSession,
		),
		DeleteSegment: func(ctx context.Context, sessionID string, segmentID int64, revision int) error {
			return deletionRunner().DeleteRefinedAudio(ctx, sessionID, segmentID, revision)
		},
		DeleteSegmentText: func(ctx context.Context, sessionID string, segmentID int64, revision int) error {
			return deletionRunner().DeleteRefinedText(ctx, sessionID, segmentID, revision)
		},
		RepairSummaries: func(ctx context.Context) (int, error) {
			return repairStructuredSummaries(ctx, store, publishSummaryReady)
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
			return resummarize(ctx, store, summarizer, hub, publishSummaryReady, sessionID, preset)
		},
		OnSessionMerged: func(ctx context.Context, sessionID string) {
			if summarizer == nil {
				return
			}
			summarizeMergedSession(ctx, store, summarizer, hub, publishSummaryReady, sessionID)
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
			credentials, err := googleauth.Detect(ctx, latestCfg.GoogleCredentialsFile, driveapi.DriveFileScope)
			if err != nil {
				return nil, fmt.Errorf("detect Google credentials: %w", err)
			}
			syncer, err := gdrive.NewSyncer(ctx, credentials, latestCfg.GDriveFolderID)
			if err != nil {
				return nil, fmt.Errorf("create syncer: %w", err)
			}
			result, err := syncer.RestoreFromDrive(ctx, func(rs gdrive.RestoredSession) error {
				sess := restoredDriveSession(&rs, time.Now().UTC())
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
		BatchBackfill: func(ctx context.Context) (string, int, error) {
			sessionIDs, err := store.GetSessionsForBackfill()
			if err != nil {
				return "", 0, fmt.Errorf("query sessions: %w", err)
			}
			if len(sessionIDs) == 0 {
				return "", 0, nil
			}
			return submitSummaryBatch(ctx, sessionIDs)
		},
		BatchPoll: func(ctx context.Context, batchName string) (map[string]any, error) {
			return pollSummaryBatch(ctx, batchName)
		},
	}

	hardware := startHardware(ctx, &cfg, capture, micStatus, warningRegistry, hub)
	pauseControls := newPersistentControls(
		store,
		func() {
			capture.Pause()
			hardware.CaptureStopped()
		},
		capture.RequestResume,
		hub.BroadcastStatusChanged,
	)
	controlHooks.Pause = func() error {
		err := pauseControls.Pause()
		if err != nil {
			warningRegistry.Set("capture-state", "Pause not saved; a restart will resume recording")
		} else {
			warningRegistry.Clear("capture-state")
		}
		return err
	}
	controlHooks.Resume = func() error {
		err := pauseControls.Resume()
		if err != nil {
			if errors.Is(err, errRefusedResumeNotPersisted) {
				warningRegistry.Set("capture-state", "Pause not saved; a restart will resume recording")
			}
		} else {
			warningRegistry.Clear("capture-state")
		}
		return err
	}
	wireLightConsent(capture, hardware, warningRegistry, func(error) {
		if err := pauseControls.RefuseResume(); err != nil {
			warningRegistry.Set("capture-state", "Pause not saved; a restart will resume recording")
		}
	})
	if restoredPause {
		hub.BroadcastStatusChanged(true)
	}
	startPowerButton(ctx, &cfg, capture, controlHooks, warningRegistry)

	setSummaryPublisher := func(next *envoy.Publisher) {
		summaryPublisher.Store(next)
		if next == nil {
			// Avoid storing a typed nil *Publisher in EventPublisher.
			manager.SetEventPublisher(nil)
			return
		}
		manager.SetEventPublisher(next)
	}
	sweepEnvoy := func(pub *envoy.Publisher) {
		if pub == nil {
			return
		}
		if err := pub.PublishPendingLifecycleEvents(ctx); err != nil {
			log.Printf("envoy lifecycle sweep error: %v", err)
		}
		ids, err := store.GetSessionsNeedingEnvoyPublish()
		if err != nil {
			log.Printf("envoy sweep error: %v", err)
			return
		}
		for _, id := range ids {
			if err := pub.PublishSummaryReady(ctx, id); err != nil {
				log.Printf("envoy sweep: session %s: %v", id, err)
			}
		}
	}
	configureEnvoyPublisher := func(currentCfg config.Config) {
		if !currentCfg.EnvoyEnabled || strings.TrimSpace(currentCfg.EnvoyWebhookURL) == "" {
			warningRegistry.Clear("envoy")
			setSummaryPublisher(nil)
			return
		}

		pub := envoy.NewPublisher(
			currentCfg.EnvoyWebhookURL,
			currentCfg.EnvoyWebhookSecret,
			store,
			envoy.WithLogger(appLogger),
		)
		warningRegistry.Clear("envoy")
		setSummaryPublisher(pub)
		log.Printf("envoy webhook publishing enabled")
		go sweepEnvoy(pub)
	}
	configureEnvoyPublisher(cfg)
	if pub := summaryPublisher.Load(); pub != nil && len(recoveredIDs) > 0 {
		ids := append([]string(nil), recoveredIDs...)
		go func() {
			for _, sessionID := range ids {
				if err := pub.PublishSessionEnded(context.Background(), sessionID); err != nil {
					log.Printf("warning: failed to publish recovered session end for %s: %v", sessionID, err)
				}
			}
		}()
	}
	if indexer != nil {
		go func() {
			if err := indexer.BackfillMissing(ctx); err != nil && ctx.Err() == nil {
				log.Printf("warning: embedding backfill failed: %v", err)
			}
		}()
	}

	// Register config change callback.
	onConfigChange(cfgStore, func(newCfg *config.Config) {
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
		if provider, _, err := llm.ParseModel(newCfg.Summarization.Model); err == nil && (provider != "gemini" && newAPIKeys[provider] != "" || provider == "gemini" && providerConfigured(newCfg, provider)) {
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

		configureEnvoyPublisher(*newCfg)
	})
	watchDriveSyncSettings(cfgStore, drive,
		func(cfg *config.Config) (sessionSyncer, error) {
			credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, driveapi.DriveFileScope)
			if err != nil {
				return nil, fmt.Errorf("detect Google credentials: %w", err)
			}
			syncer, err := gdrive.NewSyncer(ctx, credentials, cfg.GDriveFolderID, appLogger)
			if err != nil {
				return nil, err
			}
			orchestrator := gdrive.NewOrchestrator(syncer, store, appLogger)
			orchestrator.SetAudioDir(cfg.AudioDir)
			return orchestrator, nil
		},
		func(syncer sessionSyncer) {
			if syncer == nil {
				manager.SetSyncer(nil)
				return
			}
			manager.SetSyncer(syncer)
		})

	// Summarize recovered sessions (and any with pending/empty summaries).
	if startupSummarizer != nil {
		go func() {
			pendingIDs, err := store.GetSessionsForBackfill()
			if err != nil {
				log.Printf("warning: failed to query sessions needing summary: %v", err)
				return
			}
			if len(pendingIDs) == 0 {
				return
			}
			if len(pendingIDs) >= 4 {
				batchName, count, err := submitSummaryBatch(ctx, pendingIDs)
				if err == nil {
					log.Printf("startup: submitting %d sessions to batch summarization", count)
					go func(batchName string) {
						ticker := time.NewTicker(30 * time.Second)
						defer ticker.Stop()
						for {
							result, err := pollSummaryBatch(ctx, batchName)
							if err != nil {
								if ctx.Err() != nil {
									return
								}
								log.Printf("warning: startup batch job %s poll failed: %v", batchName, err)
							} else if state, _ := result["state"].(string); state != "" {
								succeeded, _ := result["succeeded"].(int)
								failed, _ := result["failed"].(int)
								switch state {
								case string(genai.JobStateSucceeded), string(genai.JobStatePartiallySucceeded):
									log.Printf("startup: batch job %s complete: %d succeeded, %d failed", batchName, succeeded, failed)
									return
								case string(genai.JobStateFailed):
									if errMsg, _ := result["error"].(string); errMsg != "" {
										log.Printf("startup: batch job %s failed: %s", batchName, errMsg)
									} else {
										log.Printf("startup: batch job %s failed", batchName)
									}
									return
								}
							}
							select {
							case <-ctx.Done():
								return
							case <-ticker.C:
							}
						}
					}(batchName)
					return
				}
				log.Printf("warning: startup batch submission failed for %d sessions: %v; falling back to synchronous summarization", len(pendingIDs), err)
			}
			for _, id := range pendingIDs {
				summarizeBackfillSession(ctx, store, startupSummarizer, publishSummaryReady, id)
			}
		}()
	}
	defer func() { _ = store.Close() }()

	var deletionDrive deletionRecordingSource
	if cfg.GDriveFolderID != "" && cfg.GDriveSyncEnabled {
		credentials, syncErr := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, driveapi.DriveFileScope)
		var syncer *gdrive.Syncer
		if syncErr == nil {
			syncer, syncErr = gdrive.NewSyncer(ctx, credentials, cfg.GDriveFolderID, appLogger)
		}
		if syncErr != nil {
			log.Printf("warning: gdrive sync disabled: %v", syncErr)
			warningRegistry.Set("gdrive", "Google Drive sync failed to initialize — backups are disabled")
		} else {
			deletionDrive = syncer
			orchestrator := gdrive.NewOrchestrator(syncer, store, appLogger)
			orchestrator.SetAudioDir(cfg.AudioDir)
			drive.Set(orchestrator)
			manager.SetSyncer(orchestrator)
		}
	}
	// The Drive sync sweep runs whether sync is on or not: it syncs nothing
	// while sync is off, and picks up a syncer turned on in the settings.
	go sweepDriveSync(ctx, store, drive, warningRegistry, 5*time.Minute)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepEnvoy(summaryPublisher.Load())
			}
		}
	}()

	var dgStop func()
	if cfg.DeepgramAPIKey != "" {
		controlHooks.FaultDeepgramDisconnect = func() error {
			current := resilientRef.Load()
			if current == nil {
				return server.ErrDeepgramNotConfigured
			}
			return current.Close()
		}
		dgStop = func() {
			current := resilientRef.Load()
			if current == nil {
				return
			}
			if err := current.Close(); err != nil {
				log.Printf("warning: close resilient deepgram client failed: %v", err)
			}
		}
	}

	paErr := audio.InitializePortAudio()
	paInitFailed := false
	if paErr != nil {
		log.Printf("warning: portaudio init failed: %v — microphone unavailable", paErr)
		paInitFailed = true
	}

	client.Init(client.InitLib{LogLevel: client.LogLevelDefault})

	// micLoops holds every mic loop and the mic startup retry, so shutdown can
	// wait for them before it stops the device and terminates PortAudio.
	var micLoops sync.WaitGroup
	micWait := func(d time.Duration) { capture.sleepUnlessPaused(ctx, d) }

	startMicPipeline := func(updateStartupState bool) error {
		var (
			mic        *audio.Mic
			sampleRate int
		)

		for _, rate := range cfg.SampleRateCandidates() {
			candidate, openErr := audio.NewMic(rate, rate/4) // 250ms buffer
			if openErr != nil {
				log.Printf("warning: microphone open failed at %d Hz: %v", rate, openErr)
				continue
			}
			mic = candidate
			sampleRate = rate
			break
		}

		if mic == nil {
			warningRegistry.Set("microphone", "Microphone unavailable — recording and live transcription are disabled")
			if updateStartupState {
				startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "Microphone unavailable"}
			}
			return fmt.Errorf("microphone unavailable")
		}

		audioRecorder.SetSampleRate(sampleRate)
		// micRef is published before the start so the controller, woken when
		// the start clears the forced-red hold, already sees the open mic.
		micRef.Store(mic)
		if err := startOpenedMic(capture, mic, func() { micRef.Store(nil) }, log.Printf); err != nil {
			warningRegistry.Set("microphone", "Microphone failed to start — recording and live transcription are disabled")
			if updateStartupState {
				startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "Microphone failed to start"}
			}
			return fmt.Errorf("microphone start failed at %d Hz: %w", sampleRate, err)
		}

		hardware.CaptureStarted()
		warningRegistry.Clear("microphone")
		log.Printf("microphone started at %d Hz", sampleRate)
		message := fmt.Sprintf("Microphone open at %d Hz", sampleRate)
		if updateStartupState {
			startupStatus.Mic = config.ComponentState{Name: "mic", Status: storage.ComponentStatusConnected, Message: message}
		} else if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, message)
		}

		if cfg.DeepgramAPIKey == "" {
			startMicMonitoring(ctx, &micLoops, capture, mic, audioRecorder, nil, micWait, log.Printf, hub)
			return nil
		}

		cOptions := &interfaces.ClientOptions{EnableKeepAlive: true}
		tOptions := &interfaces.LiveTranscriptionOptions{
			Model:          cfg.DeepgramModel,
			Language:       "en-US",
			Diarize:        true,
			Punctuate:      true,
			SmartFormat:    true,
			Encoding:       "linear16",
			SampleRate:     sampleRate,
			Channels:       1,
			Endpointing:    cfg.Transcription.Endpointing,
			InterimResults: true,
			UtteranceEndMs: cfg.Transcription.UtteranceEndMs,
			VadEvents:      true,
		}
		resilientConfig := transcribe.ResilientConfig{
			BufferSize:            cfg.DeepgramBufferSize,
			InitialReconnectDelay: cfg.ParsedDeepgramReconnectInitialDelay(),
			MaxReconnectBackoff:   cfg.ParsedDeepgramReconnectMaxBackoff(),
		}
		resillientStateCallback := func(state transcribe.ConnectionState) {
			if hub != nil {
				hub.BroadcastComponentStatus("deepgram", state.String(), fmt.Sprintf("Deepgram %s", state.String()))
			}
		}
		candidateResilient := transcribe.NewResilientClient(ctx, nil, resilientConfig, log.Printf, resillientStateCallback)
		callback := transcriptCallback{manager: manager, resilient: candidateResilient}
		factory := makeDeepgramClientFactory(ctx, cfg.DeepgramAPIKey, cOptions, tOptions, callback)
		candidateResilient.Factory = factory

		initialClient, err := factory(ctx)
		if err != nil {
			log.Printf("warning: deepgram client unavailable, running API/UI only: %v", err)
			warningRegistry.Set("deepgram", "Deepgram initialization failed — live transcription is disabled")
			if !updateStartupState && hub != nil {
				hub.BroadcastComponentStatus("deepgram", storage.ComponentStatusError, "Deepgram initialization failed")
			}
			startMicMonitoring(ctx, &micLoops, capture, mic, audioRecorder, nil, micWait, log.Printf, hub)
			return nil
		}

		candidateResilient.Client = initialClient
		resilientRef.Store(candidateResilient)
		if !updateStartupState && hub != nil {
			// A resilient client starts connected and reports only changes.
			hub.BroadcastComponentStatus("deepgram", storage.ComponentStatusConnected, "Deepgram connected")
		}

		startMicMonitoring(ctx, &micLoops, capture, mic, audioRecorder, candidateResilient, micWait, log.Printf, hub)
		return nil
	}

	retryMic := func() {
		retryMicStartup(
			ctx,
			micRetryEvery,
			time.After,
			capture,
			func() {
				if hub != nil {
					hub.BroadcastComponentStatus("mic", storage.ComponentStatusReconnecting, "Retrying microphone initialization...")
				}
			},
			func() error {
				if err := audio.ReinitPortAudio(); err != nil {
					return err
				}
				return startMicPipeline(false)
			},
			func(err error) {
				log.Printf("mic startup retry failed: %v", err)
				if hub != nil {
					hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, err.Error())
				}
			},
			func() {
				log.Printf("mic startup retry succeeded")
			},
		)
	}
	startMic := func(atBoot bool) error {
		err := startMicPipeline(atBoot)
		if err != nil {
			slog.Warn("microphone unavailable, running API/UI only", "error", err)
		}
		return err
	}
	// booted closes once start-up has sent its component statuses.
	booted := make(chan struct{})
	if startMicrophone(ctx, &micLoops, capture, booted, !paInitFailed, startMic, retryMic) {
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: storage.ComponentStatusPaused, Message: "Microphone closed while capture is paused"}
		if cfg.DeepgramAPIKey != "" {
			startupStatus.Deepgram = config.ComponentState{Name: "deepgram", Status: storage.ComponentStatusPaused, Message: "Deepgram connects when capture resumes"}
		}
	}

	if paInitFailed {
		slog.Warn("microphone unavailable, running API/UI only")
		warningRegistry.Set("microphone", "Microphone unavailable — recording and live transcription are disabled")
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "Microphone unavailable"}
	}
	// Initialize speaker output device (completely isolated from mic).
	var speaker *audio.Speaker
	if cfg.SpeakerEnabled {
		if paInitFailed {
			slog.Warn("speaker requested but portaudio init failed — speaker disabled")
			warningRegistry.Set("speaker", "Speaker unavailable — PortAudio initialization failed")
		} else {
			speaker = audio.NewSpeaker(cfg.SpeakerDevice)
			log.Printf("speaker output enabled (device=%q)", cfg.SpeakerDevice)
		}
	}

	// Initialize TTS orchestrator if both speaker and provider are configured.
	var ttsOrchestrator *tts.Orchestrator
	if speaker != nil && cfg.TTSProvider != "" {
		var ttsProvider tts.Provider
		var ttsErr error
		switch cfg.TTSProvider {
		case "elevenlabs":
			if cfg.TTSAPIKey == "" {
				ttsErr = fmt.Errorf("ElevenLabs requires GHOST_WISPR_TTS_API_KEY")
			} else {
				ttsProvider, ttsErr = tts.NewElevenLabsProvider(cfg.TTSAPIKey, cfg.TTSVoice)
			}
		case "google":
			if cfg.TTSAPIKey != "" {
				ttsProvider, ttsErr = tts.NewGoogleProvider(cfg.TTSAPIKey, cfg.TTSVoice)
			} else {
				credentials, credentialErr := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
				if credentialErr != nil {
					ttsErr = fmt.Errorf("detect Google credentials: %w", credentialErr)
				} else {
					ttsProvider, ttsErr = tts.NewGoogleProvider("", cfg.TTSVoice, tts.WithAuthCredentials(credentials))
				}
			}
		case "gemini":
			credentials, credentialErr := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
			if credentialErr != nil {
				ttsErr = fmt.Errorf("detect Google credentials: %w", credentialErr)
			} else {
				ttsProvider, ttsErr = tts.NewGeminiProvider(
					cfg.GCPProject,
					cfg.TTSModel,
					cfg.TTSVoice,
					tts.WithGeminiAuthCredentials(credentials),
				)
			}
		default:
			ttsErr = fmt.Errorf("unsupported TTS provider: %s", cfg.TTSProvider)
		}
		if ttsErr != nil {
			log.Printf("warning: TTS disabled: %v", ttsErr)
			warningRegistry.Set("tts", fmt.Sprintf("TTS provider initialization failed: %v", ttsErr))
		} else {
			ttsOrchestrator = tts.NewOrchestrator(ttsProvider, speaker, tts.OrchestratorOpts{})
			log.Printf("TTS orchestrator started (provider=%s)", cfg.TTSProvider)
		}
	}

	// Wire up TTS hooks now that orchestrator is initialized.
	if ttsOrchestrator != nil {
		controlHooks.TTSSpeak = ttsOrchestrator.Speak
		controlHooks.TTSStatus = func(id string) (map[string]any, bool) {
			status, ok := ttsOrchestrator.Status(id)
			if !ok {
				return nil, false
			}
			return map[string]any{
				"id":            status.ID,
				"status":        status.Status,
				"bytes_written": status.BytesWritten,
				"duration_ms":   status.DurationMs,
				"model":         status.Model,
				"voice":         status.Voice,
				"error":         status.Error,
			}, true
		}
		controlHooks.TTSWaitForSynthesis = func(ctx context.Context, id string) (map[string]any, error) {
			status, err := ttsOrchestrator.WaitForSynthesis(ctx, id)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"id":            status.ID,
				"status":        status.Status,
				"bytes_written": status.BytesWritten,
				"duration_ms":   status.DurationMs,
				"model":         status.Model,
				"voice":         status.Voice,
				"error":         status.Error,
			}, nil
		}
		controlHooks.TTSHasProvider = ttsOrchestrator.HasProvider
		controlHooks.TTSHasSpeaker = ttsOrchestrator.HasSpeaker
	}

	// Wire up the mic diagnostic hook. A paused instance opens the mic only on
	// resume, so until then there is none to diagnose.
	controlHooks.DiagnoseMic = func(ctx context.Context) (map[string]any, error) {
		mic := micRef.Load()
		if mic == nil {
			if capture.IsPaused() {
				return nil, fmt.Errorf("microphone not opened while capture is paused")
			}
			return nil, fmt.Errorf("microphone not available")
		}
		sampleRate := mic.SampleRate()
		return map[string]any{
			"mic_open":    mic.IsOpen(),
			"sample_rate": sampleRate,
			"status":      "operational",
			"message":     fmt.Sprintf("Microphone open at %d Hz", sampleRate),
		}, nil
	}

	lightOff := hardware.Done()
	// This cleanup must run before anything else splices or erases in these
	// directories: before the HTTP listener below (segment and session
	// deletions), before recoverPendingSplices, before the first Drive sweep
	// (5 minutes), whose ticker starts above, and before GC, which starts
	// below with a pass at once.
	// erase.Leftovers unlinks the second name an erase.Replace under way still
	// needs, cleanupSpliceTempCopiesExcept removes the copies a running deletion
	// splices, and ForgetEraseDirectories forgets directories no erase is
	// using only while none runs. The recorder, started above, may already
	// erase a saved session's raw recording in the audio directory:
	// erase.Remove makes no second name, and either erase finishing the file
	// overwrites it. The cleanup then records the audio directory
	// (RecordEraseDirectory), so an erase the recorder cuts off there is
	// still finished at a start with another audio_dir.
	if err := cleanupRecordingDirectories(store, cfg.AudioDir); err != nil {
		log.Printf("warning: failed to clean leftover splice copies: %v", err)
		recordPendingSpliceCleanupWarning(warningRegistry, err)
	}
	if cfg.GCEnabled {
		go collectGarbage(ctx, cfgStore, store, drive, gcHealthState, warningRegistry, appLogger, gcEvery)
	}

	// Create health checker with actual component references. With no Deepgram
	// API key it gets no Deepgram client source, so readiness never excuses
	// Deepgram, paused or not.
	var deepgramHealth interface{ IsConnected() bool }
	if cfg.DeepgramAPIKey != "" {
		deepgramHealth = &resilientClientProxy{ref: &resilientRef}
	}
	healthChecker := server.NewDefaultHealthChecker(deepgramHealth, store, micStatus, capture, llmTracker)
	handler, err := server.HandlerWithLogger(assets, hub, store, controlHooks, authToken, healthChecker, appLogger, embeddingClient, cfgStore)
	if err != nil {
		panic(fmt.Sprintf("build http handler failed: %v", err))
	}

	addr := listenAddress(os.Getenv("GHOST_WISPR_ADDR"))
	httpServer := &http.Server{Addr: addr, Handler: handler}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server error: %v", err)
		}
	}()

	go func() {
		outcomes, err := recoverPendingSplices(ctx, store, cfg.AudioDir, deletionDrive, audio.Splice, manager.RefineSession)
		applyPendingSpliceRecoveryWarnings(warningRegistry, outcomes)
		if err != nil {
			log.Printf("warning: failed to recover pending segment splices: %v", err)
			recordPendingSpliceRecoveryWarning(warningRegistry, err)
		} else {
			recovered := 0
			for i := range outcomes {
				if outcomes[i].Recovered {
					recovered++
				}
			}
			if recovered > 0 {
				log.Printf("recovered %d pending segment splices", recovered)
			}
		}
	}()

	// Log structured startup banner.
	config.LogStartupBanner(appLogger, &startupStatus, Version, Commit, BuildTime)
	slog.Info("deepgram API key", "status", config.MaskAPIKey(cfg.DeepgramAPIKey))
	uiAddr := addr
	if strings.HasPrefix(uiAddr, ":") {
		uiAddr = "127.0.0.1" + uiAddr
	}
	log.Printf("ghost-wispr %s (commit=%s built=%s): web UI on http://%s", Version, Commit, BuildTime, uiAddr)

	// Broadcast initial component status to any connected WebSocket clients.
	for _, c := range startupStatus.Components() {
		hub.BroadcastComponentStatus(c.Name, c.Status, c.Message)
	}
	close(booted)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("ghost-wispr: shutting down")
	hardware.Stop()
	cancel()
	manager.StopSessionStartPublishes()
	// The mic loops end at the device's next buffer. Stopping the device or
	// terminating PortAudio while one is still inside a read crashes inside
	// PortAudio, so neither happens before they end, nor at all if one is stuck
	// in a read: the process exit releases the device then.
	micLoopsEnded := joinMicLoops(&micLoops, micShutdownWait)
	if !micLoopsEnded {
		log.Printf("warning: microphone still reading %v after shutdown began; leaving the device and PortAudio to the process exit", micShutdownWait)
	}
	select {
	case <-lightOff:
	case <-time.After(lightShutdownWait):
		log.Printf("warning: recording light not turned off within %v; leaving it to its watchdog", lightShutdownWait)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := manager.ForceEndSession(shutdownCtx); err != nil {
		log.Printf("warning: force end session failed: %v", err)
	}
	lifecycleCtx, lifecycleCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := manager.WaitForLifecyclePublishes(lifecycleCtx); err != nil {
		log.Printf("warning: lifecycle event publishing did not finish before shutdown: %v", err)
	}
	lifecycleCancel()

	if dgStop != nil {
		dgStop()
	}
	if mic := micRef.Load(); mic != nil && micLoopsEnded {
		_ = mic.Stop()
	}
	if ttsOrchestrator != nil {
		ttsOrchestrator.Stop()
	}
	if speaker != nil {
		_ = speaker.Close()
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("warning: http shutdown failed: %v", err)
	}
	if micLoopsEnded {
		_ = audio.TerminatePortAudio() // best-effort cleanup
	}
}

type micStreamer interface {
	Stream(writer io.Writer) error
	Start() error
	Stop() error
	Close() error
	Reopen() error
}

// shutdownWriter drops audio once ctx ends and returns ctx's error, which ends
// Mic.Stream at the device's next buffer, so the mic loop ends at shutdown.
type shutdownWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w shutdownWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.dst.Write(p)
}

func streamMicWithRetry(
	ctx context.Context,
	capture *captureGate,
	streamer micStreamer,
	writer io.Writer,
	wait func(time.Duration),
	logf func(string, ...any),
	hub *server.Hub,
) {
	const (
		overflowWait = 250 * time.Millisecond
		baseBackoff  = time.Second
		maxBackoff   = 30 * time.Second
	)

	backoff := baseBackoff
	gated := shutdownWriter{ctx: ctx, dst: capture.Writer(writer)}

	for {
		if ctx.Err() != nil {
			return
		}

		var err error
		if capture.isClosed() {
			if !holdMicWhilePaused(ctx, capture, streamer, logf, hub) || ctx.Err() != nil {
				return
			}
			if err = capture.confirmMicStart(); err != nil {
				logf("recording light unavailable before microphone restart: %v", err)
				closeMic(streamer, "without a red recording light", logf)
				capture.finishResume()
				wait(backoff)
				continue
			}
			err = streamer.Start()
			capture.finishMicStart()
			// This start, whether it works or not, ends the resume's
			// "resuming" state: readiness then reports what came back.
			capture.finishResume()
			if err == nil {
				logf("microphone restarted: capture resumed")
				if hub != nil {
					hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone capture resumed")
				}
				continue
			}
			// The device did not come back (unplugged during the pause): close
			// it, so it holds no input device through the backoff, and
			// reconnect below.
			closeMic(streamer, "that did not restart", logf)
		} else {
			err = streamer.Stream(gated)
			if err == nil || ctx.Err() != nil {
				return
			}
			// A pause, or a device error that coincides with one: stop the
			// device at the loop top instead of reporting a failure.
			if errors.Is(err, errGateClosed) || capture.isClosed() {
				continue
			}
		}

		if strings.Contains(strings.ToLower(err.Error()), "overflow") {
			logf("warning: mic input overflow, restarting stream")
			if hub != nil {
				hub.BroadcastComponentStatus("mic", "warning", "Mic input overflow, restarting stream")
			}
			wait(overflowWait)
			if ctx.Err() != nil {
				return
			}
			if capture.isClosed() {
				continue
			}
			if hub != nil {
				hub.BroadcastComponentStatus("mic", storage.ComponentStatusReconnecting, "Attempting to reconnect microphone...")
			}
			if err := capture.confirmMicStart(); err != nil {
				logf("recording light unavailable before microphone reconnect: %v", err)
				closeMic(streamer, "without a red recording light", logf)
				continue
			}
			reopenErr := streamer.Reopen()
			capture.finishMicStart()
			if reopenErr != nil {
				logf("mic reopen failed: %v", reopenErr)
				if hub != nil {
					hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, reopenErr.Error())
				}
				continue
			}
			logf("mic reopened successfully")
			if hub != nil {
				hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone reconnected")
			}
			continue
		}

		logf("mic stream error (retrying in %v): %v", backoff, err)
		if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, err.Error())
		}
		wait(backoff)

		if ctx.Err() != nil {
			return
		}
		if capture.isClosed() {
			continue // never reopen the mic while paused; the loop top stops it
		}
		if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusReconnecting, "Attempting to reconnect microphone...")
		}

		if err := capture.confirmMicStart(); err != nil {
			logf("recording light unavailable before microphone reconnect: %v", err)
			closeMic(streamer, "without a red recording light", logf)
			continue
		}
		reopenErr := streamer.Reopen()
		capture.finishMicStart()
		if reopenErr != nil {
			logf("mic reopen failed: %v", reopenErr)
			if hub != nil {
				hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, reopenErr.Error())
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		logf("mic reopened successfully")
		if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone reconnected")
		}
		backoff = baseBackoff
	}
}

// closeMic closes a device that failed to start, or that the light could not
// be confirmed red for, so it holds no input device meanwhile. An overflow, or
// a transient read error, leaves the device capturing with IsOpen false, so
// the light goes dark while audio still flows; a resume finds the device
// stopped but open. The next Stream then fails on the closed device, and the
// error path backs off and confirms red again before it reopens.
func closeMic(streamer micStreamer, which string, logf func(string, ...any)) {
	if err := streamer.Close(); err != nil {
		logf("warning: close microphone %s: %v", which, err)
	}
}

// holdMicWhilePaused stops the capture device and blocks until capture
// resumes. It reports false if ctx ends first.
func holdMicWhilePaused(ctx context.Context, capture *captureGate, streamer micStreamer, logf func(string, ...any), hub *server.Hub) bool {
	if err := streamer.Stop(); err != nil {
		logf("warning: stop microphone for capture pause: %v", err)
	}
	logf("microphone stopped: capture paused")
	if hub != nil {
		hub.BroadcastComponentStatus("mic", storage.ComponentStatusPaused, "Microphone stopped while capture is paused")
	}
	return capture.waitResumed(ctx)
}
