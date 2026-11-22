package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
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
	refinementWarning  func(sessionID, warning string)
	sessionEnded       func(sessionID string)
	sessionStarted     func(sessionID string, seq uint64)

	mu               sync.Mutex
	currentSessionID string
	currentStartedAt time.Time
	// meeting is the current session's meeting context, which ends in the
	// same step that clears or replaces the session (moveSessionLocked),
	// before any session-ended hook runs: what joined agents and the voice
	// do for a meeting ends with it there (MeetingContext).
	meeting                        context.Context
	meetingCancel                  context.CancelFunc
	manualSession                  bool
	prevTimeout                    time.Duration
	capturePaused                  bool              // no session may start while true
	sessionStarts                  int               // session starts under way
	sessionSeq                     uint64            // settled session starts so far; each start's sequence number (SessionMark)
	startIDs                       map[uint64]string // the last startIDsKept settled starts' ids, by sequence number (SessionOfStart)
	startsDone                     *sync.Cond        // signalled (with mu) when a session start finishes
	endingSession                  bool              // an end of the current session is under way
	endsDone                       *sync.Cond        // signalled (with mu) when an end of the session finishes, when endsRunning falls, and when a flush ends
	endsRunning                    int               // session ends under way, from their claim until they return (WaitForLifecyclePublishes)
	flushesRunning                 int               // flushes storing the words they took from the buffer (flushBuffer); an end waits for them before it writes
	sealedSession                  string            // a session whose end is writing it: no flush takes words for it (endSession)
	failedEnd                      sessionEnd        // how far an end of the current session that failed got
	finalizeWaiter                 chan struct{}     // closed by Deepgram's next Finalize reply; see AwaitFinalize
	lastIDTime                     time.Time         // the start second the latest session ID names
	stream                         LiveStream        // the live transcription's audio stream (SetLiveStream); nil until set
	floor                          time.Duration     // on the live stream's Captured() count, no word from before it reaches a later session (afterFloorLocked); 0 before any
	resumes                        int               // capture resumes so far
	lifecyclePublishes             sync.WaitGroup
	lifecycleStartPublishesStopped bool

	// background is the context of the work StopBackground cancels, with
	// stopping.ErrStopped, and waits for, so the store closes under none of
	// it: every goroutine that writes the store and that no caller waits for
	// starts through Go, or runs inside Run. That is the work a session's end
	// leaves running (its refinement and summary, and the sync, index and
	// publish a summary starts), this manager's own entry points
	// (RefineSession, SummarizeSession, the session-ended hook), and the app's
	// background work on the store. Once stopped (backgroundStopped, under mu),
	// nothing more starts. backgroundRunning, under mu, counts the work still
	// running.
	background        context.Context
	stopBackground    context.CancelCauseFunc
	backgroundWork    sync.WaitGroup
	backgroundRunning int
	backgroundStopped bool
}

// sessionEnd is how far an end of a session got. An end that fails leaves it
// behind, so retrying the end finishes it with the same end time and recording.
type sessionEnd struct {
	sessionID    string
	endedAt      time.Time // when the session's recording ended
	audioPath    string    // the saved recording, once saved
	saved        bool      // the recorder has saved the recording, or the end gave it up
	saveFailures int       // attempts to save the recording that failed
	closeStored  bool      // endedAt is in the store (SessionStore.RecordSessionClose)
}

// maxRecordingSaveAttempts is how many times ending a session tries to save
// its recording. One that still cannot be saved (its raw file was deleted, or
// the card stays full) is given up and the session ends without it, so a
// capture pause waiting for the end does not keep the microphone off for good.
const maxRecordingSaveAttempts = 5

// maxSessionIDAttempts is how many ids in a row a session start tries before
// it gives up: each id it finds taken moves it to the next second.
const maxSessionIDAttempts = 100

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
	m.background, m.stopBackground = context.WithCancelCause(context.Background())
	detector.OnSessionEnd(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The silence is the current session's: a session that opens while
		// this end waits for another (openWaitingSession) has its own timer.
		m.mu.Lock()
		silent := m.currentSessionID
		m.mu.Unlock()
		if silent != "" {
			_ = m.endSession(ctx, silent)
		}
	})

	return m
}

// Message takes a Deepgram live result (MessageKept).
func (m *Manager) Message(mr *api.MessageResponse) error {
	_, err := m.MessageKept(mr)
	return err
}

// MessageKept is MessageKeptOn for a result of the live stream's current
// socket (SetLiveStream).
func (m *Manager) MessageKept(mr *api.MessageResponse) (*api.MessageResponse, error) {
	return m.MessageKeptOn(nil, mr)
}

