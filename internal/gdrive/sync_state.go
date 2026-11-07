package gdrive

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/googleapi"

	gwstatus "github.com/sjawhar/ghost-wispr/internal/status"
)

type SyncState string

const (
	SyncStatePending        SyncState = gwstatus.SyncStatePending
	SyncStateSyncing        SyncState = gwstatus.SyncStateSyncing
	SyncStateSynced         SyncState = gwstatus.SyncStateSynced
	SyncStateFailed         SyncState = gwstatus.SyncStateFailed
	SyncStateRetryScheduled SyncState = gwstatus.SyncStateRetryScheduled
	SyncStateRemoteDeleted  SyncState = gwstatus.SyncStateRemoteDeleted
)

const MaxSyncRetries = 5

var retryBackoffByCount = [...]time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	6 * time.Hour,
}

type RetryPlan struct {
	NextState     SyncState
	RetryCount    int
	Delay         time.Duration
	NextAttemptAt time.Time
	Exhausted     bool
}

func ValidateSyncStateTransition(from, to SyncState) error {
	if from == to {
		return nil
	}

	allowed := map[SyncState]map[SyncState]struct{}{
		SyncStatePending: {
			SyncStateSyncing: {},
		},
		// A sync ends in one write, under its claim: synced, back to pending
		// (the session changed during the upload), a retry, a failure once
		// the retries ran out, or its Drive folder gone.
		SyncStateSyncing: {
			SyncStateSynced:         {},
			SyncStatePending:        {},
			SyncStateRetryScheduled: {},
			SyncStateFailed:         {},
			SyncStateRemoteDeleted:  {},
		},
		// A manual retry (retry-sync) starts again once the automatic
		// retries ran out; the sweep syncs only PENDING and RETRY_SCHEDULED.
		SyncStateFailed: {
			SyncStateRetryScheduled: {},
			SyncStateSyncing:        {},
		},
		SyncStateRetryScheduled: {
			SyncStateSyncing: {},
		},
		// A retry asks Drive again: the folder may be back from the trash.
		// The sync checks it before it writes anything, and ends
		// REMOTE_DELETED again while it is not.
		SyncStateRemoteDeleted: {
			SyncStateSyncing: {},
		},
	}

	if _, ok := allowed[from][to]; ok {
		return nil
	}

	return fmt.Errorf("invalid sync state transition: %s -> %s", from, to)
}

func BuildRetryPlan(now time.Time, currentRetryCount int) RetryPlan {
	if currentRetryCount >= MaxSyncRetries {
		return RetryPlan{
			NextState:  SyncStateFailed,
			RetryCount: currentRetryCount,
			Exhausted:  true,
		}
	}

	nextRetryCount := currentRetryCount + 1
	delay := RetryDelayForCount(nextRetryCount)

	return RetryPlan{
		NextState:     SyncStateRetryScheduled,
		RetryCount:    nextRetryCount,
		Delay:         delay,
		NextAttemptAt: now.UTC().Add(delay),
		Exhausted:     false,
	}
}

func RetryDelayForCount(retryCount int) time.Duration {
	if retryCount <= 0 {
		return retryBackoffByCount[0]
	}
	if retryCount > len(retryBackoffByCount) {
		return retryBackoffByCount[len(retryBackoffByCount)-1]
	}
	return retryBackoffByCount[retryCount-1]
}

func IsRetryReady(now time.Time, retryCount int, lastSyncAttempt *time.Time) bool {
	if retryCount <= 0 {
		return true
	}
	if lastSyncAttempt == nil {
		return true
	}
	readyAt := lastSyncAttempt.UTC().Add(RetryDelayForCount(retryCount))
	return !now.UTC().Before(readyAt)
}

// IsRemoteDeletedError reports whether err is, or wraps, Drive answering that
// a file is not found (404), or ErrFolderTrashed: a session folder in the
// trash is treated as one deleted.
func IsRemoteDeletedError(err error) bool {
	if errors.Is(err, ErrFolderTrashed) {
		return true
	}
	var gErr *googleapi.Error
	if !errors.As(err, &gErr) {
		return false
	}

	if gErr.Code == http.StatusNotFound {
		return true
	}

	for _, item := range gErr.Errors {
		if strings.EqualFold(item.Reason, "notFound") {
			return true
		}
	}

	return false
}
