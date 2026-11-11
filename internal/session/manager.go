package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Manager struct {
	store              Store
	recorder           Recorder
	summarizer         Summarizer
	hub                EventBroadcaster
	syncer             SessionSyncer
	publisher          EventPublisher
	indexer            EmbeddingIndexer
	detector           *Detector
	buffer             *UtteranceBuffer
	minSessionSegments int
	logger             *slog.Logger
	batchLogger        *slog.Logger
	batchTranscriber   transcribe.BatchTranscriber
	refinementWait     time.Duration

	mu                             sync.Mutex
	currentSessionID               string
	currentStartedAt               time.Time
	manualSession                  bool
	prevTimeout                    time.Duration
	capturePaused                  bool          // no session may start while true
	sessionStarts                  int           // session starts under way
	startsDone                     *sync.Cond    // signalled (with mu) when a session start finishes
	endingSession                  bool          // an end of the current session is under way
	endsDone                       *sync.Cond    // signalled (with mu) when an end of the session finishes
	failedEnd                      sessionEnd    // how far an end of the current session that failed got
	finalizeWaiter                 chan struct{} // closed by Deepgram's next Finalize reply; see AwaitFinalize
	lastIDTime                     time.Time     // the start second the latest session ID names
	lifecyclePublishes             sync.WaitGroup
	lifecycleStartPublishesStopped bool
}

// sessionEnd is how far an end of a session got. An end that fails leaves it
// behind, so retrying the end finishes it with the same end time and recording.
type sessionEnd struct {
	sessionID    string
	endedAt      time.Time // when the session's recording ended
	audioPath    string    // the saved recording, once saved
	saved        bool      // the recorder has saved the recording, or the end gave it up
	saveFailures int       // attempts to save the recording that failed
}

// maxRecordingSaveAttempts is how many times ending a session tries to save
// its recording. One that still cannot be saved (its raw file was deleted, or
// the card stays full) is given up and the session ends without it, so a
// capture pause waiting for the end does not keep the microphone off for good.
const maxRecordingSaveAttempts = 5

func NewManager(store Store, recorder Recorder, summarizer Summarizer, hub EventBroadcaster, detector *Detector, minSessionSegments int, logger ...*slog.Logger) *Manager {
	if detector == nil {
		detector = NewDetector(30 * time.Second)
	}
	if minSessionSegments < 0 {
		minSessionSegments = 0
	}

	l := logging.WithModule(slog.Default(), "session")
	if len(logger) > 0 && logger[0] != nil {
		l = logging.WithModule(logger[0], "session")
	}

	m := &Manager{
		store:              store,
		recorder:           recorder,
		summarizer:         summarizer,
		hub:                hub,
		detector:           detector,
		buffer:             NewUtteranceBuffer(),
		minSessionSegments: minSessionSegments,
		logger:             l,
		batchLogger:        logging.WithModule(l, "transcribe"),
		refinementWait:     30 * time.Second,
	}
	m.startsDone = sync.NewCond(&m.mu)
	m.endsDone = sync.NewCond(&m.mu)
	detector.OnSessionEnd(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.endCurrentSession(ctx)
	})

	return m
}

func (m *Manager) sessionIDForBroadcast() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSessionID
}

