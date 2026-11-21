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
	"path/filepath"
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
	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/tts"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"

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
// the page offers Retry refinement rather than waiting forever. A refinement
// the stop cut off did not fail: it is left as it is, for the next start
// (stopping.ErrStopped).
func newMergedRefinementFailureRecorder(store *storage.SQLiteStore) func(context.Context, string, error) {
	return func(ctx context.Context, sessionID string, cause error) {
		if stopping.CutOff(ctx) {
			slog.Info("merged refinement cut off by the stop; the next start takes it up", "session_id", sessionID, "cause", cause)
			return
		}
		generation, err := store.DeletionGeneration(sessionID)
		if err == nil {
			err = store.UpdateRefinementAtGeneration(sessionID, "", storage.RefinementFailed, "", generation)
		}
		if err != nil {
			slog.Warn("record failed merged refinement", "session_id", sessionID, "cause", cause, "error", err)
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
// hardware before the start opens the microphone, and startUnderRed runs the
// start with the recording light confirmed and held red, then ends a resume's
// "resuming" state unless a pause cancelled the start.
type resumeWaiter interface {
	waitResumed(ctx context.Context) bool
	prepareMicOpen(ctx context.Context) bool
	startUnderRed(start func() error) error
}

// retryMicStartup runs start every retryInterval until it succeeds or ctx
// ends. A paused instance opens no input device: a retry that comes due
// while capture is paused waits (capture.waitResumed) until capture resumes.
// Each attempt runs under a red hold (startUnderRed): the PortAudio scan and the
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
		// attempt; the next comes once capture resumes, without the interval.
		if !capture.prepareMicOpen(ctx) {
			wait = false
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

		err := capture.startUnderRed(start)
		if errors.Is(err, errMicStartPaused) {
			// A pause cancelled the attempt: the next resume tries again, and
			// that attempt ends the resume's "resuming" state.
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
// (startUnderRed): the light is confirmed red before it, and held red until it
// returns, so every capture stream it opens, the PortAudio scan's included,
// opens while the light last read red. A paused instance opens no input device
// until it resumes: while the capture gate is closed, start runs in loops once
// the gate opens and the hardware is ready for it (prepareMicOpen), with atBoot
// false, and startMicrophone returns deferred true. A pause while it waits for
// the hardware, or one that cancels the start before it opens a device
// (errMicStartPaused), cancels that resume, and the next resume starts the
// microphone. Otherwise start runs at once with atBoot true, so it can write
// the start-up status, and startMicrophone returns its error: one that wraps
// errLightNotConfirmed if the light refused it, or errNoCaptureDevice, and
// start did not run. Every start, whether it works or not, ends the "resuming"
// state of a resume that came before or during it, unless a pause cancelled
// it: that resume is then resuming until the start after it finishes.
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
			err := capture.startUnderRed(func() error { return start(false) })
			if errors.Is(err, errMicStartPaused) {
				continue
			}
			if err != nil {
				retry()
			}
			return
		}
	}
	if capture.isClosed() {
		afterBoot(startOnResume)
		return true, nil
	}
	err = capture.startUnderRed(func() error { return start(true) })
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
	voice   voiceListener
	stamp   func() (closings int, open bool)
	// keyterms are the live stream's keyterms: each dial sends them, and a
	// join or a leave changes them on the open socket. nil sends tOptions'
	// own.
	keyterms *transcript.LiveKeyterms
}

// voiceListener is the voice as the transcript reader feeds it: the
// voice.Supervisor.
type voiceListener interface {
	OnTranscript(t *voice.Transcript)
	OnUtteranceEnd(closings int, gateOpen bool)
}

// keptMessager is a session manager that reports what of each result its
// transcript keeps (session.Manager.MessageKept): words from before the latest
// pause are dropped, and the voice reads exactly what is left.
type keptMessager interface {
	MessageKept(mr *api.MessageResponse) (*api.MessageResponse, error)
}

// The build fails if the session manager stops reporting what its transcript
// keeps: the voice would then read every live result, a late wake from before
// a pause included (contract 4, R4).
var _ keptMessager = (*session.Manager)(nil)

// Message hands a Deepgram result to the session manager, then what the
// manager's transcript kept of it to the voice, with the capture gate's stamp
// taken as the result arrived.
func (c transcriptCallback) Message(mr *api.MessageResponse) error {
	var closings int
	open := true
	if c.stamp != nil {
		closings, open = c.stamp()
	}
	kept := mr
	var err error
	switch manager := c.manager.(type) {
	case nil:
	case keptMessager:
		kept, err = manager.MessageKept(mr)
	default:
		err = manager.Message(mr)
	}
	if c.voice != nil && kept != nil {
		if t := voiceTranscript(kept, func() (int, bool) { return closings, open }); t != nil {
			c.voice.OnTranscript(t)
		}
	}
	return err
}

// voiceTranscript is mr as the voice reads it, or nil when it holds no text.
func voiceTranscript(mr *api.MessageResponse, stamp func() (int, bool)) *voice.Transcript {
	if len(mr.Channel.Alternatives) == 0 || strings.TrimSpace(mr.Channel.Alternatives[0].Transcript) == "" {
		return nil
	}
	alt := &mr.Channel.Alternatives[0]
	t := &voice.Transcript{
		Text:         alt.Transcript,
		Words:        make([]voice.Word, 0, len(alt.Words)),
		IsFinal:      mr.IsFinal,
		SpeechFinal:  mr.SpeechFinal,
		FromFinalize: mr.FromFinalize,
		At:           time.Now(),
	}
	if stamp != nil {
		t.Closings, t.GateOpen = stamp()
	}
	for i := range alt.Words {
		w := &alt.Words[i]
		speaker := -1
		if w.Speaker != nil {
			speaker = *w.Speaker
		}
		text := w.PunctuatedWord
		if text == "" {
			text = w.Word
		}
		t.Words = append(t.Words, voice.Word{Text: text, Speaker: speaker, Start: w.Start, End: w.End})
	}
	return t
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
	var closings int
	open := true
	if c.stamp != nil {
		closings, open = c.stamp()
	}
	var err error
	if c.manager != nil {
		err = c.manager.UtteranceEnd(ur)
	}
	if c.voice != nil {
		c.voice.OnUtteranceEnd(closings, open)
	}
	return err
}

func (c transcriptCallback) Close(*api.CloseResponse) error {
	log.Println("disconnected from Deepgram")
	if mgr, ok := c.manager.(*session.Manager); ok {
		mgr.OnTranscriptionDisconnect()
	}
	return nil
}

// Error logs an Error message Deepgram sent: its code and variant, never its
// text, which can quote what it heard. The socket logs its own read errors
// itself (deepgramSocket.read).
func (c transcriptCallback) Error(er *api.ErrorResponse) error {
	slog.Warn("deepgram error", "provider", "deepgram", "code", providererr.Token(er.ErrCode), "variant", providererr.Token(er.Variant))
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

// newSessionRecorder is the app's recorder: it keeps the sample rate of each
// session's raw recording with the session in store, so a raw recording kept
// after a stop inside its session is saved at its true duration.
func newSessionRecorder(audioDir string, store audio.SessionRates) *audio.Recorder {
	recorder := audio.NewRecorder(audioDir)
	recorder.UseSessionRates(store)
	return recorder
}

// startMicMonitoring starts the mic loop. Live transcription, downstream,
// gets each buffer just before the recorder, through a bounded queue
// that never blocks the loop (transcribe.AsyncSender), so no Deepgram
// trouble stops the recording. tap, the voice's room audio (voice.Tap), gets
// each buffer after them, behind the capture gate like them; it must never
// block. It returns the Deepgram queue, whose Finalize the capture pause
// uses, or nil without downstream.
func startMicMonitoring(ctx context.Context, loops *sync.WaitGroup, capture *captureGate, streamer micStreamer, recorder *audio.Recorder, downstream interface {
	io.Writer
	Finalize() error
}, tap io.Writer, wait func(time.Duration), logf func(string, ...any), hub *server.Hub) *transcribe.AsyncSender {
	if streamer == nil || recorder == nil {
		return nil
	}
	var sender *transcribe.AsyncSender
	send := io.Discard
	if downstream != nil {
		sender = transcribe.NewAsyncSender(ctx, downstream, deepgramSendChunks, logf)
		send = sender
	}
	writer := recorder.Writer(send)
	if tap != nil {
		writer = tappedWriter{dst: writer, tap: tap}
	}
	streamMicLoopStarter(ctx, loops, capture, streamer, writer, wait, logf, hub)
	return sender
}

// tappedWriter writes each buffer to dst, then to tap, and reports dst's
// result. When the capture gate stamps a Write, it drops the tap copy if a
// later gate closing made that in-flight write old before dst returned.
type tappedWriter struct {
	dst, tap io.Writer
}

func (w tappedWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	_, _ = w.tap.Write(p)
	return n, err
}

func (w tappedWriter) WriteWithGateStamp(p []byte, gate *captureGate, closings int) (int, error) {
	n, err := w.dst.Write(p)
	gate.writeTapIfCurrent(w.tap, p, closings)
	return n, err
}

// liveTranscriptionOptions are the live Deepgram request's options for a
// microphone at sampleRate, with Dispatch's project keys as keyterms.
func liveTranscriptionOptions(cfg *config.Config, sampleRate int, projects []string) *interfaces.LiveTranscriptionOptions {
	return &interfaces.LiveTranscriptionOptions{
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
		Keyterm:        voiceKeyterms(cfg, projects),
		VadEvents:      true,
	}
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
// app keeps (keptRecordings.census), every interval until ctx ends or the stop
// refuses a pass: each pass reads the store, as work the stop waits for
// (runPasses). GET /api/status reads the cache, so a stuck disk or a hung
// recording write cannot hang the status. The status warns once they take
// more than a tenth of the audio budget, which they are not counted against,
// or as soon as there is one when there is no budget (0): GC never removes
// them.
func refreshKeptRawRecordings(ctx context.Context, background backgroundWork, kept *keptRecordings, budgetMB func() int, cache *atomic.Pointer[server.KeptRawRecordings], interval time.Duration) {
	runPasses(ctx, background, interval, func(context.Context) {
		census := kept.census()
		if budget, limited := gc.AudioBudgetBytes(budgetMB()); limited {
			census.WarnBytes = budget / 10
		}
		cache.Store(&census)
	})
}

// startEmbeddingBackfill indexes, in the background and as work the stop cuts
// off and waits for, the sessions with no embeddings
// (embedding.Indexer.BackfillMissing).
func startEmbeddingBackfill(ctx context.Context, background backgroundWork, indexer *embedding.Indexer) {
	go func() {
		_ = background.Run(ctx, func(ctx context.Context) error {
			if err := indexer.BackfillMissing(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("embedding backfill failed", "error", err)
			}
			return nil
		})
	}()
}

// makeDeepgramClientFactory dials live-transcription sockets the app owns
// (deepgramSocket), each write bounded by writeTimeout. Each dial carries the
// live keyterms as they are then, and is the socket a join's Configure goes
// to until the next dial.
func makeDeepgramClientFactory(_ context.Context, apiKey string, cOptions *interfaces.ClientOptions, tOptions *interfaces.LiveTranscriptionOptions, callback transcriptCallback, writeTimeout time.Duration) transcribe.ClientFactory {
	return func(factoryCtx context.Context) (io.WriteCloser, error) {
		options := tOptions
		var keyterms []string
		if callback.keyterms != nil {
			keyterms = callback.keyterms.List()
			withKeyterms := *tOptions
			withKeyterms.Keyterm = keyterms
			options = &withKeyterms
		}
		socket, err := dialDeepgramLive(factoryCtx, apiKey, cOptions, options, callback, writeTimeout)
		if err != nil {
			return nil, err // never a nil *deepgramSocket in the interface
		}
		if callback.keyterms != nil {
			callback.keyterms.Attach(socket, keyterms)
		}
		return socket, nil
	}
}

type mergedSessionHookStore interface {
	GetSession(string) (storage.Session, error)
	ClaimRefinement(id, audioPath string) (generation int, claimed bool, err error)
}

// onSessionMerged refines the session a merge made, then summarizes it. A
// merge that took a recording removed from this unit (audio_removed_at)
// restores it first, through retryRefinement; the refinement is claimed
// before that, so a stop during the restore leaves it running, which the next
// start retries once (storage.SQLiteStore.RecoverStaleRefinements). Left
// pending, nothing would: no recovery lists a pending refinement, and the
// page offers no Retry for one.
func onSessionMerged(ctx context.Context, store mergedSessionHookStore, refine func(context.Context, string, string, time.Time) error, summarize func(context.Context, string, time.Time) error, retryRefinement func(context.Context, string) error, recordRefinementFailure func(context.Context, string, error), sessionID string) {
	merged, err := store.GetSession(sessionID)
	if err != nil {
		slog.Warn("read merged session before refinement", "session_id", sessionID, "error", err)
		return
	}
	if merged.AudioRemovedAt != "" {
		if merged.AudioPath != "" {
			if _, _, err := store.ClaimRefinement(sessionID, merged.AudioPath); err != nil {
				slog.Warn("claim merged session's refinement before restoring its recordings", "session_id", sessionID, "error", err)
			}
		}
		if err := retryRefinement(ctx, sessionID); err != nil {
			recordRefinementFailure(ctx, sessionID, err)
			if err := summarize(ctx, sessionID, merged.StartedAt); err != nil {
				slog.Warn("summarize failed merged session", "session_id", sessionID, "error", err)
			}
		}
		return
	}
	if merged.AudioPath != "" {
		if err := refine(ctx, sessionID, merged.AudioPath, merged.StartedAt); err == nil {
			return
		} else {
			slog.Warn("refine merged session", "session_id", sessionID, "error", err)
		}
	}
	if err := summarize(ctx, sessionID, merged.StartedAt); err != nil {
		slog.Warn("summarize merged session", "session_id", sessionID, "error", err)
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

// newTTSProvider builds the TTS provider cfg names. geminiOptions go to the
// Gemini provider: ghost-wispr tts-first-audio's tests point it at a fake.
func newTTSProvider(ctx context.Context, cfg *config.Config, geminiOptions ...tts.GeminiOption) (tts.Provider, error) {
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
		return tts.NewGeminiProvider(cfg.GCPProject, cfg.TTSModel, cfg.TTSVoice, append([]tts.GeminiOption{tts.WithGeminiAuthCredentials(credentials)}, geminiOptions...)...)
	default:
		return nil, fmt.Errorf("unsupported TTS provider: %s", cfg.TTSProvider)
	}
}

func main() {
	// Before any Google client is built: its own logger writes around the
	// app's (logging.WithholdGoogleSDKLogging).
	googleSDKLogging := logging.WithholdGoogleSDKLogging()
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
	if len(os.Args) > 1 && os.Args[1] == "kws-selftest" {
		os.Exit(runKWSSelfTest(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "speaker-trace" {
		os.Exit(runSpeakerTraceCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "tts-first-audio" {
		os.Exit(runTTSFirstAudioCommand(context.Background(), os.Args[1:], configPath, nil, os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "play-sound" {
		// Its stderr is for why it failed: the speaker stream's notes would
		// crowd the cause out of the 300 bytes accept-voice.sh keeps.
		logging.SetDefault(logging.New(os.Stderr, slog.LevelWarn))
		os.Exit(runPlaySoundCommand(context.Background(), os.Args[1:], configPath, nil, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "issue-holder" {
		// Its stderr is for why it failed: the scratch ledger's start-up notes
		// would crowd the cause out of the 300 bytes accept-voice.sh keeps.
		logging.SetDefault(logging.New(os.Stderr, slog.LevelWarn))
		os.Exit(runIssueHolderCommand(context.Background(), os.Args[1:], configPath, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "joined-probe" {
		logging.SetDefault(logging.New(os.Stderr, slog.LevelWarn))
		os.Exit(runJoinedProbeCommand(context.Background(), os.Args[1:], configPath, os.Stdout, os.Stderr))
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
	appLogger, logSink := startAppLogger(cfgStore, os.Stdout)
	if googleSDKLogging {
		slog.Warn("GOOGLE_SDK_GO_LOGGING_LEVEL is ignored: Google's SDK logging writes each request whole, which can hold a meeting's words")
	}
	for _, w := range cfgWarnings {
		slog.Warn("config", "warning", w)
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
	// The recording light's pattern check saves the black pattern at each
	// start unless a save recorded here is less than a day old, so a recorder
	// restarted over and over does not wear the light's flash: the check
	// records each save's attempt here before it sends it.
	indicator.RecordSavesIn(filepath.Join(filepath.Dir(dbPath), "blink1-saves"))

	// Recover any sessions left as 'active' from a previous crash/restart:
	// each ends when its raw recording was last written.
	recoveredIDs, err := store.RecoverStaleSessions(cfg.AudioDir)
	if err != nil {
		slog.Warn("failed to recover stale sessions", "error", err)
	} else if len(recoveredIDs) > 0 {
		log.Printf("recovered %d stale session(s): %v", len(recoveredIDs), recoveredIDs)
		cancelRecoveredQuestions(store, recoveredIDs)
	}
	// A refinement left running by the previous run stopped with it. The store
	// owes each one automatic retry, which runs below, once the recording
	// directories are cleaned up; the stop ends with it (stopRecovery).
	recovery := recoverAtStart(store)
	// So did a Drive sync: retry it in the folder it had.
	if ids, err := store.RecoverInterruptedSyncs(gdrive.MaxSyncRetries, time.Now().UTC()); err != nil {
		slog.Warn("failed to recover interrupted Drive syncs", "error", err)
	} else if len(ids) > 0 {
		log.Printf("recovered %d interrupted Drive sync(s): %v", len(ids), ids)
	}

	// Backfill sessions with empty titles.
	emptyTitleSessions, err := store.GetSessionsWithEmptyTitles()
	if err != nil {
		slog.Warn("failed to query sessions with empty titles", "error", err)
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
				slog.Warn("failed to backfill title", "session_id", sess.ID, "error", err)
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
	audioRecorder := newSessionRecorder(cfg.AudioDir, store)

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
			slog.Warn("embedding indexer disabled", "error", err)
		} else if embeddingClient, err = embedding.NewClient(cfg.EmbeddingModel, embedding.WithGenAIConfig(embedding.GenAIConfig{
			Backend:     embeddingOptions.Backend,
			Project:     embeddingOptions.Project,
			Location:    embeddingOptions.Location,
			Credentials: embeddingOptions.Credentials,
		})); err != nil {
			slog.Warn("embedding indexer disabled", "error", err)
		} else {
			indexer = embedding.NewIndexer(embeddingClient, store, 500)
			indexer.SetModel(cfg.EmbeddingModel)
		}
	}

	manager := session.NewManager(store, audioRecorder, sessionSummarizer, hub, detector, cfg.MinSessionSegments, appLogger)
	if indexer != nil {
		manager.SetIndexer(indexer)
	}
	// The meeting voice and its Concierge, once they run, drop what an ended
	// session still waits on and forget a deleted session. The voice link is
	// wired to the capture gate below, before anything can close it.
	var conciergeSessionHooks conciergeHooks
	var voiceHooks voiceLink
	var joinedLink joinedHooks // the joined agents' roster, once the voice has a speaker
	sessionsForgotten := wireVoiceSessionHooks(manager, &conciergeSessionHooks, &voiceHooks, &joinedLink)

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

	// ctx ends at shutdown; the capture gate's pause and the mic loops stop
	// with it, and work it cuts off reads as stopped (newAppContext).
	ctx, stopApp := newAppContext()
	defer stopApp()

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
	// A refused resume saves the pause state from the gate's opening, which
	// runs as work the stop waits for.
	capture.background = manager
	// The meeting voice. Its link is wired now, so every gate closing reaches
	// it, the one restorePause may make included; the voice itself starts
	// once the speaker output exists, below. Its tap is one more writer
	// behind the gate, while the unit has a speaker for a voice.
	wireVoiceToCapture(capture, &voiceHooks, &conciergeSessionHooks)
	var voiceTap *voice.Tap
	var voiceAudio io.Writer // nil with no speaker: the mic writes nothing more
	if cfg.SpeakerEnabled {
		voiceTap = voice.NewTap()
		voiceAudio = voiceTap
	}

	restoredState, err := restorePause(store, capture)
	var restorePauseErr error
	if err != nil {
		// The recorder must not start capturing when it cannot establish the
		// prior consent state.
		restorePauseErr = err
		slog.Warn("restore capture pause failed; starting paused", "error", err)
		capture.Pause()
		restoredState = micClosed
	}
	restoredPause := restoredState != micRecording
	warningRegistry := newWarningsRegistry()
	for i, warning := range cfgWarnings {
		warningRegistry.Set(fmt.Sprintf("config-%03d", i), warning)
	}
	if err := refreshRawEraseReservationWarnings(store, warningRegistry); err != nil {
		slog.Warn("raw erase reservations", "error", err)
	}
	startCertificateExpiryChecks(ctx, cfgStore, warningRegistry, certificateExpiryCheckEvery, googleauth.CertExpiry)
	// Dispatch's project keys: keyterms of the live transcript, and the
	// projects whose spoken keys the voice writes as keys (R4).
	var dispatchProjects projectKeys
	if cfg.SpeakerEnabled && cfg.DispatchToken != "" && cfg.Concierge.DispatchURL != "" {
		if client, err := dispatch.NewClient(cfg.Concierge.DispatchURL, cfg.DispatchToken, newDispatchHTTP(appLogger)); err != nil {
			slog.Warn("Dispatch's project keys", "error", err)
		} else {
			watchProjectKeys(ctx, client.Projects, projectKeysEvery, &dispatchProjects)
		}
	}
	// The live transcript's keyterms, which a join or a leave changes on the
	// open Deepgram stream (R4, R17); nil when the voice cannot run.
	liveKeyterms := newLiveKeyterms(&cfg, dispatchProjects.Get)
	if liveKeyterms != nil {
		// A keyterm list Deepgram refused shows on /api/status until one is taken.
		liveKeyterms.SetRefusalHook(keytermsRefusedWarning(warningRegistry))
	}
	if restorePauseErr != nil {
		warningRegistry.Set(captureStateWarningKey, "Capture pause state could not be restored; the recorder started paused")
	}
	if batchTranscriber, err := makeBatchTranscriber(&cfg); err != nil {
		slog.Warn("batch transcription disabled", "error", err)
		warningRegistry.Set("batch-transcription", "Batch transcription unavailable — using streaming transcript fallback")
	} else {
		manager.SetBatchTranscriber(batchTranscriber)
	}

	authToken := os.Getenv("GHOST_WISPR_AUTH_TOKEN")

	server.SetVersionInfo(server.VersionInfo{Version: Version, Commit: Commit, BuildTime: BuildTime})
	drive := &driveSync{}
	envoyEvents := &envoyPublishing{store: store, manager: manager, warnings: warningRegistry}
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
			return "", 0, fmt.Errorf("create batch job: %w", providererr.From("gemini", err))
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
			return nil, fmt.Errorf("get batch job: %w", providererr.From("gemini", err))
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
				slog.Warn("summary batch completed without inline results; marking submitted sessions failed", "batch", batchName)
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
			// Gemini's message can quote the requests, the transcripts: the
			// answer keeps the error's code's name.
			errMsg := "unknown error"
			if job.Error != nil && job.Error.Code != nil {
				errMsg = "the batch job failed: " + providererr.RPC("gemini", *job.Error.Code, job.Error.Message).Error()
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
	go refreshKeptRawRecordings(ctx, manager, kept, func() int { return cfgStore.Get().GCMaxAudioSizeMB }, &keptRaw, keptRawRefreshEvery)
	gcHealthState := &gcHealth{}
	gcHealthState.countUnverified(store)

	// refusedCuts keeps a refused "Delete text and audio" on /api/status, in
	// the store, while its segment is there, naming the session and the
	// segment's time.
	refusedCuts := newRefusedCuts(store)
	deletion, err := newAppDeleteRunner(store, cfgStore, manager, manager.RefineSession, manager.SummarizeSession, refusedCuts, warningRegistry, appLogger)
	if err != nil {
		panic(fmt.Sprintf("segment deletion: %v", err))
	}
	var presetSummaries presetSummarizer
	var mergedSummarizer session.ReportedSummarizer
	if summarizer != nil {
		presetSummaries, mergedSummarizer = summarizer, summarizer
	}

	controlHooks := &server.ControlHooks{
		IsPaused: capture.IsPaused,
		// Pause, Close and Resume: wirePauseControls, below.
		IsCapturing: func() bool { return capture.capturing(micStatus) },
		RefreshRawEraseWarnings: func() {
			if err := refreshRawEraseReservationWarnings(store, warningRegistry); err != nil {
				slog.Warn("raw erase reservations after delete failure", "error", err)
			}
		},
		CaptureState:  func() string { return capture.captureState(micStatus) },
		ActiveSession: manager.ActiveSession,
		// Warnings, DeleteSegment and DeleteSegmentText: wireSegmentDeletion,
		// below.
		OnSessionDeleted: func(sessionIDs []string) {
			sessionsDeletedHook(store, warningRegistry)(sessionIDs)
			sessionsForgotten(sessionIDs)
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
		Presets: func() map[string]config.Preset {
			if summarizer == nil {
				return nil
			}
			return summarizer.Presets()
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
	}
	wireStoreWork(controlHooks, &storeWork{
		manager:                       manager,
		store:                         store,
		drive:                         drive,
		deletion:                      deletion,
		summarizer:                    presetSummaries,
		mergedSummarizer:              mergedSummarizer,
		hub:                           hub,
		publishSummaryReady:           publishSummaryReady,
		recordMergedRefinementFailure: newMergedRefinementFailureRecorder(store),
		restoreFromDrive: func(ctx context.Context) (*gdrive.RestoreResult, error) {
			latestCfg := cfgStore.Get()
			if latestCfg.GDriveFolderID == "" {
				return nil, fmt.Errorf("google drive folder ID not configured")
			}
			syncer, err := newDriveSyncer(ctx, &latestCfg, appLogger)
			if err != nil {
				return nil, err
			}
			return syncer.RestoreFromDrive(ctx, importRestoredDriveSession(store, func() time.Time { return time.Now().UTC() }))
		},
		submitSummaryBatch: submitSummaryBatch,
		pollSummaryBatch:   pollSummaryBatch,
	})
	wireSegmentDeletion(controlHooks, warningRegistry, refusedCuts, deletion)
	wireVoiceSegmentDeletion(controlHooks, &voiceHooks)

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
		capture,
		hub.BroadcastStatusChanged,
	)
	wirePauseControls(controlHooks, pauseControls, capture, hardware, warningRegistry)
	if restoredPause {
		hub.BroadcastStatusChanged(true)
	}
	// micDevice orders the Jabra's mute light against every open, close and
	// probe of the capture device. A resume waits, before it confirms red,
	// until the light can show it (prepareLightForOpen).
	micDevice := newCaptureDevice(capture.inClosedState)
	capture.beforeMicOpen = micDevice.prepareLightForOpen
	capture.onMicHeld = micHeldWarning(warningRegistry)
	capture.capturePresent = captureDevicePresent(warningRegistry, func() bool { return audio.CaptureDevicePresent(cfg.MicDevice) })
	// A named microphone opens through ALSA's plug layer, so it opens at the
	// first rate the start tries, 16000 Hz, whatever rate its card captures
	// at; the recorder and Deepgram take the rate it opened at. Set before any
	// PortAudio initialization, all of which run in a microphone start, below.
	if err := audio.UsePlugCapture(cfg.MicDevice); err != nil {
		slog.Warn("open the microphone through ALSA's plug layer", "error", err)
	}
	// The buttons and the paused listener save the pause state: each runs as
	// work the stop waits for, and ends with the app's context.
	startPowerButton(ctx, &cfg, capture, controlHooks, warningRegistry, manager)
	startJabraMuteButton(ctx, &cfg, capture, micDevice, controlHooks, warningRegistry, func() { voiceHooks.Stop("Jabra button") }, manager)
	startPausedListener(ctx, cfgStore, &pausedListenerUnit{
		capture: capture, controls: pauseControls, hooks: controlHooks, device: micDevice,
		wake: hardware.Wake, warnings: warningRegistry, restored: restoredState, built: local.Built,
		background: manager,
	})

	envoyEvents.configure(ctx, &cfg, appLogger)
	envoyEvents.publishRecoveredEnds(recoveredIDs)
	if indexer != nil {
		startEmbeddingBackfill(ctx, manager, indexer)
	}

	// Register config change callback.
	onConfigChange(cfgStore, func(newCfg *config.Config) {
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
			slog.Warn("config: batch transcriber disabled", "error", err)
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

	// Summarize recovered sessions (and any with pending/empty summaries), as
	// work the stop cuts off and waits for.
	if startupSummarizer != nil {
		startSummaryBackfill(ctx, manager, summaryBackfill{
			store:      store,
			summarizer: startupSummarizer,
			publish:    publishSummaryReady,
			submit:     submitSummaryBatch,
			poll:       pollSummaryBatch,
			pollEvery:  30 * time.Second,
		})
	}
	// The shutdown closes the store within its deadline (closeStore); a
	// return before it closes it here.
	storeClosing := false
	defer func() {
		if !storeClosing {
			_ = store.Close()
		}
	}()

	var deletionDrive deletionRecordingSource
	if cfg.GDriveFolderID != "" && cfg.GDriveSyncEnabled {
		syncer, syncErr := newDriveSyncer(ctx, &cfg, appLogger)
		if syncErr != nil {
			slog.Warn("gdrive sync disabled", "error", syncErr)
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
	go sweepDriveSync(ctx, manager, store, drive, warningRegistry, 5*time.Minute, kept)

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
				slog.Warn("close resilient deepgram client failed", "error", err)
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
	// unit that starts paused scans at its first resume. The speaker plays
	// through ALSA, without PortAudio, so it needs no scan.
	var portAudioReady atomic.Bool
	scanPortAudio := func(updateStartupState bool, scan func() error) error {
		err := micDevice.probe(scan)
		portAudioReady.Store(err == nil)
		if err != nil {
			slog.Warn("portaudio init failed — microphone unavailable", "error", err)
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
		// So does shutdown, whose wait for the mic loops a long scan can
		// outlast: nothing would close a microphone opened after it.
		for _, rate := range cfg.SampleRateCandidates() {
			if err := micStartCancelled(ctx, capture); err != nil {
				return err
			}
			var candidate *audio.Mic
			openErr := micDevice.openStream(func() (bool, error) {
				var err error
				candidate, err = audio.NewMic(cfg.MicDevice, rate, rate/4) // 250ms buffer
				return err == nil, err
			})
			if openErr != nil {
				slog.Warn("microphone open failed", "rate", rate, "error", openErr)
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

		if err := audioRecorder.SetSampleRate(sampleRate); err != nil {
			// The session's recording still saves at this rate when it ends;
			// only a raw kept after a stop would be saved at the old one.
			slog.Warn("the session under way keeps its old sample rate in the database", "error", err)
		}
		if voiceTap != nil {
			voiceTap.SetRate(sampleRate)
		}
		// micRef is published before the start so the controller, woken when
		// the start clears the forced-red hold, already sees the open mic.
		micRef.Store(mic)
		if err := startOpenedMic(capture, ordered, func() { micRef.Store(nil) }); errors.Is(err, errMicStartPaused) {
			// A pause came during the light's check: as during the scan, the
			// next resume starts again (startMicrophone).
			return err
		} else if err != nil {
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
			startMicMonitoring(ctx, &micLoops, capture, ordered, audioRecorder, nil, voiceAudio, micWait, log.Printf, hub)
			return nil
		}

		cOptions := &interfaces.ClientOptions{EnableKeepAlive: true}
		tOptions := liveTranscriptionOptions(&cfg, sampleRate, dispatchProjects.Get())
		resilientConfig := liveTranscriptionConfig(&cfg, sampleRate)
		callback := transcriptCallback{manager: manager, voice: &voiceHooks, stamp: capture.voiceStamp, keyterms: liveKeyterms}
		candidateResilient := newLiveTranscription(ctx, cfg.DeepgramAPIKey, cOptions, tOptions, resilientConfig, callback, warningRegistry, hub)
		resilientRef.Store(candidateResilient)
		deepgramSendRef.Store(startMicMonitoring(ctx, &micLoops, capture, ordered, audioRecorder, candidateResilient, voiceAudio, micWait, log.Printf, hub))
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
		if !portAudioReady.Load() {
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
	// Initialize the speaker's output. It plays through ALSA's playback device,
	// opening no microphone and needing no red light, so it speaks while
	// capture is paused. Its stream opens through micDevice and closes
	// OutputIdleClose after the last sound, so the Jabra's light never changes
	// while one is open.
	var output *audio.Output
	if cfg.SpeakerEnabled {
		outputCfg := audio.OutputConfig{
			Device:  cfg.SpeakerDevice,
			Opening: micDevice.playbackOpening,
			Closing: micDevice.playbackClosing,
		}
		// speaker_trace_dir, a debug setting: keep the last seconds of the
		// execution trace and save them on each underrun.
		if dir := strings.TrimSpace(cfg.SpeakerTraceDir); dir != "" {
			if speakerTrace, err := audio.StartSpeakerTrace(dir); err != nil {
				slog.Warn("speaker trace off", "error", err)
			} else {
				defer speakerTrace.Stop()
				outputCfg.Underrun = speakerTrace.Underrun
				log.Printf("speaker trace on: each underrun saves the last seconds of the execution trace in %s", dir)
			}
		}
		output = audio.NewOutput(outputCfg)
		log.Printf("speaker output enabled (device=%q)", cfg.SpeakerDevice)
	}

	// Initialize TTS orchestrator if both speaker and provider are configured.
	var ttsOrchestrator *tts.Orchestrator
	if output != nil && cfg.TTSProvider != "" {
		ttsProvider, ttsErr := newTTSProvider(ctx, &cfg)
		if ttsErr != nil {
			slog.Warn("TTS disabled", "error", ttsErr)
			warningRegistry.Set("tts", fmt.Sprintf("TTS provider initialization failed: %v", ttsErr))
		} else {
			// POST /api/tts/speak waits in its queue while a voice exchange
			// holds the speaker.
			ttsOrchestrator = tts.NewOrchestrator(ttsProvider, output, tts.OrchestratorOpts{Floor: voiceHooks.WithFloor})
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
	}

	// The meeting voice: the Supervisor over the speaker output, whenever the
	// unit has a speaker, since a person turns the voice on and picks it on the
	// settings page with no restart (R13). The voice for each call is built from
	// the settings saved last; the Concierge runs only while the voice is on.
	var conc *lazyConcierge
	if output != nil {
		var voices voiceSource
		voices.apply(ctx, &cfg)
		voices.warnBuild(cfg.Voice.Enabled, warningRegistry)
		onConfigChange(cfgStore, func(newCfg *config.Config) {
			voices.apply(ctx, newCfg)
			voices.warnBuild(newCfg.Voice.Enabled, warningRegistry)
		})
		// Agents a person tells, through Dispatch, to join the meeting (R17).
		var issues *dispatch.Client
		if cfg.DispatchToken != "" && cfg.Concierge.DispatchURL != "" {
			if client, err := dispatch.NewClient(cfg.Concierge.DispatchURL, cfg.DispatchToken, newDispatchHTTP(appLogger)); err != nil {
				slog.Warn("joined agents: the Dispatch client for issue titles", "error", err)
			} else {
				issues = client
			}
		}
		roster := startJoinedAgents(ctx, &joinedDeps{
			session:   func() string { id, _ := manager.ActiveSession(); return id },
			meeting:   manager.MeetingContext,
			store:     store,
			floor:     &voiceHooks,
			dispatch:  issues,
			notify:    hub.BroadcastVoiceEvent,
			keyterms:  liveKeyterms,
			publicURL: cfg.PublicURL,
		})
		joinedLink.set(roster)
		wireJoinedRoutes(controlHooks, roster)
		conc = newLazyConcierge(func() (*concierge.Concierge, func(context.Context), error) {
			current := cfgStore.Get()
			// The session page reads a session's questions again on each change.
			return startConcierge(ctx, &current, store, appLogger, func(sessionID string) {
				hub.BroadcastVoiceEvent("voice_tasks_changed", sessionID)
			}, roster)
		}, func(c *concierge.Concierge) {
			if c == nil { // it stopped: the session hooks reach nothing
				conciergeSessionHooks.set(nil)
				return
			}
			conciergeSessionHooks.set(c)
		}, appLogger)
		conc.followEnabled(cfg.Voice.Enabled) // a failed start is logged; each call then answers unreachable
		onConfigChange(cfgStore, followVoiceEnabled(&voiceHooks, conc))
		if err := startVoice(&voiceDeps{
			output:      output,
			tap:         voiceTap,
			concierge:   conc,
			link:        &voiceHooks,
			voices:      voices.Voice,
			enabled:     func() bool { return cfgStore.Get().Voice.Enabled },
			wakePhrases: cfg.Voice.WakePhrases,
			timing:      voiceTiming(appLogger),
			session:     func() string { id, _ := manager.ActiveSession(); return id },
			meeting:     manager.MeetingContext,
			red:         func() bool { return capture.capturing(micStatus) },
			notify:      hub.BroadcastVoiceEvent,
			projects:    dispatchProjects.Get,
			segment:     joinedLink.OnSegment,
			handDone:    joinedLink.HandDone,
			waiting:     waitingLED{hardware},
		}); err != nil {
			slog.Warn("the meeting voice is off", "error", err)
			warningRegistry.Set("voice", "The meeting voice is off: "+err.Error())
		}
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
	go func() {
		_ = manager.Run(ctx, func(ctx context.Context) error {
			kept.run(ctx)
			return nil
		})
	}()
	// The refinements a stop cut off (recoverAtStart above) are retried, after
	// the cleanup above, since a retry may restore recordings.
	recovery.retry(ctx, warningRegistry, deletion.retryRefinement)
	if cfg.GCEnabled {
		go collectGarbage(ctx, manager, cfgStore, deletionNoticeGCStore{SQLiteStore: store, hub: hub, onDeleted: sessionsForgotten}, drive, gcHealthState, warningRegistry, appLogger, gcEvery)
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
	slog.Info("deepgram API key", "status", config.APIKeyStatus(cfg.DeepgramAPIKey))
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
	steps := shutdownSteps{
		closeVoice:         voiceHooks.Close,
		stopCapture:        capture.Stop,
		stopApp:            stopApp,
		stopStartPublishes: manager.StopSessionStartPublishes,
		// Each mic loop closes its microphone and ends, even if its device
		// delivers no frame. Stopping the device or terminating PortAudio
		// while a Stream still runs crashes inside PortAudio, so neither
		// happens before they end, nor at all if one is stuck: the process
		// exit releases the device then.
		joinMicLoops: func() bool {
			ended := joinMicLoops(&micLoops, micShutdownWait)
			if !ended {
				slog.Warn("microphone still reading after shutdown began; leaving the device and PortAudio to the process exit", "waited", micShutdownWait)
			}
			return ended
		},
		// With the microphones closed, the light goes off. Stop refuses any
		// further red confirmation, and lets the off write through even if a
		// microphone is stuck open, since the process is exiting.
		lightOff: func() {
			hardware.Stop()
			stopLight()
			select {
			case <-lightOff:
			case <-time.After(lightShutdownWait):
				slog.Warn("recording light not turned off in time; leaving it to its watchdog", "waited", lightShutdownWait)
			}
		},
		// No request runs once the stop waits for the store's work: a request
		// after it (a manual start or end, a merge, a delete) would start
		// work the stop has already refused or no longer waits for.
		stopServing:   func(ctx context.Context) { stopServing(ctx, httpServer, httpShutdownWait) },
		stopStoreWork: func(ctx context.Context) { recovery.stop(ctx, manager, lifecyclePublishWait) },
		stopDevices: func(ctx context.Context, micLoopsEnded bool) {
			if dgStop != nil {
				dgStop()
			}
			if mic := micRef.Load(); mic != nil && micLoopsEnded {
				_ = mic.Stop()
			}
			if conc != nil {
				conc.Stop(ctx)
			}
			if ttsOrchestrator != nil {
				ttsOrchestrator.Stop()
			}
			if output != nil {
				if err := output.Close(); err != nil {
					slog.Warn("speaker output close failed", "error", err)
				}
			}
			if micLoopsEnded {
				_ = audio.TerminatePortAudio() // best-effort cleanup
			}
		},
		closeStore: func(ctx context.Context) {
			storeClosing = true
			closeStore(ctx, store)
		},
	}
	steps.run(shutdownWait)
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
	// closeAtShutdown closes the microphone unless it is closed already.
	// Shutdown turns the light off once every mic loop has ended, so no loop
	// may end with its microphone open: not one a start that shutdown
	// overtook handed an open microphone, nor one that has just opened it.
	closeAtShutdown := func() {
		if !released {
			closeMic(capture, streamer, "at shutdown")
		}
	}

	for {
		if ctx.Err() != nil {
			closeAtShutdown()
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
				closeMic(capture, streamer, why)
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
			if err = capture.confirmMicOpen(); errors.Is(err, errMicStartPaused) {
				// A pause came during the light's check: nothing is lit or
				// opened, and the loop top waits for the next resume. The
				// resuming state is left to that resume's own open: a resume
				// that came after the pause is still starting.
				released = true
				continue
			} else if err != nil {
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
			if err == nil && capture.closedSinceMicOpen() {
				// A pause, or a hold for the light, came during the red
				// confirmation or the open, and a resume may have opened the
				// gate again since: the device may have captured through the
				// pause. Close it, which drops what it holds, and open it
				// again once capture is back.
				reopen, reopenWhy = true, "after a pause during its open"
				continue
			}
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
				closeAtShutdown()
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
			if errors.Is(err, errClosedSinceMicOpen) {
				logf("the capture gate closed while the microphone was open; reopening the microphone")
				reopen, reopenWhy = true, "after a pause while it was open"
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
			closeMic(capture, streamer, why)
		}
		released = true // the loop top, after a pause, closes nothing more
		// A pause since the microphone opened, though a resume may have
		// opened the gate again since, leaves the open to the loop top: under
		// that resume's own red confirmation, which ends its resuming state.
		if isOverflow(err) {
			slog.Warn("mic input overflow, restarting stream")
			if hub != nil {
				hub.BroadcastComponentStatus("mic", "warning", "Mic input overflow, restarting stream")
			}
			wait(overflowWait)
			if ctx.Err() != nil {
				return
			}
			if capture.isClosed() || capture.closedSinceMicOpen() {
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
		if capture.isClosed() || capture.closedSinceMicOpen() {
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
			slog.Warn("the microphone is still reading; it stays open, with the light red, until its read returns")
			capture.micHeld(true)
			closed.held = true
		case err != nil:
			slog.Warn("close microphone failed", "why", why, "error", err)
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
// be confirmed red, or the gate is closed or has closed since the microphone
// last opened, also during the status store or the light's check
// (confirmMicReopen, errMicStartPaused): the loop top then opens it, under the
// resume's own confirmation. Otherwise it returns true and the reopen's
// error, which is errNoCaptureDevice, with the device closed and the light
// left off, while no capture device is present.
func reopenMic(capture *captureGate, streamer micStreamer, logf func(string, ...any), hub *server.Hub) (bool, error) {
	if hub != nil {
		hub.BroadcastComponentStatus("mic", storage.ComponentStatusReconnecting, "Attempting to reconnect microphone...")
	}
	if err := capture.confirmMicReopen(); errors.Is(err, errMicStartPaused) {
		// A pause came before or during the light's check: the loop top
		// waits for the next resume, or takes the one that came, and opens
		// the device under its own confirmation.
		closeMic(capture, streamer, "for capture pause")
		return false, nil
	} else if errors.Is(err, errNoCaptureDevice) {
		logf("no capture device present; the microphone reconnects once one is")
		closeMic(capture, streamer, "with no capture device present")
		if hub != nil {
			hub.BroadcastComponentStatus("mic", storage.ComponentStatusError, "No microphone found")
		}
		return true, err
	} else if err != nil {
		logf("recording light unavailable before microphone reconnect: %v", err)
		capture.lightUnconfirmed(err)
		closeMic(capture, streamer, "without a red recording light")
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
func closeMic(capture *captureGate, streamer micStreamer, which string) {
	if err := streamer.Close(); err != nil {
		slog.Warn("close microphone failed", "why", which, "error", err)
	}
	capture.micClosed()
}

// holdMicWhilePaused closes the capture device, which releases it, and blocks
// until capture resumes (waitWhilePaused). It reports false if ctx ends first.
func holdMicWhilePaused(ctx context.Context, capture *captureGate, streamer micStreamer, logf func(string, ...any), hub *server.Hub) bool {
	closeMic(capture, streamer, "for capture pause")
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
