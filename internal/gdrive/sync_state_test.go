package gdrive

import (
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func TestSyncStateMachineTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    SyncState
		to      SyncState
		wantErr bool
	}{
		{name: "pending to syncing", from: SyncStatePending, to: SyncStateSyncing},
		{name: "syncing to synced", from: SyncStateSyncing, to: SyncStateSynced},
		{name: "syncing to failed", from: SyncStateSyncing, to: SyncStateFailed},
		{name: "failed to retry scheduled", from: SyncStateFailed, to: SyncStateRetryScheduled},
		{name: "retry scheduled to syncing", from: SyncStateRetryScheduled, to: SyncStateSyncing},
		{name: "syncing to remote deleted", from: SyncStateSyncing, to: SyncStateRemoteDeleted},
		{name: "invalid pending to synced", from: SyncStatePending, to: SyncStateSynced, wantErr: true},
		{name: "invalid failed to synced", from: SyncStateFailed, to: SyncStateSynced, wantErr: true},
		{name: "invalid synced to syncing", from: SyncStateSynced, to: SyncStateSyncing, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSyncStateTransition(tc.from, tc.to)
			if tc.wantErr && err == nil {
				t.Fatalf("expected transition %s -> %s to fail", tc.from, tc.to)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected transition %s -> %s to pass, got %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestRetryPlanBackoffSchedule(t *testing.T) {
	now := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name              string
		currentRetryCount int
		wantRetryCount    int
		wantDelay         time.Duration
	}{
		{name: "first retry after five minutes", currentRetryCount: 0, wantRetryCount: 1, wantDelay: 5 * time.Minute},
		{name: "second retry after fifteen minutes", currentRetryCount: 1, wantRetryCount: 2, wantDelay: 15 * time.Minute},
		{name: "third retry after one hour", currentRetryCount: 2, wantRetryCount: 3, wantDelay: 1 * time.Hour},
		{name: "fourth retry after six hours", currentRetryCount: 3, wantRetryCount: 4, wantDelay: 6 * time.Hour},
		{name: "fifth retry stays capped at six hours", currentRetryCount: 4, wantRetryCount: 5, wantDelay: 6 * time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := BuildRetryPlan(now, tc.currentRetryCount)
			if plan.Exhausted {
				t.Fatalf("unexpected exhausted retry plan for count=%d", tc.currentRetryCount)
			}
			if plan.NextState != SyncStateRetryScheduled {
				t.Fatalf("expected next state %s, got %s", SyncStateRetryScheduled, plan.NextState)
			}
			if plan.RetryCount != tc.wantRetryCount {
				t.Fatalf("expected retry count %d, got %d", tc.wantRetryCount, plan.RetryCount)
			}
			if plan.Delay != tc.wantDelay {
				t.Fatalf("expected delay %s, got %s", tc.wantDelay, plan.Delay)
			}
			if !plan.NextAttemptAt.Equal(now.Add(tc.wantDelay)) {
				t.Fatalf("expected next attempt at %s, got %s", now.Add(tc.wantDelay), plan.NextAttemptAt)
			}
		})
	}
}

func TestRetryPlanExhaustionAtMaxRetries(t *testing.T) {
	now := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)

	plan := BuildRetryPlan(now, 5)
	if !plan.Exhausted {
		t.Fatal("expected retry plan to be exhausted at retry_count=5")
	}
	if plan.NextState != SyncStateFailed {
		t.Fatalf("expected exhausted plan to stay %s, got %s", SyncStateFailed, plan.NextState)
	}
	if !plan.NextAttemptAt.IsZero() {
		t.Fatalf("expected no next attempt when exhausted, got %s", plan.NextAttemptAt)
	}
}

func TestClassifyRemoteDeletedError(t *testing.T) {
	err := &googleapi.Error{
		Code: 404,
		Errors: []googleapi.ErrorItem{
			{Reason: "notFound", Message: "File not found"},
		},
	}

	if !IsRemoteDeletedError(err) {
		t.Fatal("expected 404 notFound to be treated as remote deleted")
	}
}

func TestIsRetryReady(t *testing.T) {
	now := time.Date(2026, 3, 23, 11, 0, 0, 0, time.UTC)

	attempt := now.Add(-4 * time.Minute)
	if IsRetryReady(now, 1, &attempt) {
		t.Fatal("expected first retry to wait full 5 minutes")
	}

	attempt = now.Add(-5 * time.Minute)
	if !IsRetryReady(now, 1, &attempt) {
		t.Fatal("expected first retry to be ready at 5 minutes")
	}

	attempt = now.Add(-59 * time.Minute)
	if IsRetryReady(now, 3, &attempt) {
		t.Fatal("expected third retry to wait full 1 hour")
	}

	attempt = now.Add(-1 * time.Hour)
	if !IsRetryReady(now, 3, &attempt) {
		t.Fatal("expected third retry to be ready at 1 hour")
	}
}