// MessageKeptOn takes a Deepgram live result, timed in stream, the stream of
// the socket that heard it, into the live transcript; a nil stream is the
// live stream's (SetLiveStream). It returns the part of the result the
// transcript keeps: mr itself; a copy without its leading words from before
// the floor (afterFloorLocked), whose Transcript is rebuilt from the words
// kept, whose Words is a subslice of mr's, and whose other fields are mr's,
// to be read only; or nil when it keeps nothing. It also returns nil, and
// shows the page nothing, when capture was paused or resumed while it took
// mr: such words are from before the pause, and go at most to the session
// the pause ends, never to a later one. The voice reads the result it
// returns, so no word from before a pause, or from an ended recording, can
// wake it.
func (m *Manager) MessageKeptOn(stream LiveStream, mr *api.MessageResponse) (*api.MessageResponse, error) {
	if mr.FromFinalize {
		// After the words below are buffered or saved, so a waiter ending the
		// session on this signal saves them too.
		defer m.finalizeReplied()
	}
	if len(mr.Channel.Alternatives) == 0 || strings.TrimSpace(mr.Channel.Alternatives[0].Transcript) == "" {
		if mr.IsFinal {
			m.releasePreroll()
		}
		return nil, nil
	}
	m.mu.Lock()
	if stream == nil {
		stream = m.stream
	}
	kept := m.afterFloorLocked(stream, mr)
	m.mu.Unlock()
	if kept == nil {
		if mr.IsFinal {
			m.releasePreroll()
		}
		return nil, nil
	}
	m.holdPreroll(stream, heardFrom(kept, liveWords(kept)))

	// Take the words: place them again, as a resume or the end of a session
	// may have moved the floor since, and buffer a final's, in one step under
	// m.mu, which ResumeCapture and the end also hold.
	m.mu.Lock()
	kept = m.afterFloorLocked(stream, kept)
	resumes := m.resumes
	if kept != nil && mr.IsFinal {
		words := liveWords(kept)
		if len(words) == 0 {
			// No word timings: one word for the whole sentence (Fix #7).
			words = []transcribe.Word{{PunctuatedWord: strings.TrimSpace(kept.Channel.Alternatives[0].Transcript)}}
		}
		m.timeCapturedLocked(stream, words)
		m.buffer.AddWords(words)
	}
	m.mu.Unlock()
	if kept == nil {
		if mr.IsFinal {
			m.releasePreroll()
		}
		return nil, nil
	}
	if mr.IsFinal {
		m.detector.OnSpeech()
	}

	// Show the page the utterance so far: the words buffered, then an
	// interim's text. While paused, or once a resume has come since the words
	// were taken, nothing: they are from before the pause.
	if sessionID, capturing := m.capturingSince(resumes); m.hub != nil && capturing {
		var b strings.Builder
		speaker, startTime := -1, 0.0
		for _, w := range m.buffer.Words() {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(w.PunctuatedWord)
			if speaker == -1 && w.Speaker != nil {
				speaker, startTime = *w.Speaker, w.Start
			}
		}
		if !mr.IsFinal {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strings.TrimSpace(kept.Channel.Alternatives[0].Transcript))
			if words := liveWords(kept); speaker == -1 && len(words) > 0 {
				if words[0].Speaker != nil {
					speaker = *words[0].Speaker
				}
				startTime = words[0].Start
			}
		}
		if b.Len() > 0 {
			m.hub.BroadcastLiveTranscriptInterim(sessionID, speaker, b.String(), startTime)
		}
	}

	var err error
	if mr.IsFinal && mr.SpeechFinal {
		err = m.flushBuffer()
	}
	if _, capturing := m.capturingSince(resumes); !capturing {
		return nil, err
	}
	return kept, err
}

// capturingSince returns the current session, and whether capture is not
// paused and has not resumed since the resumes count was resumes.
func (m *Manager) capturingSince(resumes int) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSessionID, !m.capturePaused && m.resumes == resumes
}

// liveWords returns the words of Deepgram live result mr.
func liveWords(mr *api.MessageResponse) []transcribe.Word {
	words := make([]transcribe.Word, 0, len(mr.Channel.Alternatives[0].Words))
	for _, word := range mr.Channel.Alternatives[0].Words {
		words = append(words, transcribe.Word{
			Speaker:        word.Speaker,
			PunctuatedWord: word.PunctuatedWord,
			Start:          word.Start,
			End:            word.End,
		})
	}
	return words
}

// timeCapturedLocked sets when each word was captured (Word.CapturedStart,
// Word.CapturedMid) from its times into stream, the stream of the socket
// that heard it, while the session takes its result, so a reconnect before
// the word is placed (splitAtCloseLocked, raiseFloorToCloseLocked) cannot
// read it against another socket's stream. It must be called with m.mu held;
// the stream never takes m.mu.
func (m *Manager) timeCapturedLocked(stream LiveStream, words []transcribe.Word) {
	if stream == nil {
		return
	}
	for i := range words {
		word := &words[i]
		word.CapturedStart = stream.CapturedBefore(time.Duration(word.Start * float64(time.Second)))
		word.CapturedMid = stream.CapturedBefore(time.Duration((word.Start + word.End) / 2 * float64(time.Second)))
	}
}

// heardFrom returns where in the live stream the words of result mr begin:
// its first word's start, else the start of the audio it covers. Deepgram
// gives both in seconds.
func heardFrom(mr *api.MessageResponse, words []transcribe.Word) time.Duration {
	start := mr.Start
	if len(words) > 0 {
		start = words[0].Start
	}
	return time.Duration(start * float64(time.Second))
}

// holdPreroll makes the next session's recording start before the words
// Deepgram heard from at into stream, the stream of the socket that heard
// them, while no session is open or the current one is ending: the words open
// a session only at the end of their speech (flushBuffer), or once the ending
// session has ended, Deepgram can hear them late, as after a reconnect, and
// the recording must hold them from the first, as the live transcript does.
// The stream says how long ago they were captured; without one, they are
// taken as captured now. The recorder holds nothing while a recording is
// open, and keeps no audio of one that closed. While capture is paused it
// holds nothing: the resume drops that audio.
func (m *Manager) holdPreroll(stream LiveStream, at time.Duration) {
	if m.recorder == nil {
		return
	}
	m.mu.Lock()
	idle := (m.currentSessionID == "" || m.endingSession) && !m.capturePaused
	m.mu.Unlock()
	if !idle {
		return
	}
	var ago time.Duration
	if stream != nil {
		ago = stream.CapturedSince(at)
	}
	m.recorder.HoldPreroll(ago)
}

// lateResultMargin is how much more audio than the live stream can fall
// behind (LiveStream.MaxBehind) the recorder keeps from before a session:
// after an outage, Deepgram's first result for the audio the live client
// buffered comes some seconds after the reconnect, and the audio queued in
// front of the client (at most 16 s, deepgramSendChunks) is captured
// meanwhile.
const lateResultMargin = 30 * time.Second