func (m *Manager) Message(mr *api.MessageResponse) error {
	if mr.FromFinalize {
		// After the words below are buffered or saved, so a waiter ending the
		// session on this signal saves them too.
		defer m.finalizeReplied()
	}
	if len(mr.Channel.Alternatives) == 0 {
		return nil
	}

	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	if sentence == "" {
		return nil
	}

	// Extract words from the Deepgram response.
	words := make([]transcribe.Word, 0, len(mr.Channel.Alternatives[0].Words))
	for _, word := range mr.Channel.Alternatives[0].Words {
		words = append(words, transcribe.Word{
			Speaker:        word.Speaker,
			PunctuatedWord: word.PunctuatedWord,
			Start:          word.Start,
			End:            word.End,
		})
	}

	// Interim result (not final) — prepend any buffered words for context, then broadcast.
	if !mr.IsFinal {
		if m.hub != nil {
			speaker := -1
			startTime := 0.0
			broadcastText := sentence

			// Prepend buffered text so the interim display shows the full ongoing utterance.
			if buffered := m.buffer.Words(); len(buffered) > 0 {
				var b strings.Builder
				for _, w := range buffered {
					if b.Len() > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(w.PunctuatedWord)
					if speaker == -1 && w.Speaker != nil {
						speaker = *w.Speaker
						startTime = w.Start
					}
				}
				if b.Len() > 0 {
					broadcastText = b.String() + " " + sentence
				}
			}
			// Fall back to current message words for speaker/start.
			if speaker == -1 && len(words) > 0 {
				if words[0].Speaker != nil {
					speaker = *words[0].Speaker
				}
				startTime = words[0].Start
			}
			m.hub.BroadcastLiveTranscriptInterim(m.sessionIDForBroadcast(), speaker, broadcastText, startTime)
		}
		return nil
	}

	// If is_final but no word timings provided, create a fallback word (Fix #7).
	if len(words) == 0 {
		words = []transcribe.Word{{PunctuatedWord: sentence, Start: 0, End: 0}}
	}

	// Final result — buffer words until speech_final.
	m.buffer.AddWords(words)
	m.detector.OnSpeech()

	// After buffering, broadcast an interim event with full buffer contents
	// so the UI always reflects what has been confirmed so far.
	if m.hub != nil {
		if buffered := m.buffer.Words(); len(buffered) > 0 {
			var b strings.Builder
			speaker := -1
			startTime := 0.0
			for _, w := range buffered {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(w.PunctuatedWord)
				if speaker == -1 && w.Speaker != nil {
					speaker = *w.Speaker
					startTime = w.Start
				}
			}
			m.hub.BroadcastLiveTranscriptInterim(m.sessionIDForBroadcast(), speaker, b.String(), startTime)
		}
	}

	// If speech_final, flush the buffer and persist/broadcast.
	if mr.SpeechFinal {
		return m.flushBuffer()
	}

	return nil
}

func (m *Manager) UtteranceEnd(_ *api.UtteranceEndResponse) error {
	if err := m.flushBuffer(); err != nil {
		return err
	}
	m.detector.OnUtteranceEnd()
	return nil
}

func (m *Manager) flushBuffer() error {
	// Take the words and pick their session in one step under m.mu, which
	// PauseCapture and ResumeCapture also hold: words taken before a pause go
	// to the session they were spoken in, or are dropped while paused, and can
	// never open a session after the resume.
	m.mu.Lock()
	words := m.buffer.Flush()
	segments := transcribe.GroupWordsBySpeaker(words)
	if len(segments) == 0 {
		m.mu.Unlock()
		return nil
	}
	sessionID, err := m.claimSessionLocked(time.Now().UTC())
	if err != nil {
		if errors.Is(err, ErrCapturePaused) {
			// Late results for audio sent before the pause: no session may
			// start while paused, so they are dropped.
			m.logger.Info("dropped transcript received while capture is paused", "operation", "flush_buffer", "segments", len(segments))
			return nil
		}
		return err
	}

	for i := range segments {
		segments[i].Timestamp = time.Now().UTC()
		if err := m.store.AppendSegment(sessionID, segments[i]); err != nil {
			return fmt.Errorf("append segment: %w", err)
		}

		if m.hub != nil {
			m.hub.BroadcastLiveTranscript(sessionID, segments[i])
		}
	}
	return nil
}

func (m *Manager) ForceEndSession(ctx context.Context) error {
	// Flush any buffered words before ending — is_final=true words not yet persisted.
	if err := m.flushBuffer(); err != nil && !errors.Is(err, ErrNoActiveSession) {
		// Log flush failure but don't block session end.
		m.logger.Warn("flush buffer failed", "error", err)
	}
	return m.endCurrentSession(ctx)
}

