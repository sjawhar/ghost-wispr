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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/indicator"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/monoclock"
	"github.com/sjawhar/ghost-wispr/internal/providererr"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
	"github.com/sjawhar/ghost-wispr/internal/tts"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	voicetts "github.com/sjawhar/ghost-wispr/internal/voice/tts"
	"github.com/sjawhar/ghost-wispr/internal/wake/local"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
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
	// heardAt times a word for the voice: when the audio at a point of the
	// live stream was captured (transcribe.ResilientClient.CapturedAt). nil,
	// or a point it cannot time, leaves the word untimed. A socket's own
	// stream times its words in its place, once it has one (stream).
	heardAt func(at time.Duration) (time.Time, bool)
	// stream is the stream of the socket whose messages this copy hands on,
	// set once the resilient client gave it one (forStream); nil before.
	stream *transcribe.Stream
}

// forStream is c for the messages of the socket with stream
// (streamedCallback).
func (c transcriptCallback) forStream(stream *transcribe.Stream) api.LiveMessageCallback {
	c.stream = stream
	return c
}

// admit reports whether a message of c's socket may be handed on, after any
// wait for a retired socket's last results (transcribe.Stream.Admit). A
// retired socket's message past its wait is dropped, with the session it
// would have reached and its time into that socket's stream in the log:
// handed on, it would reach the session after the next socket's.
func (c transcriptCallback) admit(kind string, streamSeconds float64) bool {
	if c.stream == nil || c.stream.Admit() {
		return true
	}
	var sessionID string
	if active, ok := c.manager.(interface{ ActiveSession() (string, time.Time) }); ok {
		sessionID, _ = active.ActiveSession()
	}
	slog.Warn("deepgram: a replaced socket's message came after its wait for its last results; dropped", "kind", kind, "session_id", sessionID, "stream_seconds", streamSeconds)
	return false
}

// voiceListener is the voice as the transcript reader feeds it: the
// voice.Supervisor.
type voiceListener interface {
	OnTranscript(t *voice.Transcript)
	OnUtteranceEnd(closings int, gateOpen bool, lastWordEnd time.Time, at monoclock.Stamp)
}

// keptMessager is a session manager that takes each result with the stream
// of the socket that heard it and reports what of it its transcript keeps
// (session.Manager.MessageKeptOn): words from before the latest pause are
// dropped, and the voice reads exactly what is left.
type keptMessager interface {
	MessageKeptOn(stream session.LiveStream, mr *api.MessageResponse) (*api.MessageResponse, error)
}

// The build fails if the session manager stops reporting what its transcript
// keeps: the voice would then read every live result, a late wake from before
// a pause included (contract 4, R4).
var _ keptMessager = (*session.Manager)(nil)

// Message hands a Deepgram result to the session manager, with the stream of
// the socket that heard it, then what the manager's transcript kept of it to
// the voice, with the capture gate's stamp and the time this callback began
// handing it on, before the manager's work: the SDK runs the callbacks in
// turn off the socket's one receive loop, so this is callback time, not
// Deepgram's transport time. On the first words of a recording session the
// manager's work starts the session.
func (c transcriptCallback) Message(mr *api.MessageResponse) error {
	if !c.admit("result", mr.Start) {
		return nil
	}
	entered := monoclock.Now()
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
		var stream session.LiveStream // nil: the live stream's current socket
		if c.stream != nil {
			stream = c.stream
		}
		kept, err = manager.MessageKeptOn(stream, mr)
	default:
		err = manager.Message(mr)
	}
	if c.voice != nil && kept != nil {
		heardAt := c.heardAt
		if c.stream != nil {
			heardAt = c.stream.CapturedAt
		}
		if t := voiceTranscript(kept, entered, func() (int, bool) { return closings, open }, heardAt); t != nil {
			c.voice.OnTranscript(t)
		}
	}
	return err
}

