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
	AppendSegment(sessionID string, seg transcribe.Segment) error
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	UpdateSummary(sessionID, summary, status string) error
}

type Recorder interface {
	StartSession(sessionID string) error
	EndSession() (string, error)
}

type Summarizer interface {
	Summarize(ctx context.Context, sessionID, transcript string) (string, error)
}

type EventBroadcaster interface {
	BroadcastLiveTranscript(seg transcribe.Segment)
	BroadcastSessionStarted(sessionID string)
	BroadcastSessionEnded(sessionID string, duration time.Duration)
	BroadcastSummaryReady(sessionID, summary, status string)
}

type LifecycleManager interface {
	Message(mr *api.MessageResponse) error
	UtteranceEnd(ur *api.UtteranceEndResponse) error
	ForceEndSession(ctx context.Context) error
}
