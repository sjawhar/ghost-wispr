package session

import (
	"context"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Store interface {
	CreateSession(id string, startedAt time.Time) error
	// EndSession ends the session, and queues the lifecycle event queueEvent,
	// unless "", in the same write (storage.SQLiteStore.EndSession).
	EndSession(id string, endedAt time.Time, audioPath, queueEvent string) error
	// RecordSessionClose records when an end of the active session closed its
	// recording, so a start after an end that did not finish ends it then.
	RecordSessionClose(id string, closedAt time.Time) error
	DiscardSession(id string) error
	AppendSegment(sessionID string, seg transcribe.Segment) error
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	CountSegments(sessionID string) (int, error)
	// UpdateSummaryAtGeneration stores the summary and the model its provider
	// reported only while generation is still the session's transcript
	// generation.
	UpdateSummaryAtGeneration(sessionID, title, summary, status, preset, speakerNames, model string, generation int) error
	// StartSummary marks the summary running; the session keeps its title
	// and its last good summary until a new one is stored.
	StartSummary(sessionID string) error
	// StartEmptySummary marks the summary running, with no summarizer to make
	// one: the summary version and Envoy delivery state stay as they are.
	StartEmptySummary(sessionID string) error
	// FailSummaryAtGeneration records that the summary failed, and why,
	// keeping the session's title and last good summary, and returns the
	// session, only while generation is still the session's transcript
	// generation.
	FailSummaryAtGeneration(sessionID, errMsg string, generation int) (storage.Session, error)
	// DeletionGeneration returns the session's transcript generation.
	DeletionGeneration(sessionID string) (int, error)
	// UpdateRefinementAtGeneration records a failed refinement while its
	// claimed transcript generation is still current.
	UpdateRefinementAtGeneration(sessionID, transcript, status, model string, generation int) error
	// PublishRefinementAtGeneration atomically publishes the completed
	// refinement's words, rebuilt segments and both transcript views.
	PublishRefinementAtGeneration(sessionID, transcript, model string, words []storage.RefinedWord, segments []storage.RefinedSegment, generation int) (int, error)
	// ClaimRefinement marks the refinement running while the session still
	// names audioPath, the recording it will read, reports whether it did,
	// and returns the transcript generation it claimed at.
	ClaimRefinement(sessionID, audioPath string) (generation int, claimed bool, err error)
	GetRefinement(sessionID string) (transcript, status string, err error)
	// TextDeletionSpans returns the spans "Delete text" recorded in
	// recording, whose words a refinement of it leaves out.
	TextDeletionSpans(recording string) ([]storage.TextDeletionSpan, error)
	UpdateTitle(sessionID, title string) error
	Canonicalize(sessionID string) (transcript string, generation int, err error)
}

type Recorder interface {
	StartSession(sessionID string) error
	EndSession(sessionID string) (string, error)
	DiscardUnsaved() string
	// HoldPreroll makes the next session's recording start a preroll before
	// the audio captured ago, keeping that audio and all the audio from now
	// on until that session starts or ReleasePreroll; it does nothing while a
	// recording is open.
	HoldPreroll(ago time.Duration)
	// ReleasePreroll ends a hold whose words opened no session: only the
	// usual preroll then leads the next session.
	ReleasePreroll()
	// SetLookback sets how far before now HoldPreroll can reach, besides
	// the preroll.
	SetLookback(lookback time.Duration)
	// Pending returns how much audio the next session's recording would
	// open with if it started now; 0 while a recording is open.
	Pending() time.Duration
	// SinceClose returns how much audio has been taken since the last
	// recording closed, and true; false while a recording is open.
	SinceClose() (time.Duration, bool)
}

// LiveStream is the stream of audio live transcription sends Deepgram, which
// times the words in its results from the stream's start.
type LiveStream interface {
	// CapturedSince returns how much audio has been captured after the
	// point at into the stream.
	CapturedSince(at time.Duration) time.Duration
	// Captured returns how much audio has been captured in all, on a count
	// no reconnect moves.
	Captured() time.Duration
	// CapturedBefore returns how much audio had been captured before the
	// point at into the stream, on the count Captured gives.
	CapturedBefore(at time.Duration) time.Duration
	// MaxBehind returns how far the stream can fall behind the microphone.
	MaxBehind() time.Duration
}

type Summarizer interface {
	Summarize(ctx context.Context, sessionID, transcript string) (title, summary, preset, speakerNames string, err error)
}

// ReportedSummarizer returns a summary and its provider-reported model in one
// result, avoiding a shared result cache across concurrent sessions.
type ReportedSummarizer interface {
	SummarizeReported(ctx context.Context, sessionID, transcript string) (title, summary, preset, speakerNames, model string, err error)
}

type SessionSyncer interface {
	SyncSession(ctx context.Context, sessionID string) error
}

type EventPublisher interface {
	PublishSessionStarted(ctx context.Context, sessionID string) error
	PublishSessionEnded(ctx context.Context, sessionID string) error
	PublishSummaryReady(ctx context.Context, sessionID string) error
}

type EmbeddingIndexer interface {
	IndexSessionAtGeneration(ctx context.Context, sessionID, transcript string, generation int) error
}

type EventBroadcaster interface {
	BroadcastLiveTranscript(sessionID string, seg transcribe.Segment)
	BroadcastSessionStarted(sessionID string)
	BroadcastSessionEnded(sessionID string, duration time.Duration)
	BroadcastSummaryReady(sessionID, title, summary, status, preset string)
	// BroadcastSummaryFailed announces a failed summary, why it failed
	// (reason), and the title, summary and preset the session keeps.
	BroadcastSummaryFailed(sessionID, title, summary, preset, reason string)
	BroadcastLiveTranscriptInterim(sessionID string, speaker int, text string, startTime float64)
	// BroadcastSessionComponentStatus announces a component's status about
	// session sessionID (its refinement, summary or sync), naming the session
	// in the event's session_id, so deleting the session deletes the event.
	BroadcastSessionComponentStatus(sessionID, component, status, message string)
}

type LifecycleManager interface {
	Message(mr *api.MessageResponse) error
	UtteranceEnd(ur *api.UtteranceEndResponse) error
	ForceEndSession(ctx context.Context) error
}