// newSessionIDLocked returns the ID of a session starting at now: its start
// time to the second, moved past the latest ID so two sessions started in the
// same second (a short session ended by a pause, then a resume) never share
// one. Must be called with m.mu held.
func (m *Manager) newSessionIDLocked(now time.Time) string {
	idTime := now.UTC().Truncate(time.Second)
	if !idTime.After(m.lastIDTime) {
		idTime = m.lastIDTime.Add(time.Second)
	}
	m.lastIDTime = idTime
	return idTime.Format("20060102150405")
}

// claimSessionLocked returns the current session, starting one at now when
// there is none, or ErrCapturePaused while capture is paused. It must be called
// with m.mu held and returns with it released.
func (m *Manager) claimSessionLocked(now time.Time) (string, error) {
	if m.currentSessionID != "" {
		sessionID := m.currentSessionID
		m.mu.Unlock()
		return sessionID, nil
	}
	if m.capturePaused {
		m.mu.Unlock()
		return "", ErrCapturePaused
	}

	sessionID := m.newSessionIDLocked(now)
	startedAt := now.UTC()
	m.currentSessionID = sessionID
	m.currentStartedAt = startedAt
	m.sessionStarts++
	m.mu.Unlock()
	defer m.finishSessionStart()

	if err := m.store.CreateSession(sessionID, startedAt); err != nil {
		m.mu.Lock()
		m.currentSessionID = ""
		m.currentStartedAt = time.Time{}
		m.mu.Unlock()
		return "", fmt.Errorf("create session: %w", err)
	}

	if m.recorder != nil {
		if err := m.recorder.StartSession(sessionID); err != nil {
			m.mu.Lock()
			m.currentSessionID = ""
			m.currentStartedAt = time.Time{}
			m.mu.Unlock()
			_ = m.store.EndSession(sessionID, time.Now().UTC(), "")
			return "", fmt.Errorf("start audio recorder session: %w", err)
		}
	}

	if m.hub != nil {
		m.hub.BroadcastSessionStarted(sessionID)
	}
	m.publishSessionStarted(sessionID)

	return sessionID, nil
}

// StopSessionStartPublishes prevents a shutdown from registering a new
// session-started send while WaitForLifecyclePublishes is waiting.
func (m *Manager) StopSessionStartPublishes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycleStartPublishesStopped = true
}

func (m *Manager) publishSessionStarted(sessionID string) {
	m.publishLifecycle(sessionID, "session_started", true, EventPublisher.PublishSessionStarted)
}

func (m *Manager) publishSessionEnded(sessionID string) {
	m.publishLifecycle(sessionID, "session_ended", false, EventPublisher.PublishSessionEnded)
}

func (m *Manager) publishLifecycle(sessionID, eventType string, isStart bool, send func(EventPublisher, context.Context, string) error) {
	m.mu.Lock()
	if isStart && m.lifecycleStartPublishesStopped {
		m.mu.Unlock()
		return
	}
	publisher := m.publisher
	if publisher == nil {
		m.mu.Unlock()
		return
	}
	// Shutdown first prevents new start sends, then waits. Register the send
	// while holding mu so that sequence cannot race with this Add.
	m.lifecyclePublishes.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.lifecyclePublishes.Done()
		if err := send(publisher, context.Background(), sessionID); err != nil {
			m.logger.Error("envoy lifecycle publish failed", "operation", "publish_"+eventType, "session_id", sessionID, "error", err)
		}
	}()
}