// SetLiveStream sets the stream of audio live transcription sends Deepgram,
// whose results Message takes, and makes the recorder keep as much audio as
// the stream can fall behind, so a recording can start before words Deepgram
// hears late. A nil stream unsets it: words are then taken as captured when
// their result comes, and the recorder keeps no audio besides the preroll.
func (m *Manager) SetLiveStream(stream LiveStream) {
	m.mu.Lock()
	m.stream = stream
	m.floor = 0 // a mark on the old stream's count
	m.mu.Unlock()
	if m.recorder == nil {
		return
	}
	var lookback time.Duration
	if stream != nil {
		lookback = stream.MaxBehind() + lateResultMargin
	}
	m.recorder.SetLookback(lookback)
}

// releasePreroll ends the hold of holdPreroll once Deepgram has finalized
// audio with no words while no session is open (or the current one is
// ending) and none of its words wait to open one: words it showed only as
// interim results were taken back, and they open no session, so no more
// audio than the usual preroll leads the next.
func (m *Manager) releasePreroll() {
	if m.recorder == nil {
		return
	}
	m.mu.Lock()
	idle := (m.currentSessionID == "" || m.endingSession) && m.buffer.Len() == 0
	m.mu.Unlock()
	if idle {
		m.recorder.ReleasePreroll()
	}
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
	if m.sealedSession != "" && m.sealedSession == m.currentSessionID {
		// The session's end is writing it: words stored now would land in an
		// ended session. They stay in the buffer, and the end sorts them as
		// it sorts the words that come after it (raiseFloorToCloseLocked,
		// openWaitingSession).
		m.mu.Unlock()
		return nil
	}
	words := m.buffer.Flush()
	if m.endingSession && !m.capturePaused {
		// The current session is ending, and its recording may be closed
		// already: the words spoken after it closed wait for the next
		// session, which its end opens (endCurrentSession), and whose
		// recording holds them (holdPreroll).
		var later []transcribe.Word
		words, later = m.splitAtCloseLocked(words)
		m.buffer.PutBack(later)
	}
	segments := transcribe.GroupWordsBySpeaker(words)
	if len(segments) == 0 {
		m.mu.Unlock()
		return nil
	}
	// The words are out of the buffer but not yet stored: an end of their
	// session waits until they are before it writes the end (endSession), so
	// the session never ends, nor its summary starts, without them.
	m.flushesRunning++
	defer func() {
		m.mu.Lock()
		m.flushesRunning--
		m.endsDone.Broadcast()
		m.mu.Unlock()
	}()
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

// splitAtCloseLocked splits words, in the order Deepgram heard them, into
// those captured before the current recording closed, which its session
// keeps (its recording holds their audio), and those captured after. A word
// was captured before the close when it was captured longer ago than the
// audio the recorder has taken since the close: the live stream's count of
// all the audio captured, less the word's own place on it, read when its
// result came (timeCapturedLocked). Without a live stream, words are taken as
// captured now. While the recording is still open, every word is before the
// close. It must be called with m.mu held; it reads the recorder and the live
// stream, which never take m.mu.
func (m *Manager) splitAtCloseLocked(words []transcribe.Word) (before, after []transcribe.Word) {
	if m.recorder == nil {
		return words, nil
	}
	since, closed := m.recorder.SinceClose()
	if !closed {
		return words, nil
	}
	var captured time.Duration
	if m.stream != nil {
		captured = m.stream.Captured()
	}
	for i, word := range words {
		var ago time.Duration
		if m.stream != nil {
			ago = max(captured-word.CapturedStart, 0)
		}
		if ago <= since {
			return words[:i], words[i:]
		}
	}
	return words, nil
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
	// A session start under way settles which session is current: it may find
	// the id it claimed taken and move past it, so words never go to that id.
	for m.sessionStarts > 0 {
		m.startsDone.Wait()
	}
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
	startedAt := m.recordingStart(now.UTC())
	m.moveSessionLocked(sessionID)
	m.currentStartedAt = startedAt
	m.sessionStarts++
	m.mu.Unlock()
	defer m.finishSessionStart()

	sessionID, err := m.startSession(sessionID, startedAt)
	if err != nil {
		m.mu.Lock()
		m.moveSessionLocked("")
		m.currentStartedAt = time.Time{}
		m.mu.Unlock()
		return "", err
	}

	if m.hub != nil {
		m.hub.BroadcastSessionStarted(sessionID)
	}
	m.publishSessionStarted(sessionID)

	return sessionID, nil
}

// startSession creates session sessionID, started at startedAt, in the store,
// starts its recording, and returns the id it started. It never reuses an id
// a session has in the store (storage.ErrSessionExists) or a recording has on
// disk (os.ErrExist from the recorder): a clock set back, as a Pi's is before
// its clock is synchronized, can name a new session as an earlier one. It
// takes the next id instead (nextSessionID), up to maxSessionIDAttempts in
// a row. Any other failure starts no session.
func (m *Manager) startSession(sessionID string, startedAt time.Time) (string, error) {
	for attempt := 1; ; attempt++ {
		err := m.store.CreateSession(sessionID, startedAt)
		if err != nil && !errors.Is(err, storage.ErrSessionExists) {
			return "", fmt.Errorf("create session: %w", err)
		}
		if err == nil && m.recorder != nil {
			if err = m.recorder.StartSession(sessionID); err != nil && !errors.Is(err, os.ErrExist) {
				// It never started: no session_started went out, so no
				// session_ended is queued.
				_ = m.store.EndSession(sessionID, time.Now().UTC(), "", "")
				return "", fmt.Errorf("start audio recorder session: %w", err)
			}
			if err != nil {
				// The row just made names a recording no session has:
				// retire it, so that recording stays nobody's.
				if discardErr := m.store.DiscardSession(sessionID); discardErr != nil {
					m.logger.Warn("could not retire a session whose id a recording on disk has", "operation", "start_session", "session_id", sessionID, "error", discardErr)
				}
			}
		}
		if err == nil {
			return sessionID, nil
		}
		if attempt == maxSessionIDAttempts {
			return "", fmt.Errorf("create session: %d ids in a row are taken, up to %s: %w", attempt, sessionID, err)
		}
		m.logger.Warn("session id is taken; the session starts as the next one", "operation", "start_session", "session_id", sessionID, "error", err)
		sessionID = m.nextSessionID(sessionID, startedAt)
	}
}

// nextSessionID returns the id after taken, which a session start found
// taken, and makes it the current session's in its place.
func (m *Manager) nextSessionID(taken string, startedAt time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessionID := m.newSessionIDLocked(startedAt)
	if m.currentSessionID == taken {
		m.moveSessionLocked(sessionID)
	}
	return sessionID
}

func (m *Manager) endCurrentSession(ctx context.Context) error {
	return m.endSession(ctx, "")
}

// endSession ends the current session, or, with want set, only session want:
// if another session is current once any end under way is done, it ends
// nothing and returns ErrNoActiveSession.
func (m *Manager) endSession(ctx context.Context, want string) error {
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
	if sessionID == "" || (want != "" && sessionID != want) {
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
	// overwrite its saved audio path. The end counts as under way until it
	// returns, past the point where it clears the session and before it
	// registers its session_ended send, so a shutdown waits for it
	// (WaitForLifecyclePublishes).
	m.endingSession = true
	m.endsRunning++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.endsRunning--
		m.endsDone.Broadcast()
		m.mu.Unlock()
	}()
	ended := false
	defer func() {
		if !ended {
			m.mu.Lock()
			m.failedEnd = end
			m.endingSession = false
			m.sealedSession = ""
			m.endsDone.Broadcast()
			m.mu.Unlock()
		}
	}()
	endedAt := end.endedAt
	// Store when the recording closed before saving it: the save consumes the
	// raw recording whose last write would say so, and an end the stop's
	// deadline cuts off after it leaves the session active, for the next
	// start to end at this time (storage.SQLiteStore.RecoverStaleSessions).
	// A refused write leaves the raw recording, and the end, to a retry.
	if !end.closeStored {
		if err := m.store.RecordSessionClose(sessionID, endedAt); err != nil {
			return fmt.Errorf("record when the session closed: %w", err)
		}
		end.closeStored = true
	}
	if !end.saved && m.recorder != nil {
		path, err := m.recorder.EndSession(sessionID)
		if err != nil {
			end.saveFailures++
			if end.saveFailures < maxRecordingSaveAttempts {
				return fmt.Errorf("end audio recorder session: %w", err)
			}
			if rawPath := m.recorder.DiscardUnsaved(); rawPath != "" {
				m.logger.Error("recording could not be saved; ending the session without it and keeping its raw audio, which the app tries to save again after each Drive sync sweep", "operation", "end_session", "session_id", sessionID, "attempts", end.saveFailures, "raw_audio", rawPath, "error", err)
			} else {
				m.logger.Error("recording could not be saved and its raw audio is gone; ending the session without it", "operation", "end_session", "session_id", sessionID, "attempts", end.saveFailures, "error", err)
			}
		}
		end.audioPath, end.saved = path, true
	}

	// Seal the session: from here no flush takes words for it (flushBuffer).
	// The flushes that took words before, a pause's or those of results that
	// came while the recording was saved, store them before the session ends,
	// up to ctx's deadline, so its summary has them and none lands in it
	// after. With a publisher, the session_ended event is queued in the same
	// write as the end: a stop that refuses its send (publishLifecycle) leaves
	// it for the next start's sweep.
	m.mu.Lock()
	m.sealedSession = sessionID
	if m.flushesRunning > 0 {
		stop := context.AfterFunc(ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.endsDone.Broadcast()
		})
		defer stop()
		for m.flushesRunning > 0 {
			if err := ctx.Err(); err != nil {
				m.mu.Unlock()
				return err
			}
			m.endsDone.Wait()
		}
	}
	queue := ""
	if m.publisher != nil {
		queue = sessionEndedEvent
	}
	m.mu.Unlock()
	if err := m.store.EndSession(sessionID, endedAt, end.audioPath, queue); err != nil {
		return fmt.Errorf("end session: %w", err)
	}

	m.mu.Lock()
	m.moveSessionLocked("")
	m.currentStartedAt = time.Time{}
	m.failedEnd = sessionEnd{}
	m.endManualLocked()
	// No word captured before the recording closed reaches a later session.
	m.raiseFloorToCloseLocked()
	m.endingSession = false
	m.sealedSession = ""
	m.endsDone.Broadcast()
	ended = true
	minSegs := m.minSessionSegments
	m.mu.Unlock()
	// Words spoken after the recording closed waited in the buffer
	// (flushBuffer): they open the next session once this one has ended.
	defer m.openWaitingSession()

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
		m.Go(func(ctx context.Context) { m.runBatchRefinement(ctx, sessionID, end.audioPath) })
	}

	m.Go(func(ctx context.Context) { m.generateSummary(ctx, sessionID, startedAt) })
	return nil
}