// voiceTranscript is mr, which arrived at at (on both clocks), as the voice
// reads it, or nil when it holds no text. heardAt, when set, times each word
// by when its start was captured, which the voice's barge-in reads: a result
// can come seconds after its words.
func voiceTranscript(mr *api.MessageResponse, at monoclock.Stamp, stamp func() (int, bool), heardAt func(time.Duration) (time.Time, bool)) *voice.Transcript {
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
		At:           at.Wall,
		Mono:         at.Mono,
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
		word := voice.Word{Text: text, Speaker: speaker, Start: w.Start, End: w.End}
		if heardAt != nil {
			// Rounded, not truncated: 1.001 s is 1.000999999… in binary, and
			// truncated would place the word a sample early.
			if at, ok := heardAt(time.Duration(math.Round(w.Start * float64(time.Second)))); ok {
				word.At = at
			}
		}
		t.Words = append(t.Words, word)
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

// UtteranceEnd hands Deepgram's UtteranceEnd to the session manager, then to
// the voice with the gate's stamp, when its last word ended by capture (its
// last_word_end, timed as a result's words are; zero when that cannot be
// timed), and the time this callback began handling it, before the
// manager's work: callback-entry time, not Deepgram's transport time
// (Message).
func (c transcriptCallback) UtteranceEnd(ur *api.UtteranceEndResponse) error {
	if !c.admit("utterance_end", ur.LastWordEnd) {
		return nil
	}
	entered := monoclock.Now()
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
		heardAt := c.heardAt
		if c.stream != nil {
			heardAt = c.stream.CapturedAt
		}
		var lastWordEnd time.Time
		if heardAt != nil {
			if at, ok := heardAt(time.Duration(math.Round(ur.LastWordEnd * float64(time.Second)))); ok {
				lastWordEnd = at
			}
		}
		c.voice.OnUtteranceEnd(closings, open, lastWordEnd, entered)
	}
	return err
}

// Close logs the socket's end and has the session save the words it holds.
// A retired socket that ends after its wait for its last results changes
// nothing: the next socket's results already reach the session.
func (c transcriptCallback) Close(*api.CloseResponse) error {
	log.Println("disconnected from Deepgram")
	if c.stream != nil && !c.stream.Admit() {
		return nil
	}
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
	if sender != nil {
		writer = readTimedWriter{dst: writer, sender: sender}
	}
	streamMicLoopStarter(ctx, loops, capture, streamer, writer, wait, logf, hub)
	return sender
}

// readMarker is a writer behind the capture gate that takes the time the
// microphone read each buffer: the gate marks it as the buffer comes, before
// it reads the light back, which can hold the buffer for a while.
type readMarker interface {
	markRead(at time.Time)
}

// readTimedWriter writes each buffer to dst, and gives the live
// transcription's send queue the time the microphone read it, by which the
// voice times each word (transcribe.ResilientClient.CapturedAt).
type readTimedWriter struct {
	dst    io.Writer
	sender *transcribe.AsyncSender
}

func (w readTimedWriter) markRead(at time.Time) { w.sender.MarkRead(at) }

func (w readTimedWriter) Write(p []byte) (int, error) { return w.dst.Write(p) }