// WaitForLifecyclePublishes waits for in-flight session-started and
// session-ended sends, stopping when ctx is cancelled.
func (m *Manager) WaitForLifecyclePublishes(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.lifecyclePublishes.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (m *Manager) endCurrentSession(ctx context.Context) error {
	m.mu.Lock()
	// Another end of the session (silence timer, pause, stop) is under way: wait
	// for it, up to ctx's deadline, instead of reporting no session while one
	// still holds the recorder. Then this end finds the session gone, or
	// retries one that failed.
	if m.endingSession {
		stop := context.AfterFunc(ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.endsDone.Broadcast()
		})
		defer stop()
		for m.endingSession {
			if err := ctx.Err(); err != nil {
				m.mu.Unlock()
				return err
			}
			m.endsDone.Wait()
		}
	}
	sessionID := m.currentSessionID
	startedAt := m.currentStartedAt
	if sessionID == "" {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
	// An earlier end of this session failed: finish that end, with its end
	// time and the recording it saved, instead of starting another.
	end := m.failedEnd
	if end.sessionID != sessionID {
		end = sessionEnd{sessionID: sessionID, endedAt: time.Now().UTC()}
	}
	// Claim the end, so a second caller cannot end the same session again and
	// overwrite its saved audio path.
	m.endingSession = true
	m.mu.Unlock()
	ended := false
	defer func() {
		if !ended {
			m.mu.Lock()
			m.failedEnd = end
			m.endingSession = false
			m.endsDone.Broadcast()
			m.mu.Unlock()
		}
	}()
	endedAt := end.endedAt
	if !end.saved && m.recorder != nil {
		path, err := m.recorder.EndSession(sessionID)
		if err != nil {
			end.saveFailures++
			if end.saveFailures < maxRecordingSaveAttempts {
				return fmt.Errorf("end audio recorder session: %w", err)
			}
			if rawPath := m.recorder.DiscardUnsaved(); rawPath != "" {
				m.logger.Error("recording could not be saved; ending the session without it and keeping its raw audio for recovery by hand", "operation", "end_session", "session_id", sessionID, "attempts", end.saveFailures, "raw_audio", rawPath, "error", err)
			} else {
				m.logger.Error("recording could not be saved and its raw audio is gone; ending the session without it", "operation", "end_session", "session_id", sessionID, "attempts", end.saveFailures, "error", err)
			}
		}
		end.audioPath, end.saved = path, true
	}

	if err := m.store.EndSession(sessionID, endedAt, end.audioPath); err != nil {
		return fmt.Errorf("end session: %w", err)
	}

	m.mu.Lock()
	m.currentSessionID = ""
	m.currentStartedAt = time.Time{}
	m.failedEnd = sessionEnd{}
	m.endManualLocked()
	m.endingSession = false
	m.endsDone.Broadcast()
	ended = true
	minSegs := m.minSessionSegments
	m.mu.Unlock()

	// Check if session meets minimum segment threshold.
	if minSegs > 0 {
		count, err := m.store.CountSegments(sessionID)
		if err == nil && count < minSegs {
			if m.hub != nil {
				m.hub.BroadcastSessionEnded(sessionID, endedAt.Sub(startedAt))
			}
			m.publishSessionEnded(sessionID)
			if discardErr := m.store.DiscardSession(sessionID); discardErr != nil {
				m.logger.Warn("failed to discard short session", "operation", "discard_short_session", "session_id", sessionID, "error", discardErr)
			} else {
				m.logger.Info("discarded short session", "operation", "discard_short_session", "session_id", sessionID, "segments", count, "minimum_segments", minSegs)
			}
			return nil
		}
	}

	if m.hub != nil {
		m.hub.BroadcastSessionEnded(sessionID, endedAt.Sub(startedAt))
	}
	m.publishSessionEnded(sessionID)

	m.mu.Lock()
	batchTranscriber := m.batchTranscriber
	m.mu.Unlock()

	if batchTranscriber != nil && strings.TrimSpace(end.audioPath) != "" {
		go m.runBatchRefinement(context.Background(), sessionID, end.audioPath)
	}

	go m.generateSummary(context.Background(), sessionID, startedAt)
	return nil
}

func (m *Manager) runBatchRefinement(ctx context.Context, sessionID, audioPath string) {
	m.mu.Lock()
	batchTranscriber := m.batchTranscriber
	m.mu.Unlock()
	if batchTranscriber == nil {
		return
	}

	// Claim the refinement only while the session still names the recording:
	// disk pressure may have removed it since the session ended, and it
	// removes nothing while the refinement runs.
	claimed, err := m.store.ClaimRefinement(sessionID, audioPath)
	if err != nil {
		m.batchLogger.Error("failed to mark refinement running", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
		return
	}
	if !claimed {
		m.batchLogger.Info("batch refinement not started: the session no longer has this recording (removed to free space)", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath)
		return
	}

	m.batchLogger.Info("starting batch refinement", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath)
	transcript, err := batchTranscriber.Transcribe(ctx, audioPath)
	if err != nil {
		if updateErr := m.store.UpdateRefinement(sessionID, "", storage.RefinementFailed); updateErr != nil {
			m.batchLogger.Error("failed to mark refinement failed", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", updateErr)
		}
		m.batchLogger.Error("batch refinement failed", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
		if m.hub != nil {
			m.hub.BroadcastComponentStatus("refinement", storage.ComponentStatusError, fmt.Sprintf("Batch refinement failed for session %s", sessionID))
		}
		return
	}

	if err := m.store.UpdateRefinement(sessionID, transcript, storage.RefinementCompleted); err != nil {
		m.batchLogger.Error("failed to store refined transcript", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
		return
	}

	m.batchLogger.Info("batch refinement completed", "operation", "batch_refinement", "session_id", sessionID)
	if m.hub != nil {
		m.hub.BroadcastComponentStatus("refinement", storage.ComponentStatusConnected, fmt.Sprintf("Batch refinement completed for session %s", sessionID))
	}

	// Re-canonicalize now that refined transcript is available.
	if _, err := m.store.Canonicalize(sessionID); err != nil {
		m.batchLogger.Error("failed to re-canonicalize after batch refinement", "operation", "batch_refinement", "session_id", sessionID, "error", err)
	}
}

// RefineSession re-runs batch refinement for an already-ended session using its
// recorded audio, then regenerates the summary from the refined transcript. It
// recovers sessions that ended without a usable live transcript (e.g. when live
// transcription was unavailable at capture time). Intended to run off the
// request path.
func (m *Manager) RefineSession(ctx context.Context, sessionID, audioPath string, startedAt time.Time) error {
	if strings.TrimSpace(audioPath) == "" {
		return fmt.Errorf("session %s has no audio to refine", sessionID)
	}

	m.mu.Lock()
	batchTranscriber := m.batchTranscriber
	m.mu.Unlock()
	if batchTranscriber == nil {
		return fmt.Errorf("batch transcription not configured")
	}

	m.runBatchRefinement(ctx, sessionID, audioPath)
	m.generateSummary(ctx, sessionID, startedAt)
	return nil
}

func (m *Manager) generateSummary(ctx context.Context, sessionID string, startedAt time.Time) {
	m.mu.Lock()
	summarizer := m.summarizer
	syncer := m.syncer
	publisher := m.publisher
	m.mu.Unlock()

	if summarizer == nil {
		// Still canonicalize for GDrive sync even without a summarizer.
		if err := m.store.StartSummary(sessionID); err != nil {
			m.logger.Error("failed to start empty summary", "operation", "generate_summary", "session_id", sessionID, "error", err)
			return
		}
		generation, err := m.store.Canonicalize(sessionID)
		if err != nil {
			m.logger.Error("failed to canonicalize transcript", "operation", "generate_summary", "session_id", sessionID, "error", err)
			return
		}
		if err := m.store.UpdateSummaryAtGeneration(sessionID, "", "", storage.SummaryCompleted, "", "{}", generation); err != nil {
			m.logger.Error("failed to mark empty summary complete", "operation", "generate_summary", "session_id", sessionID, "error", err)
			return
		}
		if transcript, _, err := m.store.GetCanonicalTranscript(sessionID); err != nil {
			m.logger.Error("failed to get canonical transcript for indexing", "operation", "generate_summary", "session_id", sessionID, "error", err)
		} else {
			m.triggerEmbeddingIndexAtGeneration(sessionID, transcript, generation)
		}
		if syncer != nil {
			go func() {
				if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
					m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
					if m.hub != nil {
						m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
					}
				} else if m.hub != nil {
					m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusConnected, fmt.Sprintf("Google Drive synced for session %s", sessionID))
				}
			}()
		}
		return
	}

	_ = m.store.StartSummary(sessionID)

	// Wait for batch refinement if configured, then canonicalize.
	m.mu.Lock()
	batchConfigured := m.batchTranscriber != nil
	m.mu.Unlock()
	if batchConfigured {
		_ = m.waitForRefinedTranscript(ctx, sessionID)
	}

	// Canonicalize the transcript (picks refined if available, else assembles streaming).
	generation, err := m.store.Canonicalize(sessionID)
	if err != nil {
		m.failSummary(sessionID, fmt.Sprintf("failed to canonicalize transcript: %v", err))
		m.logger.Error("failed to canonicalize transcript", "operation", "generate_summary", "session_id", sessionID, "error", err)
		return
	}

	transcript, _, err := m.store.GetCanonicalTranscript(sessionID)
	if err != nil {
		m.failSummary(sessionID, fmt.Sprintf("failed to get canonical transcript: %v", err))
		m.logger.Error("failed to get canonical transcript for summarization", "operation", "generate_summary", "session_id", sessionID, "error", err)
		if m.hub != nil {
			m.hub.BroadcastComponentStatus("summary", storage.ComponentStatusError, fmt.Sprintf("Failed to retrieve transcript for session %s", sessionID))
		}
		return
	}

	// For title fallback, still need segments.
	segments, err := m.store.GetSegments(sessionID)
	if err != nil {
		m.logger.Warn("failed to get segments for title fallback", "operation", "generate_summary", "session_id", sessionID, "error", err)
	}

	title, summaryText, preset, speakerNames, err := summarizer.Summarize(ctx, sessionID, transcript)
	// Safety net: ensure title is never empty using fallback chain.
	if strings.TrimSpace(title) == "" {
		title = summary.GenerateTitle("", transcript, segments, startedAt)
	}
	if err != nil {
		m.logger.Error("summarization failed", "operation", "generate_summary", "session_id", sessionID, "error", err)
		m.failSummary(sessionID, fmt.Sprintf("summarization failed: %v", err))
		m.triggerEmbeddingIndexAtGeneration(sessionID, transcript, generation)
		if m.hub != nil {
			m.hub.BroadcastComponentStatus("summary", storage.ComponentStatusError, fmt.Sprintf("Summarization failed for session %s", sessionID))
		}
		return
	}

	storeErr := m.store.UpdateSummaryAtGeneration(sessionID, title, summaryText, storage.SummaryCompleted, preset, speakerNames, generation)
	if storeErr != nil {
		m.logger.Error("failed to store summary", "operation", "generate_summary", "session_id", sessionID, "error", storeErr)
		m.failSummary(sessionID, fmt.Sprintf("failed to store summary: %v", storeErr))
		if m.hub != nil {
			m.hub.BroadcastComponentStatus("summary", storage.ComponentStatusError, fmt.Sprintf("Failed to store summary for session %s", sessionID))
		}
		return
	}

	m.broadcastSummaryStatus(sessionID, title, summaryText, storage.SummaryCompleted, preset)
	m.triggerEmbeddingIndexAtGeneration(sessionID, transcript, generation)

	if syncer != nil {
		go func() {
			if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
				m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
				if m.hub != nil {
					m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
				}
			} else if m.hub != nil {
				m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusConnected, fmt.Sprintf("Google Drive synced for session %s", sessionID))
			}
		}()
	}

	if publisher != nil && strings.TrimSpace(summaryText) != "" {
		go func() {
			if err := publisher.PublishSummaryReady(context.Background(), sessionID); err != nil {
				m.logger.Error("envoy publish failed", "operation", "publish_summary_ready", "session_id", sessionID, "error", err)
			}
		}()
	}
}

func (m *Manager) broadcastSummaryStatus(sessionID, title, summary, status, preset string) {
	if m.hub != nil {
		m.hub.BroadcastSummaryReady(sessionID, title, summary, status, preset)
	}
}

// failSummary records that making session sessionID's summary failed, and why
// (reason), and announces the title and summary the session keeps: its last
// good ones, if it has them.
func (m *Manager) failSummary(sessionID, reason string) {
	kept, err := m.store.FailSummary(sessionID, reason)
	if err != nil {
		m.logger.Error("failed to record the failed summary", "operation", "generate_summary", "session_id", sessionID, "reason", reason, "error", err)
		return
	}
	m.broadcastSummaryStatus(sessionID, kept.Title, kept.Summary, storage.SummaryFailed, kept.SummaryPreset)
}

func (m *Manager) ActiveSession() (string, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSessionID, m.currentStartedAt
}

// SetSummarizer replaces the summarizer used for new sessions.
// Safe to call concurrently.
func (m *Manager) SetSummarizer(s Summarizer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summarizer = s
}

func (m *Manager) SetSyncer(s SessionSyncer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncer = s
}

func (m *Manager) SetEventPublisher(p EventPublisher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publisher = p
}

func (m *Manager) SetIndexer(indexer EmbeddingIndexer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.indexer = indexer
}

func (m *Manager) SetMinSessionSegments(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n < 0 {
		n = 0
	}
	m.minSessionSegments = n
}

func (m *Manager) SetBatchTranscriber(t transcribe.BatchTranscriber) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchTranscriber = t
}

