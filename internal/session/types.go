package session

import (
	"context"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Store interface {
	CreateSession(id string, startedAt time.Time) error
	EndSession(id string, endedAt time.Time, audioPath string) error
	DiscardSession(id string) error
	AppendSegment(sessionID string, seg transcribe.Segment) error
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	CountSegments(sessionID string) (int, error)
	UpdateSummary(sessionID, title, summary, status, preset string) error
	UpdateRefinement(sessionID, transcript, status string) error
	GetRefinement(sessionID string) (transcript, status string, err error)
	UpdateTitle(sessionID, title string) error
	Canonicalize(sessionID string) error
	GetCanonicalTranscript(sessionID string) (transcript, source string, err error)
}

type Recorder interface {
	StartSession(sessionID string) error
	EndSession() (string, error)
}

type Summarizer interface {
	Summarize(ctx context.Context, sessionID, transcript string) (title, summary, preset string, err error)
}

type SessionSyncer interface {
	SyncSession(ctx context.Context, sessionID string) error
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