// Go runs f in the background on the background context, as work
// StopBackground cancels and waits for, unless StopBackground has run: work
// it starts then is left to the next start. Every goroutine that writes the
// store, and that no caller waits for, starts through Go or runs inside Run.
func (m *Manager) Go(f func(ctx context.Context)) {
	if !m.beginBackground() {
		return
	}
	go func() {
		defer m.endBackground()
		f(m.background)
	}()
}

// Run runs f now, as work StopBackground waits for: f's context ends when ctx
// does, or with stopping.ErrStopped when StopBackground runs. Once
// StopBackground has run, Run runs nothing and returns stopping.ErrStopped.
// Work admitted so may itself call Run or Go, which count it again.
func (m *Manager) Run(ctx context.Context, f func(ctx context.Context) error) error {
	if !m.beginBackground() {
		return stopping.ErrStopped
	}
	defer m.endBackground()
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(m.background, func() { cancel(context.Cause(m.background)) })
	defer stop()
	return f(runCtx)
}

// beginBackground counts a piece of background work that starts, unless
// StopBackground has run, and reports whether it may start.
func (m *Manager) beginBackground() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.backgroundStopped {
		return false
	}
	m.backgroundWork.Add(1)
	m.backgroundRunning++
	return true
}

// endBackground counts a piece of background work that ended.
func (m *Manager) endBackground() {
	m.mu.Lock()
	m.backgroundRunning--
	m.mu.Unlock()
	m.backgroundWork.Done()
}