func (w readTimedWriter) WriteWithGateStamp(p []byte, gate *captureGate, closings int) (int, error) {
	if stamped, ok := w.dst.(gateStampedWriter); ok {
		return stamped.WriteWithGateStamp(p, gate, closings)
	}
	return w.dst.Write(p)
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
// NoDelay is on only while the meeting voice is enabled, where a fast turn
// end matters: with smart formatting Deepgram holds a final whose last words
// look like an unfinished number until 3 s of silence, and a question to the
// voice often ends in an issue key ("INFRA ten twenty-seven"). It trades some
// of Deepgram's number and entity formatting for that speed (Deepgram's
// "Streaming finalization behavior"); the voice reads a spoken key either way.
// A unit recording with the voice off keeps Deepgram's full formatting, as
// nothing waits for the turn's end. A change of voice.enabled marks the
// stream's options changed (ResilientClient.OptionsChanged), so the socket
// follows the setting.
func liveTranscriptionOptions(cfg *config.Config, sampleRate int, projects []string) *interfaces.LiveTranscriptionOptions {
	return &interfaces.LiveTranscriptionOptions{
		Model:          liveTranscriptionModel(cfg),
		Language:       "en-US",
		Diarize:        true,
		Punctuate:      true,
		SmartFormat:    true,
		NoDelay:        cfg.Voice.Enabled,
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

// makeDeepgramClientFactory dials live-transcription sockets the app owns
// (deepgramSocket), each write bounded by writeTimeout, each dial signed in
// with the key key gives then. optsFor builds the request options fresh for
// each dial, so a reconnect (after a disconnect or a forced one) carries the
// settings as they stand, such as no_delay following voice.enabled. Each dial
// carries the live keyterms as they are then, and is the socket a join's
// Configure goes to until the next dial.
func makeDeepgramClientFactory(_ context.Context, key liveKey, cOptions *interfaces.ClientOptions, optsFor func() *interfaces.LiveTranscriptionOptions, callback transcriptCallback, writeTimeout time.Duration) transcribe.ClientFactory {
	return func(factoryCtx context.Context) (io.WriteCloser, error) {
		apiKey, err := key(factoryCtx)
		if err != nil {
			return nil, fmt.Errorf("deepgram key: %w", err)
		}
		options := optsFor()
		var keyterms []string
		if callback.keyterms != nil {
			keyterms = callback.keyterms.List()
			withKeyterms := *options
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

// newSpeechProvider builds POST /api/tts/speak's provider: the tts role's
// model, voice and length. opts go to its engines: ghost-wispr
// tts-first-audio's tests are told the Aura sockets it dials.
func newSpeechProvider(cfg *config.Config, opts ...voicetts.Option) (*tts.EngineProvider, error) {
	p, model, err := unitProviders.of(cfg).Model(cfg, config.RoleTTS, cfg.TTS.Model)
	if err != nil {
		return nil, err
	}
	return tts.NewEngineProvider(p, model, cfg.TTS.Voice, cfg.TTS.MaxLength, opts...)
}

// speechSource is POST /api/tts/speak's provider of the settings saved last:
// a settings change of the tts role or its provider reaches the next request,
// as it reaches voice A's next call.
type speechSource struct {
	store interface{ Get() config.Config }
	// warnings, when set, holds the tts warning of /api/status: a provider's
	// refusal of a speech, or a speech that returned no audio, sets it, and
	// the next speech that works clears it.
	warnings *warningsRegistry

	mu    sync.Mutex
	key   string
	model string // the tts.model built speaks
	built *tts.EngineProvider
	err   error
}

// ttsWarningKey is the warning of text-to-speech: it cannot start, or a
// provider refused the last speech or returned no audio for it.
const ttsWarningKey = "tts"

func (s *speechSource) provider() (*tts.EngineProvider, string, error) {
	cfg := s.store.Get()
	key := strings.Join([]string{cfg.TTS.Model, cfg.TTS.Voice, strconv.Itoa(cfg.TTS.MaxLength), providersKey(&cfg)}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != key || (s.built == nil && s.err == nil) {
		s.key, s.model = key, cfg.TTS.Model
		s.built, s.err = newSpeechProvider(&cfg)
	}
	return s.built, s.model, s.err
}

// Synthesize speaks with the tts role saved last.
func (s *speechSource) Synthesize(ctx context.Context, req tts.SpeechRequest) (*tts.SpeechResponse, error) {
	p, model, err := s.provider()
	if err != nil {
		return nil, err
	}
	resp, err := p.Synthesize(ctx, req)
	s.warn(model, err)
	return resp, err
}

// ttsModelCheck is what a tts warning tells the operator to check when the
// provider took the sign-in but not the speech.
const ttsModelCheck = "check that tts.model names a speech model and tts.voice one of its voices"

// warn sets the tts warning when the provider refused a speech of model, a
// refusal no retry mends, naming its status and code and none of its words,
// or returned no audio for it (voicetts.ErrNoAudio), which POST
// /api/tts/speak answers 502; a speech that worked clears it. A speech that
// failed another way leaves it.
func (s *speechSource) warn(model string, err error) {
	if s.warnings == nil {
		return
	}
	switch refusal, refused := providerRefusal(err); {
	case err == nil:
		s.warnings.Clear(ttsWarningKey)
	case refused:
		s.warnings.Set(ttsWarningKey, fmt.Sprintf("Text-to-speech with %s failed: %s", model, refusalWarning(refusal, ttsModelCheck)))
	case errors.Is(err, voicetts.ErrNoAudio):
		provider, _, _ := config.ParseModelRef(model)
		s.warnings.Set(ttsWarningKey, fmt.Sprintf("Text-to-speech with %s failed: %s returned no audio; %s", model, provider, ttsModelCheck))
	}
}

// Name names the tts role's provider.
func (s *speechSource) Name() string {
	if p, _, err := s.provider(); err == nil {
		return p.Name()
	}
	return ""
}

// Configured reports whether the settings saved last turn text-to-speech on
// (tts.model is set), as tts.Configurable asks: with it off, POST
// /api/tts/speak answers 501 and queues nothing.
func (s *speechSource) Configured() bool {
	return strings.TrimSpace(s.store.Get().TTS.Model) != ""
}

func main() {
	// Before any Google client is built: its own logger writes around the
	// app's (logging.WithholdGoogleSDKLogging).
	googleSDKLogging := logging.WithholdGoogleSDKLogging()
	configPath := config.PathFromEnvironment()
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
	if len(os.Args) > 1 && os.Args[1] == "pi-secrets" {
		os.Exit(runPiSecretsCommand(context.Background(), os.Args[1:], os.Stdout, configPath))
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
	// Every provider signs in with the keys saved last, whatever config a
	// caller of the providers read before (providerSets.follow).
	unitProviders.follow(cfgStore)
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
	// each ends when its raw recording was last written, with its
	// session_ended event queued in the same write.
	recoveredIDs := recoverStaleSessions(store, &cfg)
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
	// So did a summary: it is pending again, and the backlog below takes it.
	// After this, a summary running is one this run makes now, which no
	// backfill takes again. That includes one a batch job of the last run
	// claimed: this run polls only the job it submits itself, so left to the
	// old job the session would stay running with nothing to end it. The
	// backlog's summary is a new summary attempt, and the old job's result,
	// if it is polled later, is dropped (storage's UpdateSummaryAtClaim).
	if ids, err := store.RecoverInterruptedSummaries(); err != nil {
		slog.Warn("failed to recover interrupted summaries", "error", err)
	} else if len(ids) > 0 {
		log.Printf("recovered %d interrupted summary(ies): %v", len(ids), ids)
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

	clientFactory := summaryClients(cfgStore)
	llmTracker := server.NewLLMHealthTracker()
	llmHealthOnResult := func(err error) {
		if err != nil {
			llmTracker.SetError(err.Error())
			return
		}
		llmTracker.SetOK()
	}
	summaries := newSummarizers(clientFactory, llmHealthOnResult)
	sessionSummarizer := summaries.apply(&cfg)

	// The status page's warnings; the search indexer below already sets one.
	warningRegistry := newWarningsRegistry()
	embeddingClient, embeddingModel, err := newEmbeddingClient(&cfg)
	if err != nil {
		slog.Warn("embedding indexer disabled", "error", err)
	}

	manager := session.NewManager(store, audioRecorder, sessionSummarizer, hub, detector, cfg.MinSessionSegments, appLogger)
	// The meeting voice and its Concierge, once they run, drop what an ended
	// session still waits on and forget a deleted session. The voice link is
	// wired to the capture gate below, before anything can close it.
	var conciergeSessionHooks conciergeHooks
	var voiceHooks voiceLink
	var joinedLink joinedHooks // the joined agents' roster, once the voice has a speaker
	sessionsForgotten := wireVoiceSessionHooks(manager, &conciergeSessionHooks, &voiceHooks, &joinedLink)

	// exitCode is the recorder's exit status, set when it stops on a fault
	// (its web server failed), so systemd starts it again. Registered before
	// every other deferred cleanup, it exits after all of them have run; no
	// log.Fatal follows it.
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()
	// ctx ends at shutdown; the capture gate's pause and the mic loops stop
	// with it, and work it cuts off reads as stopped (newAppContext).
	ctx, stopApp := newAppContext()
	defer stopApp()
	// The search indexer's backfills run as work the stop cuts off and waits
	// for (the session manager's).
	var indexer *searchIndexer
	if embeddingClient != nil {
		indexer = newSearchIndexer(ctx, manager, embeddingClient, embeddingModel, cfg.Embedding.Model, store, warningRegistry)
		manager.SetIndexer(indexer)
	}

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
	submitSummaryBatch := newSummaryBatchSubmitter(store, hub, summaries, cfgStore)
	pollSummaryBatch := newSummaryBatchPoller(store, hub, summaries, cfgStore, publishSummaryReady)

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
	controlHooks := &server.ControlHooks{
		ProviderStatuses: providerStatuses(cfgStore),
		IsPaused:         capture.IsPaused,
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
		GetLogs:    logSink.GetLogs,
		Presets:    summaries.presets,
		TestPreset: presetTester(cfgStore, store, clientFactory),
		GeneratePreset: func(ctx context.Context, description string) (config.Preset, error) {
			latestCfg := cfgStore.Get()
			modelStr := latestCfg.Summarization.Model
			provider, model, err := config.ParseModelRef(modelStr)
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
			provider, model, err := config.ParseModelRef(modelStr)
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
		summarizer:                    summaries.summarizer,
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
		cfgStore:           cfgStore,
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
		indexer.requestBackfill()
	}

	// Register config change callback.
	onConfigChange(cfgStore, func(newCfg *config.Config) {
		// The providers first, so a key saved and checked at once is checked
		// with the new Set (GET /api/providers reads the Set of the config
		// saved last, which this builds).
		unitProviders.of(newCfg)
		detector.SetTimeout(newCfg.ParsedSilenceTimeout())
		manager.SetMinSessionSegments(newCfg.MinSessionSegments)
		log.Printf("config: reloaded (silence_timeout=%s)", newCfg.SilenceTimeout)

		if batchTranscriber, err := makeBatchTranscriber(newCfg); err != nil {
			manager.SetBatchTranscriber(nil)
			slog.Warn("config: batch transcriber disabled", "error", err)
		} else {
			manager.SetBatchTranscriber(batchTranscriber)
			log.Printf("config: batch transcriber updated for %s", newCfg.BatchTranscription.Model)
		}

		envoyEvents.configure(ctx, newCfg, appLogger)
	})
	summaries.follow(cfgStore, manager)
	if indexer != nil {
		indexer.followSignIn(cfgStore)
	}
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
	// work the stop cuts off and waits for, each with the summarizer of the
	// settings saved last.
	if summaries.get() != nil {
		startSummaryBackfill(ctx, manager, summaryBackfill{
			store:     store,
			batchJobs: cfg.Summarization.BatchJobs,
			submit:    submitSummaryBatch,
			poll:      pollSummaryBatch,
			online:    newOnlineSummaries(store, summaries.summarizer, hub, publishSummaryReady),
			pollEvery: 30 * time.Second,
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
	if cfg.LiveTranscriptionKey() != "" {
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

		if cfg.LiveTranscriptionKey() == "" {
			startMicMonitoring(ctx, &micLoops, capture, ordered, audioRecorder, nil, voiceAudio, micWait, log.Printf, hub)
			return nil
		}

		cOptions := liveTranscriptionClientOptions(&cfg)
		// Fresh options per dial, so a redial follows the live settings: a
		// change of voice.enabled changes no_delay (OptionsChanged, below).
		optsFor := func() *interfaces.LiveTranscriptionOptions {
			current := cfgStore.Get()
			return liveTranscriptionOptions(&current, sampleRate, dispatchProjects.Get())
		}
		resilientConfig := liveTranscriptionConfig(&cfg, sampleRate)
		callback := transcriptCallback{manager: manager, voice: &voiceHooks, stamp: capture.voiceStamp, keyterms: liveKeyterms}
		candidateResilient := newLiveTranscription(ctx, liveTranscriptionCredential(cfgStore), cOptions, optsFor, resilientConfig, callback, warningRegistry, hub)
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
		if cfg.LiveTranscriptionKey() != "" {
			startupStatus.Deepgram = config.ComponentState{Name: "deepgram", Status: captureLightUnavailable, Message: "Deepgram connects once the recording light is confirmed red"}
		}
	case deferred:
		startupStatus.Mic = config.ComponentState{Name: "mic", Status: storage.ComponentStatusPaused, Message: "Microphone closed while capture is paused"}
		if cfg.LiveTranscriptionKey() != "" {
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

	// POST /api/tts/speak speaks with the tts role whenever there is a
	// speaker, so a settings change can turn it on with no restart; a role it
	// cannot build fails each request, and the status page says why.
	var ttsOrchestrator *tts.Orchestrator
	if output != nil {
		speech := &speechSource{store: cfgStore, warnings: warningRegistry}
		if _, _, err := speech.provider(); err != nil && strings.TrimSpace(cfg.TTS.Model) != "" {
			slog.Warn("text-to-speech unavailable", "error", err)
			warningRegistry.Set(ttsWarningKey, fmt.Sprintf("Text-to-speech cannot start: %v", err))
		}
		// POST /api/tts/speak waits in its queue while a voice exchange
		// holds the speaker.
		ttsOrchestrator = tts.NewOrchestrator(speech, output, tts.OrchestratorOpts{Floor: voiceHooks.WithFloor})
		log.Printf("text-to-speech queue started (tts.model=%q)", cfg.TTS.Model)
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
		// The live transcription asks for no_delay only while the meeting voice
		// is enabled (liveTranscriptionOptions). A change of that setting marks
		// the stream's options changed; its one reconnect owner redials once
		// with them at the next safe point, ending the old socket gracefully,
		// and losing no audio. The factory builds the options fresh each dial.
		voiceEnabledDialled := &atomic.Bool{}
		voiceEnabledDialled.Store(cfg.Voice.Enabled)
		onConfigChange(cfgStore, func(newCfg *config.Config) {
			if voiceEnabledDialled.Swap(newCfg.Voice.Enabled) == newCfg.Voice.Enabled {
				return
			}
			if rc := resilientRef.Load(); rc != nil {
				rc.OptionsChanged()
			}
		})
		if err := startVoice(&voiceDeps{
			output:      output,
			tap:         voiceTap,
			concierge:   conc,
			link:        &voiceHooks,
			voices:      voices.Voice,
			enabled:     func() bool { return cfgStore.Get().Voice.Enabled },
			wakePhrases: cfg.Voice.WakePhrases,
			timing:      voiceTiming(appLogger),
			session:     manager.SettledSession,
			mark:        manager.SessionMark,
			ofStart:     manager.SessionOfStart,
			meeting:     manager.MeetingContext,
			red:         func() bool { return capture.capturing(micStatus) },
			notify:      hub.BroadcastVoiceEvent,
			projects:    dispatchProjects.Get,
			takeBack:    concierge.TakeBack,
			asks:        concierge.Asks,
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
	if cfg.LiveTranscriptionKey() != "" {
		deepgramHealth = &resilientClientProxy{ref: &resilientRef}
	}
	healthChecker := server.NewDefaultHealthChecker(deepgramHealth, store, micStatus, captureReadiness{capture: capture, mic: micStatus}, llmTracker)
	handler, err := server.HandlerWithLogger(assets, hub, store, controlHooks, authToken, healthChecker, appLogger, searchQueryClient(embeddingClient, cfg.Embedding.Model, warningRegistry), cfgStore)
	if err != nil {
		panic(fmt.Sprintf("build http handler failed: %v", err))
	}

	addr := listenAddress(os.Getenv("GHOST_WISPR_ADDR"))
	httpServer := &http.Server{Addr: addr, Handler: handler}
	// The stop signals are caught from before the server serves: a SIGTERM
	// while it starts then shuts the recorder down cleanly.
	sig := make(chan os.Signal, 1)
	served := serveHTTP(httpServer, sig, syscall.SIGINT, syscall.SIGTERM)

	go deletion.recoverAtStart(ctx, deletionDrive, warningRegistry)

	// Log structured startup banner.
	config.LogStartupBanner(appLogger, &startupStatus, Version, Commit, BuildTime)
	slog.Info("deepgram API key", "status", config.APIKeyStatus(cfg.LiveTranscriptionKey()))
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

	// awaitStop returns on a stop signal, or on the web server's failure.
	if err := awaitStop(sig, served); err != nil {
		slog.Error("the web server stopped; shutting down so systemd starts the recorder again", "error", err)
		exitCode = 1
	}

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
				// Its websocket's close can wait on Deepgram: within the deadline.
				closeWithin(ctx, "the Deepgram connection", dgStop)
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
