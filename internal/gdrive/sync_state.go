package gdrive

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
)

type SyncState string

const (
	SyncStatePending        SyncState = "PENDING"
	SyncStateSyncing        SyncState = "SYNCING"
	SyncStateSynced         SyncState = "SYNCED"
	SyncStateFailed         SyncState = "FAILED"
	SyncStateRetryScheduled SyncState = "RETRY_SCHEDULED"
	SyncStateRemoteDeleted  SyncState = "REMOTE_DELETED"
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
		SyncStateSyncing: {
			SyncStateSynced:        {},
			SyncStateFailed:        {},
			SyncStateRemoteDeleted: {},
		},
		SyncStateFailed: {
			SyncStateRetryScheduled: {},
		},
		SyncStateRetryScheduled: {
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

func IsRemoteDeletedError(err error) bool {
	if err == nil {
		return false
	}

	gErr, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}

	if gErr.Code == 404 {
		return true
	}

	for _, item := range gErr.Errors {
		if strings.EqualFold(item.Reason, "notFound") {
			return true
		}
	}

	return false
}
