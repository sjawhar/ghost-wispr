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
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/tts"

	"google.golang.org/genai"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

const defaultListenAddress = "127.0.0.1:8080"

// captureStateWarningKey keys the /api/status warning that capture's saved
// pause switch is not what a restart would restore.
const captureStateWarningKey = "capture-state"

var vertexCredentialCache sync.Map

//go:embed static/*
var staticFiles embed.FS

// importRestoredDriveSession returns the Drive restore callback: it imports
// one restored session with the recording file names its folder holds, in
// one transaction.
func importRestoredDriveSession(store *storage.SQLiteStore, restoredAt func() time.Time) func(gdrive.RestoredSession) error {
	return func(rs gdrive.RestoredSession) error {
		sess := restoredDriveSession(&rs, restoredAt())
		return store.ImportSession(&sess, rs.Segments, rs.RecordingPaths)
	}
}

// newMergedRefinementFailureRecorder returns the merge hook's failure
// recorder: a merged session whose refinement could not run reads failed, so
// the page offers Retry refinement rather than waiting forever.
func newMergedRefinementFailureRecorder(store *storage.SQLiteStore) func(context.Context, string, error) {
	return func(_ context.Context, sessionID string, cause error) {
		generation, err := store.DeletionGeneration(sessionID)
		if err == nil {
			err = store.UpdateRefinementAtGeneration(sessionID, "", storage.RefinementFailed, "", generation)
		}
		if err != nil {
			log.Printf("warning: record failed merged refinement %s after %v: %v", sessionID, cause, err)
		}
	}
}