func (m *Manager) SetRefinementWaitTimeout(timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timeout <= 0 {
		m.refinementWait = 30 * time.Second
		return
	}
	m.refinementWait = timeout
}

func (m *Manager) waitForRefinedTranscript(ctx context.Context, sessionID string) string {
	m.mu.Lock()
	waitTimeout := m.refinementWait
	m.mu.Unlock()

	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		transcript, status, err := m.store.GetRefinement(sessionID)
		if err != nil {
			m.batchLogger.Warn("failed to read refinement state", "operation", "wait_refinement", "session_id", sessionID, "error", err)
			return ""
		}

		switch status {
		case storage.RefinementCompleted:
			return strings.TrimSpace(transcript)
		case storage.RefinementFailed:
			return ""
		case storage.RefinementPending, storage.RefinementRunning:
			if time.Now().After(deadline) {
				return ""
			}
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(100 * time.Millisecond):
			}
		default:
			return ""
		}
	}
}

func (m *Manager) triggerEmbeddingIndexAtGeneration(sessionID, transcript string, generation int) {
	m.mu.Lock()
	indexer := m.indexer
	m.mu.Unlock()

	if indexer == nil || strings.TrimSpace(transcript) == "" {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := indexer.IndexSessionAtGeneration(ctx, sessionID, transcript, generation); err != nil {
			m.logger.Error("embedding index failed", "operation", "generate_summary", "session_id", sessionID, "error", err)
		}
	}()
}

