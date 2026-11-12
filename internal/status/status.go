package status

const (
	SessionActive    = "active"
	SessionEnded     = "ended"
	SessionDiscarded = "discarded"
	SessionMerged    = "merged"

	SummaryPending   = "pending"
	SummaryRunning   = "running"
	SummaryCompleted = "completed"
	SummaryFailed    = "failed"

	SyncPending = "pending"
	SyncSynced  = "synced"
	SyncFailed  = "failed"

	EnvoyPublishPending   = "pending"
	EnvoyPublishPublished = "published"
	EnvoyPublishFailed    = "failed"
	// EnvoyPublishSkipped is a completed summary with no text: nothing is
	// sent to Envoy, and the publish sweep does not select it again.
	EnvoyPublishSkipped = "skipped"

	SyncStatePending        = "PENDING"
	SyncStateSyncing        = "SYNCING"
	SyncStateSynced         = "SYNCED"
	SyncStateFailed         = "FAILED"
	SyncStateRetryScheduled = "RETRY_SCHEDULED"
	SyncStateRemoteDeleted  = "REMOTE_DELETED"

	RefinementPending   = "pending"
	RefinementRunning   = "running"
	RefinementCompleted = "completed"
	RefinementFailed    = "failed"
	// RefinementNoAudio: the session ended without a saved recording, so it
	// can never be refined. Terminal.
	RefinementNoAudio = "no_audio"

	TranscriptSourceStreaming = "streaming"
	TranscriptSourceRefined   = "refined"

	ComponentStatusConnected    = "connected"
	ComponentStatusDisconnected = "disconnected"
	ComponentStatusReconnecting = "reconnecting"
	ComponentStatusError        = "error"
	ComponentStatusOK           = "ok"
	ComponentStatusOpen         = "open"
	ComponentStatusClosed       = "closed"
	ComponentStatusPaused       = "paused"
	ComponentStatusResuming     = "resuming"
	ComponentStatusStalled      = "stalled"
)