func restoredDriveSession(rs *gdrive.RestoredSession, restoredAt time.Time) storage.Session {
	summaryStatus := storage.SummaryPending
	if rs.Summary != "" {
		summaryStatus = storage.SummaryCompleted
	}
	audioRemovedAt := ""
	if rs.HasAudio {
		audioRemovedAt = restoredAt.UTC().Format(time.RFC3339Nano)
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
		AudioRemovedAt:   audioRemovedAt,
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

// batchClientConfig is the genai client configuration the summary batch
// operations use: cfg's Gemini settings, with Vertex AI credentials.
func batchClientConfig(ctx context.Context, cfg *config.Config) (*genai.ClientConfig, error) {
	options := genAIOptions(cfg, cfg.GeminiAPIKey)
	if err := addVertexCredentials(ctx, cfg, options); err != nil {
		return nil, err
	}
	clientConfig, err := genaiconfig.BuildClientConfig(options)
	if err != nil {
		return nil, fmt.Errorf("genai backend not configured for batch operations: %w", err)
	}
	return clientConfig, nil
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
// again, after an attempt failed.
const micRetryEvery = 15 * time.Second

// resumeWaiter is the part of the capture gate a microphone start waits on:
// waitResumed blocks until the gate is open, prepareMicOpen readies the
// hardware before the start opens the microphone, openUnderRed runs the start
// with the recording light confirmed and held red, and finishResume ends a
// resume's "resuming" state once the start that brings capture back has
// finished.
type resumeWaiter interface {
	waitResumed(ctx context.Context) bool
	prepareMicOpen(ctx context.Context) bool
	openUnderRed(open func() error) error
	finishResume()
}

// retryMicStartup runs start every retryInterval until it succeeds or ctx
// ends. A paused instance opens no input device: a retry that comes due
// while capture is paused waits (capture.waitResumed) until capture resumes.
// Each attempt runs under a red hold (openUnderRed): the PortAudio scan and the
// device open it makes both open capture streams. A light that cannot be
// confirmed red fails the attempt before either.
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

	wait := true
	for {
		if wait {
			select {
			case <-ctx.Done():
				return
			case <-after(retryInterval):
			}
		}
		wait = true
		if !capture.waitResumed(ctx) {
			return
		}
		// A pause while it waits for the Jabra (prepareMicOpen) cancels the
		// attempt; the next comes once capture resumes.
		if !capture.prepareMicOpen(ctx) {
			continue
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
		err := capture.openUnderRed(start)
		capture.finishResume()
		if errors.Is(err, errMicStartPaused) {
			// A pause cancelled the attempt: the next resume tries again.
			wait = false
			continue
		}
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
// runs retry in loops if start fails. start runs only under a red hold
// (openUnderRed): the light is confirmed red before it, and held red until it
// returns, so every capture stream it opens, the PortAudio scan's included,
// opens while the light last read red. A paused instance opens no input device
// until it resumes: while the capture gate is closed, start runs in loops once
// the gate opens and the hardware is ready for it (prepareMicOpen), with atBoot
// false, and startMicrophone returns deferred true; that start, whether it
// works or not, ends the resume's "resuming" state (finishResume). A pause
// while it waits for the hardware, or one that cancels the start before it
// opens a device (errMicStartPaused), cancels that resume, and the next
// resume starts the microphone. Otherwise start runs at once with atBoot true,
// so it can write the start-up status, and startMicrophone returns its error:
// one that wraps errLightNotConfirmed if the light refused it, or
// errNoCaptureDevice, and start did not run.
//
// Nothing runs in loops before booted closes, once start-up has sent its
// component statuses: a power-button resume can come during start-up, and
// the statuses its start sends (microphone and Deepgram connected) must not be
// followed by the start-up ones (paused), which would then stand for every
// page that connects later.
func startMicrophone(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, booted <-chan struct{}, start func(atBoot bool) error, retry func()) (deferred bool, err error) {
	afterBoot := func(run func()) {
		loops.Go(func() {
			select {
			case <-booted:
				run()
			case <-ctx.Done():
			}
		})
	}
	startOnResume := func() {
		for {
			for {
				if !capture.waitResumed(ctx) {
					return
				}
				if capture.prepareMicOpen(ctx) {
					break
				}
			}
			err := capture.openUnderRed(func() error { return start(false) })
			capture.finishResume()
			switch {
			case err == nil:
				return
			case errors.Is(err, errMicStartPaused):
				continue
			}
			retry()
			return
		}
	}
	if capture.isClosed() {
		afterBoot(startOnResume)
		return true, nil
	}
	err = capture.openUnderRed(func() error { return start(true) })
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, errMicStartPaused):
		afterBoot(startOnResume)
		return true, nil
	}
	afterBoot(retry)
	return false, err
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

// Open logs the socket. It leaves the resilient client's state alone: the
// app-owned socket reports Open while it dials, which during a reconnect is
// before the new socket replaces the dead one, and the reconnect marks the
// client connected itself once its buffered audio is sent.
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

// streamMicLoopStarter runs a mic loop in loops, the group shutdown waits for.
var streamMicLoopStarter = func(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, streamer micStreamer, writer io.Writer, wait func(time.Duration), logf func(string, ...any), hub *server.Hub) {
	loops.Go(func() { streamMicWithRetry(ctx, capture, streamer, writer, wait, logf, hub) })
}

// deepgramSendChunks is how many microphone buffers (250 ms each) the
// Deepgram send queues while a write waits: more than deepgramWriteTimeout.
const deepgramSendChunks = 64

// startMicMonitoring starts the mic loop. The recorder gets each buffer
// first, and downstream, live transcription, gets it through a bounded queue
// that never blocks the loop (transcribe.AsyncSender), so no Deepgram
// trouble stops the recording. It returns that queue, whose Finalize the
// capture pause uses, or nil without downstream.
func startMicMonitoring(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, streamer micStreamer, recorder *audio.Recorder, downstream interface {
	io.Writer
	Finalize() error
}, wait func(time.Duration), logf func(string, ...any), hub *server.Hub) *transcribe.AsyncSender {
	if streamer == nil || recorder == nil {
		return nil
	}
	var sender *transcribe.AsyncSender
	send := io.Discard
	if downstream != nil {
		sender = transcribe.NewAsyncSender(ctx, downstream, deepgramSendChunks, logf)
		send = sender
	}
	streamMicLoopStarter(ctx, loops, capture, streamer, recorder.Writer(send), wait, logf, hub)
	return sender
}

// micShutdownWait is how long shutdown waits for the mic loops to end. Each
// loop closes its microphone as shutdown begins (streamUntilClosed), which
// ends its Stream at once, even on a device that delivers no frame.
const micShutdownWait = 5 * time.Second

// joinMicLoops waits up to wait for the mic loops to end once shutdown has
// cancelled their context. It reports false if one has not: its Stream is
// stuck in a write of its audio, or in a PortAudio call, that does not
// return, and nothing may stop the device or terminate PortAudio under it.
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

// keptRawRefreshEvery is how often the count of the raw recordings the app
// keeps is refreshed for GET /api/status.
const keptRawRefreshEvery = time.Minute

// refreshKeptRawRecordings keeps cache up to date with the raw recordings the
// app keeps (keptRecordings.census), every interval until ctx ends. GET
// /api/status reads the cache, so a stuck disk or a hung recording write
// cannot hang the status. The status warns once they take more than a tenth
// of the audio budget, which they are not counted against, or as soon as
// there is one when there is no budget (0): GC never removes them.
func refreshKeptRawRecordings(ctx context.Context, kept *keptRecordings, budgetMB func() int, cache *atomic.Pointer[server.KeptRawRecordings], interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		census := kept.census()
		if budget, limited := gc.AudioBudgetBytes(budgetMB()); limited {
			census.WarnBytes = budget / 10
		}
		cache.Store(&census)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// makeDeepgramClientFactory dials live-transcription sockets the app owns
// (deepgramSocket), each write bounded by writeTimeout.
func makeDeepgramClientFactory(_ context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, callback transcriptCallback, writeTimeout time.Duration) transcribe.ClientFactory {
	return func(factoryCtx context.Context) (io.WriteCloser, error) {
		return dialDeepgramLive(factoryCtx, apiKey, cOptions, tOptions, callback, writeTimeout)
	}
}

type mergedSessionHookStore interface {
	GetSession(string) (storage.Session, error)
}

func onSessionMerged(ctx context.Context, store mergedSessionHookStore, refine func(context.Context, string, string, time.Time) error, summarize func(context.Context, string, time.Time) error, retryRefinement func(context.Context, string) error, recordRefinementFailure func(context.Context, string, error), sessionID string) {
	merged, err := store.GetSession(sessionID)
	if err != nil {
		log.Printf("warning: read merged session %s before refinement: %v", sessionID, err)
		return
	}
	if merged.AudioRemovedAt != "" {
		if err := retryRefinement(ctx, sessionID); err != nil {
			recordRefinementFailure(ctx, sessionID, err)
			if err := summarize(ctx, sessionID, merged.StartedAt); err != nil {
				log.Printf("warning: summarize failed merged session %s: %v", sessionID, err)
			}
		}
		return
	}
	if merged.AudioPath != "" {
		if err := refine(ctx, sessionID, merged.AudioPath, merged.StartedAt); err == nil {
			return
		} else {
			log.Printf("warning: refine merged session %s: %v", sessionID, err)
		}
	}
	if err := summarize(ctx, sessionID, merged.StartedAt); err != nil {
		log.Printf("warning: summarize merged session %s: %v", sessionID, err)
	}
}

// ttsStatusResponse is a TTS request's status as the TTS routes return it.
func ttsStatusResponse(status *tts.SpeakStatus) map[string]any {
	return map[string]any{
		"id":            status.ID,
		"status":        status.Status,
		"bytes_written": status.BytesWritten,
		"duration_ms":   status.DurationMs,
		"model":         status.Model,
		"voice":         status.Voice,
		"error":         status.Error,
	}
}

// newTTSProvider builds the TTS provider cfg names.
func newTTSProvider(ctx context.Context, cfg *config.Config) (tts.Provider, error) {
	switch cfg.TTSProvider {
	case "elevenlabs":
		if cfg.TTSAPIKey == "" {
			return nil, fmt.Errorf("ElevenLabs requires GHOST_WISPR_TTS_API_KEY")
		}
		return tts.NewElevenLabsProvider(cfg.TTSAPIKey, cfg.TTSVoice)
	case "google":
		if cfg.TTSAPIKey != "" {
			return tts.NewGoogleProvider(cfg.TTSAPIKey, cfg.TTSVoice)
		}
		credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("detect Google credentials: %w", err)
		}
		return tts.NewGoogleProvider("", cfg.TTSVoice, tts.WithAuthCredentials(credentials))
	case "gemini":
		credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("detect Google credentials: %w", err)
		}
		return tts.NewGeminiProvider(cfg.GCPProject, cfg.TTSModel, cfg.TTSVoice, tts.WithGeminiAuthCredentials(credentials))
	default:
		return nil, fmt.Errorf("unsupported TTS provider: %s", cfg.TTSProvider)
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

	// Recover any sessions left as 'active' from a previous crash/restart:
	// each ends when its raw recording was last written.
	recoveredIDs, err := store.RecoverStaleSessions(cfg.AudioDir)
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
	var deepgramSendRef atomic.Pointer[transcribe.AsyncSender]
	var micRef atomic.Pointer[audio.Mic]
	micStatus := &micProxy{ref: &micRef}
	// capture is the one pause switch: POST /api/pause and /api/resume (and any
	// later hardware button) call its Pause and Resume.
	capture := newSessionCaptureGate(ctx, manager, audioRecorder, func() deepgramFinalizer {
		// The send queue first: its Finalize follows the audio queued before it.
		if send := deepgramSendRef.Load(); send != nil {
			return send
		}
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
		warningRegistry.Set(captureStateWarningKey, "Capture pause state could not be restored; the recorder started paused")
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
	envoyEvents := &envoyPublishing{store: store, manager: manager, warnings: warningRegistry, openSecrets: openSecretManager}
	publishSummaryReady := envoyEvents.publishSummaryReady
	submitSummaryBatch := func(ctx context.Context, sessionIDs []string) (string, int, error) {
		if summarizer == nil {
			return "", 0, fmt.Errorf("summarization not configured")
		}
		latestCfg := cfgStore.Get()
		clientConfig, err := batchClientConfig(ctx, &latestCfg)
		if err != nil {
			return "", 0, err
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
		clientConfig, err := batchClientConfig(ctx, &latestCfg)
		if err != nil {
			return nil, err
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

	// The kept recordings pass saves the raw recordings of sessions that
	// ended without a recording, and erases a raw recording once Drive has
	// its saved recording or cannot take it; its census feeds the status.
	kept := newKeptRecordings(store, audioRecorder, cfg.AudioDir, drive, refineRecoveredRecording(manager))
	var keptRaw atomic.Pointer[server.KeptRawRecordings]
	go refreshKeptRawRecordings(ctx, kept, func() int { return cfgStore.Get().GCMaxAudioSizeMB }, &keptRaw, keptRawRefreshEvery)
	gcHealthState := &gcHealth{}
	gcHealthState.countUnverified(store)

	// refusedCuts keeps a refused "Delete text and audio" on /api/status, in
	// the store, while its segment is there, naming the session and the
	// segment's time.
	refusedCuts := newRefusedCuts(store)
	deletion, err := newAppDeleteRunner(store, cfgStore, manager.RefineSession, manager.SummarizeSession, refusedCuts, warningRegistry, appLogger)
	if err != nil {
		panic(fmt.Sprintf("segment deletion: %v", err))
	}
	recordMergedRefinementFailure := newMergedRefinementFailureRecorder(store)

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
		// Warnings, DeleteSegment and DeleteSegmentText: wireSegmentDeletion,
		// below.
		OnSessionDeleted: sessionsDeletedHook(store, warningRegistry),
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
		RefinementRecordingAvailable: deletion.checkRefinementRecording,
		RetryRefinement:              deletion.retryRefinement,
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
			onSessionMerged(
				ctx,
				store,
				manager.RefineSession,
				func(ctx context.Context, sessionID string, startedAt time.Time) error {
					if summarizer == nil {
						return manager.SummarizeSession(ctx, sessionID, startedAt)
					}
					summarizeMergedSession(ctx, store, summarizer, hub, publishSummaryReady, sessionID)
					return nil
				},
				deletion.retryRefinement,
				recordMergedRefinementFailure,
				sessionID,
			)
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
			syncer, err := newDriveSyncer(ctx, &latestCfg, appLogger)
			if err != nil {
				return nil, err
			}
			result, err := syncer.RestoreFromDrive(ctx, importRestoredDriveSession(store, func() time.Time { return time.Now().UTC() }))
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
	wireSegmentDeletion(controlHooks, warningRegistry, refusedCuts, deletion)

	// A refinement that cannot use the model a "Delete text" was recorded
	// under publishes nothing and says so here, until the session refines.
	manager.SetRefinementWarningHook(refinementModelWarningHook(warningRegistry))
	// The light has a context of its own, which shutdown ends only once the
	// mic loops have closed every microphone: the light is never off while
	// one is open.
	lightCtx, stopLight := context.WithCancel(context.Background())
	defer stopLight()
	hardware := startHardware(lightCtx, &cfg, capture, micStatus, warningRegistry, hub)
	pauseControls := newPersistentControls(
		store,
		func() { pauseCapture(capture, hardware) },
		capture.RequestResume,
		hub.BroadcastStatusChanged,
	)
	wirePauseControls(controlHooks, pauseControls, capture, hardware, warningRegistry)
	if restoredPause {
		hub.BroadcastStatusChanged(true)
	}
	// micDevice orders the Jabra's mute light against every open, close and
	// probe of the capture device. A resume waits, before it confirms red,
	// until the light can show it (prepareLightForOpen).
	micDevice := newCaptureDevice(capture.IsPaused)
	capture.beforeMicOpen = micDevice.prepareLightForOpen
	capture.onMicHeld = micHeldWarning(warningRegistry)
	capture.capturePresent = captureDevicePresent(warningRegistry, audio.CaptureDevicePresent)
	startPowerButton(ctx, &cfg, capture, controlHooks, warningRegistry)
	startJabraMuteButton(ctx, &cfg, capture, micDevice, controlHooks, warningRegistry)

	envoyEvents.configure(ctx, &cfg, appLogger)
	envoyEvents.publishRecoveredEnds(recoveredIDs)
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

		envoyEvents.configure(ctx, newCfg, appLogger)
	})
	watchDriveSyncSettings(cfgStore, drive,
		func(cfg *config.Config) (sessionSyncer, error) {
			syncer, err := newDriveSyncer(ctx, cfg, appLogger)
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
		syncer, syncErr := newDriveSyncer(ctx, &cfg, appLogger)
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
	// After each sweep, a kept recordings pass runs (kept, above).
	// A session delete stops a save of the session's kept raw recording
	// before it erases the session's files.
	controlHooks.HoldSessionAudio = kept.Hold
	go sweepDriveSync(ctx, store, drive, warningRegistry, 5*time.Minute, kept)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				envoyEvents.sweep(ctx)
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

	client.Init(client.InitLib{LogLevel: client.LogLevelDefault})

	// micLoops holds every mic loop and the mic startup retry, so shutdown can
	// wait for them before it stops the device and terminates PortAudio.
	var micLoops sync.WaitGroup
	micWait := func(d time.Duration) { capture.sleepUnlessPaused(ctx, d) }

	// PortAudio's device scan opens capture streams on the Jabra, so it is a
	// capture open like the microphone's own: it runs only inside a
	// microphone start, under the start's red hold (startMicrophone,
	// retryMicStartup), and through micDevice, never on its own at start-up. A
	// unit that starts paused scans at its first resume. The speaker needs
	// PortAudio, so it plays only once a scan has worked.
	var paScan portAudioScan
	speakerWarning := func() {
		if cfg.SpeakerEnabled {
			paScan.warn(warningRegistry)
		}
	}
	scanPortAudio := func(updateStartupState bool, scan func() error) error {
		err := micDevice.probe(scan)
		paScan.finished(err == nil)
		speakerWarning()
		if err != nil {
			log.Printf("warning: portaudio init failed: %v — microphone unavailable", err)
			warningRegistry.Set("microphone", "Microphone unavailable — recording and live transcription are disabled")
			if updateStartupState {
				startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "Microphone unavailable"}
			}
			return fmt.Errorf("initialize portaudio: %w", err)
		}
		return nil
	}
	startMicPipeline := func(updateStartupState bool) error {
		var (
			mic        *audio.Mic
			sampleRate int
		)

		// A pause during the scan cancels the start before any device opens:
		// the light then goes off at once, and the next resume starts again.
		if capture.isClosed() {
			return errMicStartPaused
		}
		for _, rate := range cfg.SampleRateCandidates() {
			var candidate *audio.Mic
			openErr := micDevice.openStream(func() (bool, error) {
				var err error
				candidate, err = audio.NewMic(rate, rate/4) // 250ms buffer
				return err == nil, err
			})
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
		ordered := orderedMic{micStreamer: mic, device: micDevice}

		audioRecorder.SetSampleRate(sampleRate)
		// micRef is published before the start so the controller, woken when
		// the start clears the forced-red hold, already sees the open mic.
		micRef.Store(mic)
		if err := startOpenedMic(capture, ordered, func() { micRef.Store(nil) }, log.Printf); err != nil {
			warning, status := micStartFailureMessages(err)
			if warning != "" {
				warningRegistry.Set("microphone", warning)
			}
			if updateStartupState {
				startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: status}
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
			startMicMonitoring(ctx, &micLoops, capture, ordered, audioRecorder, nil, micWait, log.Printf, hub)
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
		candidateResilient := newLiveTranscription(ctx, cfg.DeepgramAPIKey, cOptions, tOptions, resilientConfig, manager, warningRegistry, hub)
		resilientRef.Store(candidateResilient)
		deepgramSendRef.Store(startMicMonitoring(ctx, &micLoops, capture, ordered, audioRecorder, candidateResilient, micWait, log.Printf, hub))
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
				if err := scanPortAudio(false, audio.ReinitPortAudio); err != nil {
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
		var err error
		if !paScan.ready.Load() {
			err = scanPortAudio(atBoot, audio.InitializePortAudio)
		}
		if err == nil {
			err = startMicPipeline(atBoot)
		}
		if err != nil {
			slog.Warn("microphone unavailable, running API/UI only", "error", err)
		}
		return err
	}
	// booted closes once start-up has sent its component statuses.
	booted := make(chan struct{})
	deferred, err := startMicrophone(ctx, &micLoops, capture, booted, startMic, retryMic)
	switch {
	case deferred && capture.isLightHeld() && !capture.IsPaused():
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: captureLightUnavailable, Message: "Microphone closed: the recording light is not confirmed red"}
		if cfg.DeepgramAPIKey != "" {
			startupStatus.Deepgram = config.ComponentState{Name: "deepgram", Status: captureLightUnavailable, Message: "Deepgram connects once the recording light is confirmed red"}
		}
	case deferred:
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: storage.ComponentStatusPaused, Message: "Microphone closed while capture is paused"}
		if cfg.DeepgramAPIKey != "" {
			startupStatus.Deepgram = config.ComponentState{Name: "deepgram", Status: storage.ComponentStatusPaused, Message: "Deepgram connects when capture resumes"}
		}
	case errors.Is(err, errLightNotConfirmed):
		_, status := micStartFailureMessages(err)
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: status}
	case errors.Is(err, errNoCaptureDevice):
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: "unavailable", Message: "No microphone found"}
	}
	// No start has run yet when it waits for a resume, or for the light, so
	// nothing has scanned: the start that brings the microphone up does.
	speakerWarning()
	// Initialize speaker output device. Its playback streams go through
	// micDevice, so the Jabra's light never changes while one is open.
	var speaker *audio.Speaker
	if cfg.SpeakerEnabled {
		speaker = audio.NewSpeaker(cfg.SpeakerDevice)
		speaker.SetOutputHooks(micDevice.playbackOpening, micDevice.playbackClosing)
		log.Printf("speaker output enabled (device=%q)", cfg.SpeakerDevice)
	}

	// Initialize TTS orchestrator if both speaker and provider are configured.
	var ttsOrchestrator *tts.Orchestrator
	if speaker != nil && cfg.TTSProvider != "" {
		ttsProvider, ttsErr := newTTSProvider(ctx, &cfg)
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
			return ttsStatusResponse(&status), true
		}
		controlHooks.TTSWaitForSynthesis = func(ctx context.Context, id string) (map[string]any, error) {
			status, err := ttsOrchestrator.WaitForSynthesis(ctx, id)
			if err != nil {
				return nil, err
			}
			return ttsStatusResponse(&status), nil
		}
		controlHooks.TTSHasProvider = ttsOrchestrator.HasProvider
		controlHooks.TTSHasSpeaker = ttsOrchestrator.HasSpeaker
		controlHooks.TTSSpeakerWaits = paScan.speakerWaits
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
	// (5 minutes), whose ticker starts above, before the kept recordings pass,
	// which erases raw recordings, and before GC, which starts below with a
	// pass at once.
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
		recordPendingSpliceCleanupWarning(warningRegistry, err)
	}
	// The recordings a stop cut off (storage.RecoverStaleSessions ended their
	// sessions above) are saved now, not at the first sweep.
	go kept.run(ctx)
	if cfg.GCEnabled {
		go collectGarbage(ctx, cfgStore, deletionNoticeGCStore{SQLiteStore: store, hub: hub}, drive, gcHealthState, warningRegistry, appLogger, gcEvery)
	}

	// Create health checker with actual component references. With no Deepgram
	// API key it gets no Deepgram client source, so readiness never excuses
	// Deepgram, paused or not.
	var deepgramHealth interface{ IsConnected() bool }
	if cfg.DeepgramAPIKey != "" {
		deepgramHealth = &resilientClientProxy{ref: &resilientRef}
	}
	healthChecker := server.NewDefaultHealthChecker(deepgramHealth, store, micStatus, captureReadiness{capture: capture, mic: micStatus}, llmTracker)
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

	go deletion.recoverAtStart(ctx, deletionDrive, warningRegistry)

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
	cancel()
	manager.StopSessionStartPublishes()
	// Each mic loop closes its microphone and ends, even if its device
	// delivers no frame. Stopping the device or terminating PortAudio while a
	// Stream still runs crashes inside PortAudio, so neither happens before
	// they end, nor at all if one is stuck: the process exit releases the
	// device then.
	micLoopsEnded := joinMicLoops(&micLoops, micShutdownWait)
	if !micLoopsEnded {
		log.Printf("warning: microphone still reading %v after shutdown began; leaving the device and PortAudio to the process exit", micShutdownWait)
	}
	// Only now, with the microphones closed, does the light go off. Stop
	// refuses any further red confirmation, and lets the off write through
	// even if a microphone is stuck open, since the process is exiting.
	hardware.Stop()
	stopLight()
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

// micStreamer is the capture device the mic loop drives. Open opens a fresh
// stream and starts it, without re-initializing PortAudio; Reopen
// re-initializes PortAudio first, to pick up a device plugged in again. Close
// may be called from another goroutine while Stream runs, and then ends that
// Stream: a pause closes the device that way (streamUntilClosed).
type micStreamer interface {
	Stream(writer io.Writer) error
	Start() error
	Open() error
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
	// stallWait is how long the loop holds the microphone closed after no
	// audio reached the recording; it doubles while it opens to no audio, and
	// starts again once audio passes.
	stallWait := baseBackoff
	gated := shutdownWriter{ctx: ctx, dst: capture.Writer(writer)}
	// reopen is set when the microphone must close, and open again under a
	// fresh red confirmation, before more audio passes; reopenWhy says why.
	// The Jabra's ring may show the pause while capture is on (orderedMic),
	// and its light cannot change while the microphone is open; or the
	// recording light was not red at some point since the microphone opened
	// (errLightWentOff), so what the microphone still holds may have been
	// captured with the light off.
	reopen, reopenWhy := false, ""
	// released is set once the microphone is closed already, as by
	// streamUntilClosed; the loop top then closes nothing more.
	released := false
	// held is set while a pause could not release the microphone
	// (audio.ErrMicStillReading); the close that releases it clears the
	// warning.
	held := false

	for {
		if ctx.Err() != nil {
			return
		}

		var err error
		if capture.isClosed() || reopen || released {
			// No device is held while the gate is closed.
			why := reopenWhy
			if capture.isClosed() {
				why = "for capture pause"
			}
			if !released {
				closeMic(capture, streamer, why, logf)
			}
			if held {
				held = false
				capture.micHeld(false)
			}
			if capture.isClosed() && (!waitWhilePaused(ctx, capture, logf, hub) || ctx.Err() != nil) {
				return
			}
			reopen, reopenWhy, released = false, "", false
			// Before red: a resume during speech on the Jabra waits for it to
			// end, then lights the Jabra, so its ring reads right while the
			// microphone is open. A pause meanwhile cancels the resume, and
			// the loop holds the closed microphone until the next resume and
			// waits again: it never streams the microphone it closed.
			for !capture.prepareMicOpen(ctx) {
				if ctx.Err() != nil || !holdMicWhilePaused(ctx, capture, streamer, logf, hub) {
					return
				}
			}
			if err = capture.confirmMicOpen(); err != nil {
				if errors.Is(err, errNoCaptureDevice) {
					logf("no capture device present; the microphone opens once one is")
				} else {
					logf("recording light unavailable before microphone restart: %v", err)
					capture.lightUnconfirmed(err)
				}
				capture.finishResume()
				wait(backoff)
				backoff = min(backoff*2, maxBackoff)
				// The microphone is closed: open it again, without closing it
				// again, once the wait is over.
				released = true
				continue
			}
			err = streamer.Open()
			capture.finishMicStart()
			// This start, whether it works or not, ends the resume's
			// "resuming" state: readiness then reports what came back.
			capture.finishResume()
			if err == nil {
				backoff = baseBackoff
				capture.stalled.Store(false)
				logf("microphone reopened: capture resumed")
				if hub != nil {
					hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone capture resumed")
				}
				continue
			}
			// The device did not come back (unplugged during the pause). A
			// failed Open holds no device; reconnect below.
			released = true
		} else {
			armed := capture.audioAt.Load()
			var closed micClose
			closed, err = streamUntilClosed(ctx, capture, streamer, gated, logf)
			if capture.audioAt.Load() != armed {
				stallWait = baseBackoff // audio passed
			}
			if closed.held {
				// Once Stream has returned, the loop top closes the device.
				held, reopen, reopenWhy = true, true, "once its read returned"
			}
			released = closed.released
			if err == nil || ctx.Err() != nil {
				return
			}
			if closed.stalled && !capture.isClosed() {
				// The microphone is closed, and the light off, until the wait
				// is over; then the loop top opens it again under red.
				if hub != nil {
					hub.BroadcastComponentStatus("mic", captureNoAudio, "No audio reached the recording; the microphone is closed and opens again")
				}
				wait(stallWait)
				stallWait = min(stallWait*2, maxBackoff)
				continue
			}
			// A pause, or a device error that coincides with one: close the
			// device at the loop top instead of reporting a failure.
			if released || held || errors.Is(err, errGateClosed) || capture.isClosed() {
				continue
			}
			if errors.Is(err, errRingShowsPause) {
				logf("the Jabra's ring may show the pause while capture is on; reopening the microphone")
				reopen, reopenWhy = true, "while the Jabra's ring may show the pause"
				continue
			}
			if errors.Is(err, errLightWentOff) {
				logf("the recording light was not red while the microphone was open; reopening the microphone")
				reopen, reopenWhy = true, "after the recording light was not red"
				continue
			}
		}

		// The stream failed, or did not open, and the loop waits before it
		// reconnects: the microphone is closed first, so the light, which goes
		// off with it, is never off while it is open. A failed Open holds no
		// device, so it needs no close.
		if !released {
			why := "after a stream error"
			if isOverflow(err) {
				why = "after an overflow"
			}
			closeMic(capture, streamer, why, logf)
		}
		released = true // the loop top, after a pause, closes nothing more
		if isOverflow(err) {
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
			// The loop top streams again, whatever the reopen did, unless the
			// light refused it: the device is closed then, and the loop top
			// opens it again under a new confirmation.
			confirmed, _ := reopenMic(capture, streamer, logf, hub)
			released = !confirmed
			continue
		}

		logf("mic stream error (retrying in %v): %v", backoff, err)
		if hub != nil {
			// The error can be the write to the session's raw file, which
			// names it; this status names no session and is stored, so it
			// says only that the stream failed. The log line says why.
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, "Microphone stream error; retrying")
		}
		wait(backoff)

		if ctx.Err() != nil {
			return
		}
		if capture.isClosed() {
			continue // never reopen the mic while paused; the loop top waits
		}
		confirmed, reopenErr := reopenMic(capture, streamer, logf, hub)
		switch {
		case !confirmed:
		case reopenErr != nil:
			backoff = min(backoff*2, maxBackoff)
		default:
			backoff = baseBackoff
		}
		released = !confirmed
	}
}

// isOverflow reports whether err is PortAudio's input overflow.
func isOverflow(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "overflow")
}

// micClose is how streamUntilClosed closed the microphone while it streamed:
// released when the close released it, held when it could not
// (audio.ErrMicStillReading), and stalled when it closed it because no audio
// reached the recording.
type micClose struct{ released, held, stalled bool }

// streamUntilClosed runs streamer's Stream into w and closes streamer from
// another goroutine as soon as the gate closes (a pause, or a hold for the
// light), shutdown begins (ctx ends), or no audio has reached the recording
// for the gate's audioStall since it last did, or since the start armed it.
// A Stream whose read blocks, on a device that delivers no frame, writes
// nothing, so neither errGateClosed nor shutdownWriter reaches it: without
// that close, the device would stay open until the read returned or the
// process exited, and the light, which stays red while it is open, with it.
// A close that cannot release the device (audio.ErrMicStillReading) reports
// the microphone held (captureGate.micHeld) until the mic loop closes it once
// Stream has returned. After a close, the light is woken, so it goes off.
func streamUntilClosed(ctx context.Context, capture *captureGate, streamer micStreamer, w io.Writer, logf func(string, ...any)) (micClose, error) {
	closing := capture.closingSignal()
	streamed := make(chan struct{})
	result := make(chan micClose, 1)
	go func() {
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		var closed micClose
	watch:
		for {
			var stall <-chan time.Time
			if at := capture.audioAt.Load(); at != 0 {
				left := time.Until(time.Unix(0, at).Add(capture.audioStall))
				if left <= 0 {
					closed.stalled = true
					break
				}
				timer.Reset(left)
				stall = timer.C
			}
			select {
			case <-streamed:
				result <- closed
				return
			case <-closing:
				break watch
			case <-ctx.Done():
				break watch
			case <-stall:
			}
		}
		why := "for capture pause"
		if closed.stalled {
			why = fmt.Sprintf("after no audio reached the recording for %v", capture.audioStall)
			logf("no audio reached the recording for %v; closing the microphone", capture.audioStall)
			capture.stalled.Store(true)
		}
		switch err := streamer.Close(); {
		case errors.Is(err, audio.ErrMicStillReading):
			logf("warning: the microphone is still reading; it stays open, with the light red, until its read returns")
			capture.micHeld(true)
			closed.held = true
		case err != nil:
			logf("warning: close microphone %s: %v", why, err)
			closed.released = true
		default:
			closed.released = true
		}
		capture.micClosed()
		result <- closed
	}()
	err := streamer.Stream(w)
	close(streamed)
	return <-result, err
}

// The speaker's warnings, which GET /api/status lists, and the reasons POST
// /api/tts/speak refuses with, while PortAudio, which the speaker needs, is not
// up (portAudioScan.speakerWaits).
const (
	speakerWaitsForScanWarning = "Speaker unavailable until the microphone starts: the PortAudio device scan it needs opens the microphone too, so a unit that starts paused speaks only after its first resume"
	speakerNoPortAudioWarning  = "Speaker unavailable — PortAudio initialization failed"
)

// portAudioScan is whether PortAudio's device scan has run, and whether the
// last one worked. HTTP handlers read it while a microphone start scans.
type portAudioScan struct{ scanned, ready atomic.Bool }

// finished records a scan that worked or failed.
func (s *portAudioScan) finished(worked bool) {
	s.scanned.Store(true)
	s.ready.Store(worked)
}

// speakerWaits returns why the speaker cannot play yet, or "" once a scan has
// worked: no microphone start has scanned yet, as in a unit that starts
// paused, or the last scan failed.
func (s *portAudioScan) speakerWaits() string {
	switch {
	case s.ready.Load():
		return ""
	case s.scanned.Load():
		return speakerNoPortAudioWarning
	default:
		return speakerWaitsForScanWarning
	}
}

// warn sets the speaker's warning while it cannot play, and clears it once it
// can.
func (s *portAudioScan) warn(warnings *warningsRegistry) {
	if reason := s.speakerWaits(); reason != "" {
		warnings.Set("speaker", reason)
		return
	}
	warnings.Clear("speaker")
}

// micMissingWarningKey is the warning GET /api/status shows while no capture
// device is present.
const micMissingWarningKey = "microphone-missing"

// captureDevicePresent is the capture gate's capturePresent: present looks for
// a capture device without opening one, and the status warns while there is
// none, which is also while the light stays off for it.
func captureDevicePresent(warnings *warningsRegistry, present func() bool) func() bool {
	return func() bool {
		if present() {
			warnings.Clear(micMissingWarningKey)
			return true
		}
		warnings.Set(micMissingWarningKey, "No microphone found: the unit does not record, and the light stays off, until one is plugged in")
		return false
	}
}

// micHeldWarningKey is the warning GET /api/status shows while a pause could
// not release the microphone.
const micHeldWarningKey = "microphone-held"

// micHeldWarning is the capture gate's onMicHeld for warnings.
func micHeldWarning(warnings *warningsRegistry) func(held bool) {
	return func(held bool) {
		if held {
			warnings.Set(micHeldWarningKey, "Microphone still open after a pause: a call into PortAudio does not return. The light stays red while it is open, and nothing is recorded; restart the unit to close it")
			return
		}
		warnings.Clear(micHeldWarningKey)
	}
}

// reopenMic reopens the device once a capture device is present and the
// recording light is confirmed red, and reports each step on hub. It returns
// false, having closed the device and reopened nothing, when the light cannot
// be confirmed red; otherwise true and the reopen's error, which is
// errNoCaptureDevice, with the device closed and the light left off, while no
// capture device is present.
func reopenMic(capture *captureGate, streamer micStreamer, logf func(string, ...any), hub *server.Hub) (bool, error) {
	if hub != nil {
		hub.BroadcastComponentStatus("mic", storage.ComponentStatusReconnecting, "Attempting to reconnect microphone...")
	}
	if err := capture.confirmMicOpen(); errors.Is(err, errNoCaptureDevice) {
		logf("no capture device present; the microphone reconnects once one is")
		closeMic(capture, streamer, "with no capture device present", logf)
		if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, "No microphone found")
		}
		return true, err
	} else if err != nil {
		logf("recording light unavailable before microphone reconnect: %v", err)
		capture.lightUnconfirmed(err)
		closeMic(capture, streamer, "without a red recording light", logf)
		return false, nil
	}
	err := streamer.Reopen()
	capture.finishMicStart()
	if err != nil {
		logf("mic reopen failed: %v", err)
		if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, err.Error())
		}
		return true, err
	}
	capture.stalled.Store(false)
	logf("mic reopened successfully")
	if hub != nil {
		hub.BroadcastComponentStatus("mic", storage.ComponentStatusConnected, "Microphone reconnected")
	}
	return true, nil
}

// closeMic closes the capture device, so it holds no input device, and wakes
// the light, which stays red while the device is open: while capture is
// paused, before the loop waits after a stream error, and when the light
// cannot be confirmed red before a reconnect.
func closeMic(capture *captureGate, streamer micStreamer, which string, logf func(string, ...any)) {
	if err := streamer.Close(); err != nil {
		logf("warning: close microphone %s: %v", which, err)
	}
	capture.micClosed()
}

// holdMicWhilePaused closes the capture device, which releases it, and blocks
// until capture resumes (waitWhilePaused). It reports false if ctx ends first.
func holdMicWhilePaused(ctx context.Context, capture *captureGate, streamer micStreamer, logf func(string, ...any), hub *server.Hub) bool {
	closeMic(capture, streamer, "for capture pause", logf)
	return waitWhilePaused(ctx, capture, logf, hub)
}

// waitWhilePaused reports the closed microphone and blocks until capture
// resumes. It reports false if ctx ends first.
func waitWhilePaused(ctx context.Context, capture *captureGate, logf func(string, ...any), hub *server.Hub) bool {
	logf("microphone closed: capture paused")
	if hub != nil {
		if capture.isLightHeld() && !capture.IsPaused() {
			hub.BroadcastComponentStatus("mic", captureLightUnavailable, "Microphone closed: the recording light is not confirmed red")
		} else {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusPaused, "Microphone closed while capture is paused")
		}
	}
	return capture.waitResumed(ctx)
}