func (m *Manager) OnTranscriptionDisconnect() {
	m.mu.Lock()
	if m.buffer == nil {
		m.mu.Unlock()
		return
	}
	detector := m.detector
	m.mu.Unlock()

	// Persist buffered words — flushBuffer takes m.mu itself, so it must NOT
	// be held here.
	if err := m.flushBuffer(); err != nil {
		m.logger.Error("failed to persist buffered words on disconnect", "operation", "flush_buffer", "error", err)
	}

	if detector != nil {
		detector.OnSpeech()
	}
}

// ManualStartSession starts a new session manually. Returns the session ID.
// If a session is already active, returns ErrSessionAlreadyActive; while
// capture is paused, returns ErrCapturePaused.
// Manual sessions use a 5-minute silence timeout instead of the default 30s.
func (m *Manager) ManualStartSession(ctx context.Context, titleHint string) (string, error) {
	now := time.Now().UTC()

	m.mu.Lock()
	if m.currentSessionID != "" {
		m.mu.Unlock()
		return "", ErrSessionAlreadyActive
	}
	if m.capturePaused {
		m.mu.Unlock()
		return "", ErrCapturePaused
	}

	sessionID := m.newSessionIDLocked(now)
	m.currentSessionID = sessionID
	m.currentStartedAt = now
	m.manualSession = true
	// Manual sessions use a 5-minute silence timeout instead of the default,
	// set in the same hold that claims the session, so no end of an earlier
	// session can restore over it.
	if m.detector != nil {
		m.prevTimeout = m.detector.Timeout()
		m.detector.SetTimeout(5 * time.Minute)
	}
	m.sessionStarts++
	m.mu.Unlock()
	defer m.finishSessionStart()

	if err := m.store.CreateSession(sessionID, now); err != nil {
		m.mu.Lock()
		m.currentSessionID = ""
		m.currentStartedAt = time.Time{}
		m.endManualLocked()
		m.mu.Unlock()
		return "", fmt.Errorf("create session: %w", err)
	}

	// Set title hint if provided.
	if titleHint != "" {
		if err := m.store.UpdateTitle(sessionID, titleHint); err != nil {
			m.logger.Warn("failed to set title hint on manual session", "operation", "manual_start", "session_id", sessionID, "error", err)
		}
	}

	if m.recorder != nil {
		if err := m.recorder.StartSession(sessionID); err != nil {
			m.mu.Lock()
			m.currentSessionID = ""
			m.currentStartedAt = time.Time{}
			m.endManualLocked()
			m.mu.Unlock()
			_ = m.store.EndSession(sessionID, time.Now().UTC(), "")
			return "", fmt.Errorf("start audio recorder session: %w", err)
		}
	}

	if m.hub != nil {
		m.hub.BroadcastSessionStarted(sessionID)
	}
	m.publishSessionStarted(sessionID)

	m.logger.Info("manual session started", "operation", "manual_start", "session_id", sessionID, "title_hint", titleHint)
	return sessionID, nil
}

