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

	TranscriptSourceStreaming = "streaming"
	TranscriptSourceRefined   = "refined"

	ComponentStatusConnected    = "connected"
	ComponentStatusDisconnected = "disconnected"
	ComponentStatusReconnecting = "reconnecting"
	ComponentStatusError        = "error"
	ComponentStatusOK           = "ok"
	ComponentStatusOpen         = "open"
	ComponentStatusClosed       = "closed"
)
