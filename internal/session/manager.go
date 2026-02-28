package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Manager struct {
	store      Store
	recorder   Recorder
	summarizer Summarizer
	hub        EventBroadcaster
	detector   *Detector
	buffer   *UtteranceBuffer

	mu               sync.Mutex
	currentSessionID string
	currentStartedAt time.Time
}

func NewManager(store Store, recorder Recorder, summarizer Summarizer, hub EventBroadcaster, detector *Detector) *Manager {
	if detector == nil {
		detector = NewDetector(30 * time.Second)
	}

	m := &Manager{
		store:      store,
		recorder:   recorder,
		summarizer: summarizer,
		hub:        hub,
		detector:   detector,
		buffer:     NewUtteranceBuffer(),
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

	// Interim result (not final) — broadcast for faded live display.
	if !mr.IsFinal {
		if m.hub != nil {
			speaker := -1
			startTime := 0.0
			if len(words) > 0 {
				if words[0].Speaker != nil {
					speaker = *words[0].Speaker
				}
				startTime = words[0].Start
			}
			m.hub.BroadcastLiveTranscriptInterim(speaker, sentence, startTime)
		}
		return nil
	}

	// Final result — buffer words until speech_final.
	m.buffer.AddWords(words)
	m.detector.OnSpeech()

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
	return m.endCurrentSession(ctx)
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
		return nil
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
	m.mu.Unlock()

	if m.hub != nil {
		m.hub.BroadcastSessionEnded(sessionID, endedAt.Sub(startedAt))
	}

	go m.generateSummary(context.Background(), sessionID)
	return nil
}

func (m *Manager) generateSummary(ctx context.Context, sessionID string) {
	if m.summarizer == nil {
		_ = m.store.UpdateSummary(sessionID, "", storage.SummaryCompleted, "")
		return
	}

	_ = m.store.UpdateSummary(sessionID, "", storage.SummaryRunning, "")

	segments, err := m.store.GetSegments(sessionID)
	if err != nil {
		_ = m.store.UpdateSummary(sessionID, "", storage.SummaryFailed, "")
		m.broadcastSummaryStatus(sessionID, "", storage.SummaryFailed, "")
		return
	}

	var b strings.Builder
	for _, segment := range segments {
		if strings.TrimSpace(segment.Text) == "" {
			continue
		}
		b.WriteString(segment.Text)
		b.WriteString("\n")
	}

	summaryText, preset, err := m.summarizer.Summarize(ctx, sessionID, b.String())
	if err != nil {
		_ = m.store.UpdateSummary(sessionID, "", storage.SummaryFailed, preset)
		m.broadcastSummaryStatus(sessionID, "", storage.SummaryFailed, preset)
		return
	}

	if err := m.store.UpdateSummary(sessionID, summaryText, storage.SummaryCompleted, preset); err != nil {
		_ = m.store.UpdateSummary(sessionID, "", storage.SummaryFailed, preset)
		m.broadcastSummaryStatus(sessionID, "", storage.SummaryFailed, preset)
		return
	}

	m.broadcastSummaryStatus(sessionID, summaryText, storage.SummaryCompleted, preset)
}

func (m *Manager) broadcastSummaryStatus(sessionID, summary, status, preset string) {
	if m.hub != nil {
		m.hub.BroadcastSummaryReady(sessionID, summary, status, preset)
	}
}

func (m *Manager) currentSession() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSessionID
}
