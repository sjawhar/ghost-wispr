package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
)

// refusingSyncer is a Drive syncer whose CanSync answers err.
type refusingSyncer struct {
	countingSyncer
	err error
}

func (r *refusingSyncer) CanSync(context.Context, string) error { return r.err }

// Review of #39, round 3, note 9: a manual retry of a session whose Drive
// folder was deleted, when Drive does not answer whether the folder is back,
// is refused as Drive being unavailable (the API's 503), with why, not as a
// conflict with the session (409): the retry may work once Drive answers. A
// folder still in the trash is a conflict; a retry that can start starts.
func TestRetrySyncSaysDriveIsUnavailableWhenDriveDoesNotAnswer(t *testing.T) {
	const id = "20261106090000"
	if err := retrySync(context.Background(), nil, id); !errors.Is(err, server.ErrSyncNotConfigured) {
		t.Errorf("sync off: %v, want %v", err, server.ErrSyncNotConfigured)
	}

	noAnswer := fmt.Errorf("cannot sync session %s now: its Drive folder was deleted, and Drive did not say whether it is back: %w: googleapi: Error 503", id, gdrive.ErrDriveDidNotAnswer)
	err := retrySync(context.Background(), &refusingSyncer{err: noAnswer}, id)
	if !errors.Is(err, server.ErrDriveUnavailable) || !strings.Contains(err.Error(), "did not say whether it is back") {
		t.Errorf("Drive not answering: %v, want %v with why", err, server.ErrDriveUnavailable)
	}

	trashed := fmt.Errorf("cannot sync session %s now: its Drive folder folder-1 is still in the trash or gone; restore it on Drive, then retry", id)
	if err := retrySync(context.Background(), &refusingSyncer{err: trashed}, id); err == nil || errors.Is(err, server.ErrDriveUnavailable) {
		t.Errorf("folder still in the trash: %v, want the conflict as it is", err)
	}

	starts := &countingSyncer{}
	if err := retrySync(context.Background(), starts, id); err != nil {
		t.Fatalf("retry that can start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for starts.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.calls.Load() != 1 {
		t.Errorf("retry that can start: %d syncs started, want 1", starts.calls.Load())
	}
}
