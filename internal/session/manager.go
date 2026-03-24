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
	detector           *Detector
	buffer             *UtteranceBuffer
	minSessionSegments int
	logger             *slog.Logger
	batchLogger        *slog.Logger
	batchTranscriber   transcribe.BatchTranscriber
	refinementWait     time.Duration

	mu               sync.Mutex
	currentSessionID string
	currentStartedAt time.Time
	manualSession    bool
	prevTimeout      time.Duration
}

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

	detector.OnSessionEnd(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.endCurrentSession(ctx)
	})

	return m
}

func (m *Manager) Message(mr *api.MessageResponse) error {
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
			m.hub.BroadcastLiveTranscriptInterim(speaker, broadcastText, startTime)
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
			m.hub.BroadcastLiveTranscriptInterim(speaker, b.String(), startTime)
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
	words := m.buffer.Flush()
	if len(words) == 0 {
		return nil
	}

	segments := transcribe.GroupWordsBySpeaker(words)
	if len(segments) == 0 {
		return nil
	}

	for i := range segments {
		segments[i].Timestamp = time.Now().UTC()
		if err := m.ensureSessionStarted(segments[i].Timestamp); err != nil {
			return err
		}

		sessionID := m.currentSession()
		if err := m.store.AppendSegment(sessionID, segments[i]); err != nil {
			return fmt.Errorf("append segment: %w", err)
		}

		if m.hub != nil {
			m.hub.BroadcastLiveTranscript(segments[i])
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
	return m.endCurrentSessionAndRestore(ctx)
}

func (m *Manager) ensureSessionStarted(now time.Time) error {
	m.mu.Lock()
	if m.currentSessionID != "" {
		m.mu.Unlock()
		return nil
	}

	sessionID := now.UTC().Format("20060102150405")
	if m.currentStartedAt.Format("20060102150405") == sessionID {
		sessionID = now.UTC().Add(time.Second).Format("20060102150405")
	}
	startedAt := now.UTC()
	m.currentSessionID = sessionID
	m.currentStartedAt = startedAt
	m.mu.Unlock()

	if err := m.store.CreateSession(sessionID, startedAt); err != nil {
		m.mu.Lock()
		m.currentSessionID = ""
		m.currentStartedAt = time.Time{}
		m.mu.Unlock()
		return fmt.Errorf("create session: %w", err)
	}

	if m.recorder != nil {
		if err := m.recorder.StartSession(sessionID); err != nil {
			m.mu.Lock()
			m.currentSessionID = ""
			m.currentStartedAt = time.Time{}
			m.mu.Unlock()
			_ = m.store.EndSession(sessionID, time.Now().UTC(), "")
			return fmt.Errorf("start audio recorder session: %w", err)
		}
	}

	if m.hub != nil {
		m.hub.BroadcastSessionStarted(sessionID)
	}

	return nil
}

func (m *Manager) endCurrentSession(ctx context.Context) error {
	m.mu.Lock()
	sessionID := m.currentSessionID
	startedAt := m.currentStartedAt
	if sessionID == "" {
		m.mu.Unlock()
		return ErrNoActiveSession
	}

	m.mu.Unlock()

	endedAt := time.Now().UTC()
	audioPath := ""
	if m.recorder != nil {
		path, err := m.recorder.EndSession()
		if err != nil {
			return fmt.Errorf("end audio recorder session: %w", err)
		}
		audioPath = path
	}

	if err := m.store.EndSession(sessionID, endedAt, audioPath); err != nil {
		return fmt.Errorf("end session: %w", err)
	}

	m.mu.Lock()
	m.currentSessionID = ""
	m.currentStartedAt = time.Time{}
	minSegs := m.minSessionSegments
	m.mu.Unlock()

	// Check if session meets minimum segment threshold.
	if minSegs > 0 {
		count, err := m.store.CountSegments(sessionID)
		if err == nil && count < minSegs {
			if m.hub != nil {
				m.hub.BroadcastSessionEnded(sessionID, endedAt.Sub(startedAt))
			}
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

	m.mu.Lock()
	batchTranscriber := m.batchTranscriber
	m.mu.Unlock()

	if batchTranscriber != nil && strings.TrimSpace(audioPath) != "" {
		go m.runBatchRefinement(context.Background(), sessionID, audioPath)
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

	if err := m.store.UpdateRefinement(sessionID, "", storage.RefinementRunning); err != nil {
		m.batchLogger.Error("failed to mark refinement running", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
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
	if err := m.store.Canonicalize(sessionID); err != nil {
		m.batchLogger.Error("failed to re-canonicalize after batch refinement", "operation", "batch_refinement", "session_id", sessionID, "error", err)
	}
}

func (m *Manager) generateSummary(ctx context.Context, sessionID string, startedAt time.Time) {
	m.mu.Lock()
	summarizer := m.summarizer
	syncer := m.syncer
	m.mu.Unlock()

	if summarizer == nil {
		// Still canonicalize for GDrive sync even without summarizer.
		if err := m.store.Canonicalize(sessionID); err != nil {
			m.logger.Error("failed to canonicalize transcript", "operation", "generate_summary", "session_id", sessionID, "error", err)
		}
		if syncer != nil {
			go func() {
				if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
					m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
					if m.hub != nil {
						m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
					}
				}
			}()
		}
		return
	}

	_ = m.store.UpdateSummary(sessionID, "", "", storage.SummaryRunning, "")

	// Wait for batch refinement if configured, then canonicalize.
	m.mu.Lock()
	batchConfigured := m.batchTranscriber != nil
	m.mu.Unlock()
	if batchConfigured {
		_ = m.waitForRefinedTranscript(ctx, sessionID)
	}

	// Canonicalize the transcript (picks refined if available, else assembles streaming).
	if err := m.store.Canonicalize(sessionID); err != nil {
		m.logger.Error("failed to canonicalize transcript", "operation", "generate_summary", "session_id", sessionID, "error", err)
	}

	transcript, _, err := m.store.GetCanonicalTranscript(sessionID)
	if err != nil {
		_ = m.store.UpdateSummary(sessionID, "", "", storage.SummaryFailed, "")
		m.broadcastSummaryStatus(sessionID, "", "", storage.SummaryFailed, "")
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

	title, summaryText, preset, err := summarizer.Summarize(ctx, sessionID, transcript)
	// Safety net: ensure title is never empty using fallback chain.
	if strings.TrimSpace(title) == "" {
		title = summary.GenerateTitle("", transcript, segments, startedAt)
	}
	if err != nil {
		m.logger.Error("summarization failed", "operation", "generate_summary", "session_id", sessionID, "error", err)
		_ = m.store.UpdateSummary(sessionID, "", "", storage.SummaryFailed, preset)
		m.broadcastSummaryStatus(sessionID, "", "", storage.SummaryFailed, preset)
		if m.hub != nil {
			m.hub.BroadcastComponentStatus("summary", storage.ComponentStatusError, fmt.Sprintf("Summarization failed for session %s", sessionID))
		}
		return
	}

	if err := m.store.UpdateSummary(sessionID, title, summaryText, storage.SummaryCompleted, preset); err != nil {
		m.logger.Error("failed to store summary", "operation", "generate_summary", "session_id", sessionID, "error", err)
		_ = m.store.UpdateSummary(sessionID, "", "", storage.SummaryFailed, preset)
		m.broadcastSummaryStatus(sessionID, "", "", storage.SummaryFailed, preset)
		if m.hub != nil {
			m.hub.BroadcastComponentStatus("summary", storage.ComponentStatusError, fmt.Sprintf("Failed to store summary for session %s", sessionID))
		}
		return
	}

	m.broadcastSummaryStatus(sessionID, title, summaryText, storage.SummaryCompleted, preset)

	if syncer != nil {
		go func() {
			if err := syncer.SyncSession(context.Background(), sessionID); err != nil {
				m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
				if m.hub != nil {
					m.hub.BroadcastComponentStatus("sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
				}
			}
		}()
	}
}

func (m *Manager) broadcastSummaryStatus(sessionID, title, summary, status, preset string) {
	if m.hub != nil {
		m.hub.BroadcastSummaryReady(sessionID, title, summary, status, preset)
	}
}

func (m *Manager) currentSession() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSessionID
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

func (m *Manager) OnTranscriptionDisconnect() {
	m.mu.Lock()
	if m.buffer == nil {
		m.mu.Unlock()
		return
	}
	detector := m.detector
	m.mu.Unlock()

	// Persist buffered words — flushBuffer handles its own locking
	// via ensureSessionStarted/currentSession, so m.mu must NOT be held.
	if err := m.flushBuffer(); err != nil {
		m.logger.Error("failed to persist buffered words on disconnect", "operation", "flush_buffer", "error", err)
	}

	if detector != nil {
		detector.OnSpeech()
	}
}

// ManualStartSession starts a new session manually. Returns the session ID.
// If a session is already active, returns ErrSessionAlreadyActive.
// Manual sessions use a 5-minute silence timeout instead of the default 30s.
func (m *Manager) ManualStartSession(ctx context.Context, titleHint string) (string, error) {
	now := time.Now().UTC()

	m.mu.Lock()
	if m.currentSessionID != "" {
		m.mu.Unlock()
		return "", ErrSessionAlreadyActive
	}

	sessionID := now.Format("20060102150405")
	if m.currentStartedAt.Format("20060102150405") == sessionID {
		sessionID = now.Add(time.Second).Format("20060102150405")
	}
	m.currentSessionID = sessionID
	m.currentStartedAt = now
	m.manualSession = true
	m.mu.Unlock()

	if err := m.store.CreateSession(sessionID, now); err != nil {
		m.mu.Lock()
		m.currentSessionID = ""
		m.currentStartedAt = time.Time{}
		m.manualSession = false
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
			m.manualSession = false
			m.mu.Unlock()
			_ = m.store.EndSession(sessionID, time.Now().UTC(), "")
			return "", fmt.Errorf("start audio recorder session: %w", err)
		}
	}

	// Extend silence timeout for manual sessions (5 min vs default 30s).
	m.mu.Lock()
	detector := m.detector
	m.mu.Unlock()
	if detector != nil {
		m.mu.Lock()
		m.prevTimeout = detector.timeout
		m.mu.Unlock()
		detector.SetTimeout(5 * time.Minute)
	}

	if m.hub != nil {
		m.hub.BroadcastSessionStarted(sessionID)
	}

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

	return m.endCurrentSessionAndRestore(ctx)
}

// endCurrentSessionAndRestore ends the current session and restores the
// silence timeout if it was changed for a manual session.
func (m *Manager) endCurrentSessionAndRestore(ctx context.Context) error {
	m.mu.Lock()
	wasManual := m.manualSession
	prevTimeout := m.prevTimeout
	m.mu.Unlock()

	err := m.endCurrentSession(ctx)

	// Restore previous silence timeout if this was a manual session.
	if wasManual && prevTimeout > 0 {
		m.mu.Lock()
		detector := m.detector
		m.manualSession = false
		m.prevTimeout = 0
		m.mu.Unlock()
		if detector != nil {
			detector.SetTimeout(prevTimeout)
		}
	}

	return err
}
