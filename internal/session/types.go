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
	EndSession(id string, endedAt time.Time, audioPath string) error
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
	// UpdateRefinementAtGeneration stores the refinement and the model its
	// provider reported only while generation, the one ClaimRefinement
	// returned, is still the session's transcript generation.
	UpdateRefinementAtGeneration(sessionID, transcript, status, model string, generation int) error
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
	BroadcastLiveTranscriptInterim(sessionID string, speaker int, text string, startTime float64)
	BroadcastComponentStatus(component, status, message string)
}

type LifecycleManager interface {
	Message(mr *api.MessageResponse) error
	UtteranceEnd(ur *api.UtteranceEndResponse) error
	ForceEndSession(ctx context.Context) error
}