// StopSession ends a specific session by ID. If the given sessionID does not
// match the currently active session, returns ErrNoActiveSession.
func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	if m.currentSessionID != sessionID {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
	m.mu.Unlock()

	return m.endCurrentSession(ctx)
}

// endManualLocked ends a manual session's longer silence timeout, restoring
// the one it replaced. It runs with m.mu held, in the same hold that ends the
// session or undoes its start, so a manual session started right after keeps
// its own timeout, and a session whose end failed (still current) keeps its
// timeout. It takes the detector's lock inside m.mu; the detector never takes
// m.mu.
func (m *Manager) endManualLocked() {
	if m.manualSession && m.prevTimeout > 0 && m.detector != nil {
		m.detector.SetTimeout(m.prevTimeout)
	}
	m.manualSession = false
	m.prevTimeout = 0
}

// PauseCapture stops new sessions from starting until ResumeCapture. It only
// sets a flag and waits for nothing, so the capture gate can close at once; a
// session start already under way still finishes, and WaitForSessionStarts
// waits for it. It does not end the active session; the capture pause does
// that once Deepgram's last words are in.
func (m *Manager) PauseCapture() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capturePaused = true
}

// WaitForSessionStarts waits until no session start is under way, so a session
// end that follows a pause never runs on a half-created session. A start
// writes to the database, so this waits as long as the database does.
func (m *Manager) WaitForSessionStarts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.sessionStarts > 0 {
		m.startsDone.Wait()
	}
}

func (m *Manager) finishSessionStart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionStarts--
	m.startsDone.Broadcast()
}

// AwaitFinalize returns a channel that is closed once Deepgram's reply to a
// Finalize request (a result marked from_finalize) has been handled. Call it
// before sending Finalize. A later call replaces the earlier wait.
func (m *Manager) AwaitFinalize() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finalizeWaiter = make(chan struct{})
	return m.finalizeWaiter
}

func (m *Manager) finalizeReplied() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finalizeWaiter != nil {
		close(m.finalizeWaiter)
		m.finalizeWaiter = nil
	}
}

// ResumeCapture lets sessions start again after PauseCapture. It drops words
// Deepgram finalized during the pause that no utterance end flushed: they are
// from audio before the pause and must not open the first session after it.
func (m *Manager) ResumeCapture() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buffer.Flush()
	m.capturePaused = false
}
