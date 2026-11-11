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
	UpdateSummaryWithModel(sessionID, title, summary, status, preset, speakerNames, model string) error
	// StartSummary marks the summary running; the session keeps its title
	// and its last good summary until a new one is stored.
	StartSummary(sessionID string) error
	// FailSummary records that the summary failed, and why, keeping the
	// session's title and last good summary, and returns the session.
	FailSummary(sessionID, errMsg string) (storage.Session, error)
	UpdateRefinement(sessionID, transcript, status string) error
	UpdateRefinementWithModel(sessionID, transcript, status, model string) error
	// ClaimRefinement marks the refinement running while the session still
	// names audioPath, the recording it will read, and reports whether it did.
	ClaimRefinement(sessionID, audioPath string) (bool, error)
	GetRefinement(sessionID string) (transcript, status string, err error)
	UpdateTitle(sessionID, title string) error
	Canonicalize(sessionID string) error
	GetCanonicalTranscript(sessionID string) (transcript, source string, err error)
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
	IndexSession(ctx context.Context, sessionID, transcript string) error
}

type EventBroadcaster interface {
	BroadcastLiveTranscript(seg transcribe.Segment)
	BroadcastSessionStarted(sessionID string)
	BroadcastSessionEnded(sessionID string, duration time.Duration)
	BroadcastSummaryReady(sessionID, title, summary, status, preset string)
	BroadcastLiveTranscriptInterim(speaker int, text string, startTime float64)
	BroadcastComponentStatus(component, status, message string)
}

type LifecycleManager interface {
	Message(mr *api.MessageResponse) error
	UtteranceEnd(ur *api.UtteranceEndResponse) error
	ForceEndSession(ctx context.Context) error
}