// StopBackground cancels the work admitted through Go and Run, with
// stopping.ErrStopped, and waits until it has ended or ctx is done, so that
// the store can close under none of it. With none running it returns nil at
// once, even once ctx is done. No such work starts after it.
func (m *Manager) StopBackground(ctx context.Context) error {
	m.mu.Lock()
	m.backgroundStopped = true
	running := m.backgroundRunning
	m.mu.Unlock()
	m.stopBackground(stopping.ErrStopped)
	if running == 0 {
		return nil
	}
	done := make(chan struct{})
	go func() {
		m.backgroundWork.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	// removes nothing while the refinement runs. The result is stored only at
	// the transcript generation claimed here, so a refinement of the
	// recording from before a segment deletion cannot be stored.
	generation, claimed, err := m.store.ClaimRefinement(sessionID, audioPath)
	if err != nil {
		m.batchLogger.Error("failed to mark refinement running", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
		return
	}
	if !claimed {
		m.batchLogger.Info("batch refinement not started: the session no longer has this recording (removed to free space)", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath)
		return
	}

	m.batchLogger.Info("starting batch refinement", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath)
	result, err := m.transcribeRecordings(ctx, batchTranscriber, audioPath)
	m.mu.Lock()
	warn := m.refinementWarning
	m.mu.Unlock()
	if err != nil && stopping.CutOff(ctx) {
		// The stop cut it off: it stays running, as a process that exits
		// inside it leaves it, and the next start runs it again once.
		m.batchLogger.Info("batch refinement cut off by the stop; the next start runs it again", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath)
		return
	}
	if err != nil {
		if updateErr := m.store.UpdateRefinementAtGeneration(sessionID, "", storage.RefinementFailed, "", generation); updateErr != nil {
			m.batchLogger.Error("failed to mark refinement failed", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", updateErr)
		}
		m.batchLogger.Error("batch refinement failed", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
		if warn != nil && errors.Is(err, ErrRefinementModel) {
			warn(sessionID, err.Error())
		}
		if m.hub != nil {
			m.hub.BroadcastSessionComponentStatus(sessionID, "refinement", storage.ComponentStatusError, fmt.Sprintf("Batch refinement failed for session %s", sessionID))
		}
		return
	}
	if warn != nil {
		warn(sessionID, "")
	}

	if _, err := m.store.PublishRefinementAtGeneration(sessionID, result.Transcript, result.Model, result.Words, result.Segments, generation); err != nil {
		m.batchLogger.Error("failed to publish refined words and segments", "operation", "batch_refinement", "session_id", sessionID, "audio_path", audioPath, "error", err)
		return
	}

	m.batchLogger.Info("batch refinement completed", "operation", "batch_refinement", "session_id", sessionID, "model", result.Model)
	if m.hub != nil {
		m.hub.BroadcastSessionComponentStatus(sessionID, "refinement", storage.ComponentStatusConnected, fmt.Sprintf("Batch refinement completed for session %s", sessionID))
	}
}

// RefineSession re-runs batch refinement for an already-ended session using its
// recorded audio, then regenerates the summary from the refined transcript. It
// recovers sessions that ended without a usable live transcript (e.g. when live
// transcription was unavailable at capture time). Intended to run off the
// request path. It runs as work StopBackground waits for (Run), whoever calls
// it, and returns stopping.ErrStopped once the stop has run.
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

	return m.Run(ctx, func(ctx context.Context) error {
		m.runBatchRefinement(ctx, sessionID, audioPath)
		m.generateSummary(ctx, sessionID, startedAt)
		return nil
	})
}

// SummarizeSession makes session sessionID's summary again from its
// transcript as it is, without refining it, then indexes and syncs it as a
// refinement's summary is: "Delete text" of a session whose audio is gone,
// so there is nothing to refine. It runs as work StopBackground waits for
// (Run), and returns stopping.ErrStopped once the stop has run.
func (m *Manager) SummarizeSession(ctx context.Context, sessionID string, startedAt time.Time) error {
	return m.Run(ctx, func(ctx context.Context) error {
		m.generateSummary(ctx, sessionID, startedAt)
		return nil
	})
}

func (m *Manager) generateSummary(ctx context.Context, sessionID string, startedAt time.Time) {
	m.mu.Lock()
	summarizer := m.summarizer
	syncer := m.syncer
	publisher := m.publisher
	m.mu.Unlock()

	// The transcript generation before the run starts. A failure before the
	// run has read its text is recorded only at it, so once a segment
	// deletion has changed the text, and its own run has started, this run
	// records nothing over that one. It is read before the run starts, so a
	// deletion between the two also makes such a failure refused.
	startGeneration, err := m.store.DeletionGeneration(sessionID)
	if err != nil {
		m.logger.Error("failed to read the transcript generation", "operation", "generate_summary", "session_id", sessionID, "error", err)
		return
	}

	if summarizer == nil {
		// Without a summarizer the summary completes empty, which keeps the
		// session's summary as it is. StartEmptySummary binds it to the
		// current deletion generation, so that completion passes the
		// generation fence after a segment deletion, and leaves the summary
		// version and Envoy delivery state alone, so a summary Envoy already
		// has is not sent again.
		if err := m.store.StartEmptySummary(sessionID); err != nil {
			m.logger.Error("failed to start empty summary", "operation", "generate_summary", "session_id", sessionID, "error", err)
		}
		transcript, generation, err := m.store.Canonicalize(sessionID)
		if err != nil {
			// Terminal, so a Drive replacement waiting for this session's
			// summary stops waiting; the sync still runs, with the canonical
			// transcript the session already has.
			m.failSummary(sessionID, fmt.Sprintf("failed to canonicalize transcript: %v", err), startGeneration)
			m.logger.Error("failed to canonicalize transcript", "operation", "generate_summary", "session_id", sessionID, "error", err)
			m.triggerDriveSync(syncer, sessionID)
			return
		}
		if err := m.store.UpdateSummaryAtGeneration(sessionID, "", "", storage.SummaryCompleted, "", "{}", "", generation); err != nil {
			m.logger.Error("failed to mark empty summary complete", "operation", "generate_summary", "session_id", sessionID, "error", err)
		}
		m.triggerEmbeddingIndexAtGeneration(sessionID, transcript, generation)
		m.triggerDriveSync(syncer, sessionID)
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
	transcript, generation, err := m.store.Canonicalize(sessionID)
	if err != nil {
		m.failSummary(sessionID, fmt.Sprintf("failed to canonicalize transcript: %v", err), startGeneration)
		m.logger.Error("failed to canonicalize transcript", "operation", "generate_summary", "session_id", sessionID, "error", err)
		return
	}

	// For title fallback, still need segments.
	segments, err := m.store.GetSegments(sessionID)
	if err != nil {
		m.logger.Warn("failed to get segments for title fallback", "operation", "generate_summary", "session_id", sessionID, "error", err)
	}

	var title, summaryText, preset, speakerNames, summaryModel string
	if reporter, ok := summarizer.(ReportedSummarizer); ok {
		title, summaryText, preset, speakerNames, summaryModel, err = reporter.SummarizeReported(ctx, sessionID, transcript)
	} else {
		title, summaryText, preset, speakerNames, err = summarizer.Summarize(ctx, sessionID, transcript)
	}
	// Safety net: ensure title is never empty using fallback chain.
	if strings.TrimSpace(title) == "" {
		title = summary.GenerateTitle("", transcript, segments, startedAt)
	}
	if err != nil && stopping.CutOff(ctx) {
		// The stop cut it off: it stays running, and the next start's
		// backfill makes it (storage.SQLiteStore.GetSessionsForBackfill).
		m.logger.Info("summary cut off by the stop; the next start makes it", "operation", "generate_summary", "session_id", sessionID)
		return
	}
	if err != nil {
		m.logger.Error("summarization failed", "operation", "generate_summary", "session_id", sessionID, "error", err)
		m.failSummary(sessionID, fmt.Sprintf("summarization failed: %v", err), generation)
		m.triggerEmbeddingIndexAtGeneration(sessionID, transcript, generation)
		if m.hub != nil {
			m.hub.BroadcastSessionComponentStatus(sessionID, "summary", storage.ComponentStatusError, fmt.Sprintf("Summarization failed for session %s", sessionID))
		}
		return
	}

	storeErr := m.store.UpdateSummaryAtGeneration(sessionID, title, summaryText, storage.SummaryCompleted, preset, speakerNames, summaryModel, generation)
	if errors.Is(storeErr, sql.ErrNoRows) {
		// The text changed since it was read (a segment deletion): a newer
		// run owns the summary, so this one records no failure.
		m.logger.Warn("discarded a stale summary: the session's transcript changed since it was read", "operation", "generate_summary", "session_id", sessionID)
		return
	}
	if storeErr != nil {
		m.logger.Error("failed to store summary", "operation", "generate_summary", "session_id", sessionID, "error", storeErr)
		m.failSummary(sessionID, fmt.Sprintf("failed to store summary: %v", storeErr), generation)
		if m.hub != nil {
			m.hub.BroadcastSessionComponentStatus(sessionID, "summary", storage.ComponentStatusError, fmt.Sprintf("Failed to store summary for session %s", sessionID))
		}
		return
	}

	m.broadcastSummaryStatus(sessionID, title, summaryText, storage.SummaryCompleted, preset)
	m.triggerEmbeddingIndexAtGeneration(sessionID, transcript, generation)

	m.triggerDriveSync(syncer, sessionID)

	if publisher != nil {
		m.Go(func(ctx context.Context) {
			if err := publisher.PublishSummaryReady(ctx, sessionID); err != nil {
				m.logger.Error("envoy publish failed", "operation", "publish_summary_ready", "session_id", sessionID, "error", err)
			}
		})
	}
}

func (m *Manager) triggerDriveSync(syncer SessionSyncer, sessionID string) {
	if syncer == nil {
		return
	}
	m.Go(func(ctx context.Context) {
		if err := syncer.SyncSession(ctx, sessionID); err != nil {
			m.logger.Error("gdrive sync failed", "operation", "sync_session", "session_id", sessionID, "error", err)
			if m.hub != nil {
				m.hub.BroadcastSessionComponentStatus(sessionID, "sync", storage.ComponentStatusError, fmt.Sprintf("Google Drive sync failed for session %s", sessionID))
			}
		} else if m.hub != nil {
			m.hub.BroadcastSessionComponentStatus(sessionID, "sync", storage.ComponentStatusConnected, fmt.Sprintf("Google Drive synced for session %s", sessionID))
		}
	})
}

func (m *Manager) broadcastSummaryStatus(sessionID, title, summary, status, preset string) {
	if m.hub != nil {
		m.hub.BroadcastSummaryReady(sessionID, title, summary, status, preset)
	}
}

// failSummary records that making session sessionID's summary from the text
// of transcript generation generation failed, and why (reason), and announces
// the title and summary the session keeps, its last good ones if it has
// them, with the reason. Once the text has changed since (a segment
// deletion), it records nothing: the newer run's status stays.
func (m *Manager) failSummary(sessionID, reason string, generation int) {
	kept, err := m.store.FailSummaryAtGeneration(sessionID, reason, generation)
	if errors.Is(err, sql.ErrNoRows) {
		m.logger.Warn("discarded a stale failed summary: the session's transcript changed since it was read", "operation", "generate_summary", "session_id", sessionID, "reason", reason)
		return
	}
	if err != nil {
		m.logger.Error("failed to record the failed summary", "operation", "generate_summary", "session_id", sessionID, "reason", reason, "error", err)
		return
	}
	if m.hub != nil {
		m.hub.BroadcastSummaryFailed(sessionID, kept.Title, kept.Summary, kept.SummaryPreset, kept.SummaryError)
	}
}

func (m *Manager) ActiveSession() (string, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSessionID, m.currentStartedAt
}

// endedMeeting is the meeting context of every session that is not current:
// it is done from the start.
var endedMeeting = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// MeetingContext is the meeting context of recording session id: live while
// id is the current session and the unit records, and done in the same step
// that ends or replaces the session, or that pauses capture (PauseCapture),
// before the session-ended hook. It is done at once for any other id. It
// takes the manager's lock briefly and calls nothing.
func (m *Manager) MeetingContext(id string) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" || id != m.currentSessionID || m.meeting == nil {
		return endedMeeting
	}
	return m.meeting
}

// moveSessionLocked makes id ("" for none) the current session: the last
// session's meeting context ends here, and id's begins, done at once while
// capture is paused (a start already under way when the pause came). m.mu is
// held.
func (m *Manager) moveSessionLocked(id string) {
	if m.meetingCancel != nil {
		m.meetingCancel()
	}
	m.currentSessionID = id
	m.meeting, m.meetingCancel = nil, nil
	if id != "" {
		m.meeting, m.meetingCancel = context.WithCancel(context.Background())
		if m.capturePaused {
			m.meetingCancel()
		}
	}
}

// SettledSession is the current session once its start is done, or "" while
// none is current or a start is under way: a start may find the id it claimed
// taken and move to the next (startSession), and ActiveSession names the
// candidate meanwhile.
func (m *Manager) SettledSession() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionStarts > 0 {
		return ""
	}
	return m.currentSessionID
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

// SetRefinementWarningHook sets what a refinement reports for /api/status: a
// warning when it refused to publish because the deleted text's model cannot
// be used, and an empty warning once a refinement of the session completes.
func (m *Manager) SetRefinementWarningHook(hook func(sessionID, warning string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refinementWarning = hook
}

// SetSessionEndedHook sets what runs, in its own goroutine and as work
// StopBackground waits for (Go), each time a session ends, kept or discarded:
// the meeting voice cancels the questions the ended meeting still waits on.
func (m *Manager) SetSessionEndedHook(hook func(sessionID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionEnded = hook
}

// SetSessionStartedHook sets what runs, in its own goroutine, each time a
// session starts, with the id its start settled on and the start's sequence
// number, which counts settled starts from 1 with no gaps: the meeting voice
// binds a session to the exchange that opened just before it (seq = the
// exchange's open sequence plus one) and writes the lines it held for it.
func (m *Manager) SetSessionStartedHook(hook func(sessionID string, seq uint64)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionStarted = hook
}

// startIDsKept is how many settled starts' ids SessionOfStart can still name:
// far more than can start during one exchange, which lasts minutes at most.
const startIDsKept = 64

// SessionMark is the settled session now (SettledSession) and the count of
// settled session starts, read together: an exchange that opens with no
// settled session reads them at its open, and its session is the start after
// that count (SessionOfStart), the one under way or the next to begin.
func (m *Manager) SessionMark() (sessionID string, starts uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionStarts > 0 {
		return "", m.sessionSeq
	}
	return m.currentSessionID, m.sessionSeq
}

// SessionOfStart is the id of the settled session start with sequence number
// seq, counted from 1 with no gaps, whether or not that session has ended
// since; "" while that start has not settled, or is older than startIDsKept.
func (m *Manager) SessionOfStart(seq uint64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startIDs[seq]
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

	m.Go(func(background context.Context) {
		ctx, cancel := context.WithTimeout(background, 45*time.Second)
		defer cancel()
		if err := indexer.IndexSessionAtGeneration(ctx, sessionID, transcript, generation); err != nil {
			m.logger.Error("embedding index failed", "operation", "generate_summary", "session_id", sessionID, "error", err)
		}
	})
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
	m.moveSessionLocked(sessionID)
	startedAt := m.recordingStart(now)
	m.currentStartedAt = startedAt
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

	sessionID, err := m.startSession(sessionID, startedAt)
	if err != nil {
		m.mu.Lock()
		m.moveSessionLocked("")
		m.currentStartedAt = time.Time{}
		m.endManualLocked()
		m.mu.Unlock()
		return "", err
	}

	// Set title hint if provided.
	if titleHint != "" {
		if err := m.store.UpdateTitle(sessionID, titleHint); err != nil {
			m.logger.Warn("failed to set title hint on manual session", "operation", "manual_start", "session_id", sessionID, "error", err)
		}
	}

	if m.hub != nil {
		m.hub.BroadcastSessionStarted(sessionID)
	}
	m.publishSessionStarted(sessionID)

	// The title hint is the session's title, which names its topic: the log
	// keeps the session's id, never its words.
	m.logger.Info("manual session started", "operation", "manual_start", "session_id", sessionID, "title_hint_set", titleHint != "")
	return sessionID, nil
}

// recordingStart returns when the recording of a session starting at now
// begins: now minus the audio the recorder would open it with
// (Recorder.Pending). A session starts when its recording does, so a refined
// segment's time, the session's start plus the segment's offset into the
// recording, is the time it was spoken.
func (m *Manager) recordingStart(now time.Time) time.Time {
	if m.recorder == nil {
		return now
	}
	return now.Add(-m.recorder.Pending())
}

// openWaitingSession opens the next session with the words spoken while the
// last one was ending, which flushBuffer left in the buffer, and starts its
// silence timer: their utterance end came during the end, so the timer it
// started may have run out before this session existed.
func (m *Manager) openWaitingSession() {
	if m.buffer.Len() == 0 {
		return
	}
	if err := m.flushBuffer(); err != nil {
		m.logger.Warn("could not save the words spoken while the last session ended", "operation", "end_session", "error", err)
	}
	if id, _ := m.ActiveSession(); id != "" {
		m.detector.OnUtteranceEnd()
	}
}

// StopSession ends a specific session by ID. If the given sessionID does not
// match the currently active session, returns ErrNoActiveSession. Like
// ForceEndSession, it first saves the words Deepgram finalized that wait for
// the end of their speech to the session: their audio is in its recording,
// and the end drops them from the buffer.
func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	if m.currentSessionID != sessionID {
		m.mu.Unlock()
		return ErrNoActiveSession
	}
	m.mu.Unlock()

	if err := m.flushBuffer(); err != nil && !errors.Is(err, ErrNoActiveSession) {
		m.logger.Warn("flush buffer failed", "operation", "stop_session", "session_id", sessionID, "error", err)
	}
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

// PauseCapture stops new sessions from starting until ResumeCapture, and ends
// the current session's meeting context: the meeting ends the moment the unit
// leaves recording, whatever the session's end still has to save. It only
// sets a flag and cancels a context, and waits for nothing, so the capture
// gate can close at once; a session start already under way still finishes,
// and WaitForSessionStarts waits for it. It does not end the active session;
// the capture pause does that once Deepgram's last words are in.
func (m *Manager) PauseCapture() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capturePaused = true
	if m.meetingCancel != nil {
		m.meetingCancel()
	}
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

// finishSessionStart ends a session start and, once the start has settled on
// its id, runs the session-started hook with it, in its own goroutine: a
// failed start left currentSessionID empty, and runs no hook. The hook runs
// after the start settled, so a reader (the meeting voice) sees the settled
// session, not a candidate a taken id moved past.
func (m *Manager) finishSessionStart() {
	m.mu.Lock()
	m.sessionStarts--
	m.startsDone.Broadcast()
	id, hook := m.currentSessionID, m.sessionStarted
	var seq uint64
	if id != "" {
		m.sessionSeq++
		seq = m.sessionSeq
		if m.startIDs == nil {
			m.startIDs = map[uint64]string{}
		}
		m.startIDs[seq] = id
		// The start startIDsKept back, in the counter's own wrapping
		// arithmetic: before that many starts it names none, and across the
		// counter's wrap it names the start the record must drop.
		delete(m.startIDs, seq-startIDsKept)
	}
	m.mu.Unlock()
	if hook != nil && id != "" {
		go hook(id, seq)
	}
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
// It raises the floor to where the pause falls in the live stream's audio, so
// the words Deepgram finalizes after the resume for audio from before the
// pause are dropped too (afterFloorLocked). It must run before audio flows
// again.
func (m *Manager) ResumeCapture() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buffer.Flush()
	if m.stream != nil {
		m.floor = max(m.floor, m.stream.Captured())
	}
	m.resumes++
	m.capturePaused = false
}

// raiseFloorToCloseLocked raises the floor to where the recording that
// closed last closed, on the live stream's count, once its session's end is
// done, and drops the words captured before it from the buffer: their audio
// is in that recording, so no later session may hold them, and Deepgram can
// deliver them, or keep them buffered, past the end. The close is the
// stream's count less the audio the recorder has taken since, read first; it
// is never early, as live transcription gets each chunk before the recorder.
// A buffered word is placed by its middle, read when its result came
// (timeCapturedLocked). It must be called with m.mu held; the recorder and
// the live stream never take m.mu.
func (m *Manager) raiseFloorToCloseLocked() {
	if m.stream == nil || m.recorder == nil {
		return
	}
	since, closed := m.recorder.SinceClose()
	if !closed {
		return
	}
	m.floor = max(m.floor, m.stream.Captured()-since)
	words := m.buffer.Flush()
	kept := words[:0]
	for _, word := range words {
		if word.CapturedMid >= m.floor {
			kept = append(kept, word)
		}
	}
	m.buffer.PutBack(kept)
}

// beforeFloorLocked reports whether the point at seconds into stream was
// captured before the floor. It must be called with m.mu held.
func (m *Manager) beforeFloorLocked(stream LiveStream, at float64) bool {
	if stream == nil || m.floor <= 0 {
		return false
	}
	return stream.CapturedBefore(time.Duration(at*float64(time.Second))) < m.floor
}

// afterFloorLocked returns result mr, timed in stream, without its words
// captured before the floor: mr itself when it has none, nil when it has
// nothing else, and else a copy that keeps only the later words, with its
// Transcript rebuilt from them (MessageKeptOn). The floor is the latest
// capture resume, or the close of the last recording whose session has
// ended, whichever is later. Deepgram lags the microphone after an outage, so
// a pause's Finalize finalizes only what it has reached, and its results for
// the last words before a pause or a close can come after it, even in one
// result with the first words after it. A word is placed by its middle. A
// result without word times cannot be split, so it is dropped whole when its
// audio starts before the floor; that can drop a few words spoken after it.
// With no stream, or no floor yet, it keeps every word. mr must have an
// alternative. It must be called with m.mu held; the stream never takes m.mu.
func (m *Manager) afterFloorLocked(stream LiveStream, mr *api.MessageResponse) *api.MessageResponse {
	words := mr.Channel.Alternatives[0].Words
	if len(words) == 0 {
		if m.beforeFloorLocked(stream, mr.Start) {
			return nil
		}
		return mr
	}
	// Deepgram gives a result's words in the order they were spoken.
	dropped := 0
	for dropped < len(words) && m.beforeFloorLocked(stream, (words[dropped].Start+words[dropped].End)/2) {
		dropped++
	}
	if dropped == 0 {
		return mr
	}
	alternative := mr.Channel.Alternatives[0]
	alternative.Words = words[dropped:]
	text := make([]string, len(alternative.Words))
	for i, word := range alternative.Words {
		text[i] = word.PunctuatedWord
	}
	alternative.Transcript = strings.TrimSpace(strings.Join(text, " "))
	if alternative.Transcript == "" {
		return nil
	}
	kept := *mr
	kept.Channel.Alternatives = append([]api.Alternative{alternative}, mr.Channel.Alternatives[1:]...)
	return &kept
}
