package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestDriveReplacementWarningsComeFromTheStore fails if the GET /api/status
// warning for a deletion still owed to Google Drive is not read from the
// store: it must name the session once its attempts keep failing, or once
// it has waited past the bound without failing; be back after a restart
// (a new registry over the same database); stay off for a replacement owed
// briefly; and come off once nothing is owed.
func TestDriveReplacementWarningsComeFromTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	store, err := storage.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	for _, id := range []string{"20261106100000", "20261106110000", "20261106120000"} {
		if err := store.CreateSession(id, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, now.Add(-2*time.Hour+time.Minute), ""); err != nil {
			t.Fatal(err)
		}
	}
	owe := func(id string, since time.Time, failures int) {
		t.Helper()
		if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 1, drive_replace_since = ?, drive_replace_failures = ?, drive_replace_error = 'Drive unavailable' WHERE id = ?`,
			since.Format(time.RFC3339Nano), failures, id); err != nil {
			t.Fatal(err)
		}
	}
	owe("20261106100000", now.Add(-5*time.Minute), driveReplacementWarnFailures) // keeps failing
	owe("20261106110000", now.Add(-2*time.Hour), 0)                               // only waits
	owe("20261106120000", now.Add(-5*time.Minute), 1)                             // owed briefly

	for _, registry := range []*warningsRegistry{newWarningsRegistry(), newWarningsRegistry()} { // the second is after a restart
		if err := refreshDriveReplacementWarnings(store, registry, now); err != nil {
			t.Fatal(err)
		}
		warnings := strings.Join(registry.List(), "\n")
		if !strings.Contains(warnings, "20261106100000") || !strings.Contains(warnings, "Drive unavailable") {
			t.Errorf("warnings %q do not report the replacement that keeps failing", warnings)
		}
		if !strings.Contains(warnings, "20261106110000") {
			t.Errorf("warnings %q do not report the replacement that has waited two hours", warnings)
		}
		if strings.Contains(warnings, "20261106120000") {
			t.Errorf("warnings %q report a replacement owed for five minutes after one failure", warnings)
		}
	}

	registry := newWarningsRegistry()
	registry.Set("microphone", "Microphone unavailable")
	if err := refreshDriveReplacementWarnings(store, registry, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET drive_replace_pending = 0`); err != nil {
		t.Fatal(err)
	}
	if err := refreshDriveReplacementWarnings(store, registry, now); err != nil {
		t.Fatal(err)
	}
	if got := registry.List(); len(got) != 1 || got[0] != "Microphone unavailable" {
		t.Errorf("warnings once nothing is owed = %q, want only the unrelated one", got)
	}
}

// owingSweepStore owes one replacement that keeps failing.
type owingSweepStore struct{ sweepStoreStub }

func (owingSweepStore) OwedDriveReplacements() ([]storage.OwedDriveReplacement, error) {
	return []storage.OwedDriveReplacement{{SessionID: "20261106100000", Since: time.Now().UTC(), Failures: driveReplacementWarnFailures, LastError: "Drive unavailable"}}, nil
}

// TestTheSyncSweepWarnsAsItStarts fails if the app, starting again, waits for
// a sweep pass (five minutes) or for Drive sync to be on before it reports a
// deletion still owed to Drive.
func TestTheSyncSweepWarnsAsItStarts(t *testing.T) {
	registry := newWarningsRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	swept := goDone(func() { sweepDriveSync(ctx, owingSweepStore{}, &driveSync{}, registry, time.Hour) })
	t.Cleanup(func() {
		cancel()
		<-swept
	})
	deadline := time.Now().Add(5 * time.Second)
	for len(registry.List()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := strings.Join(registry.List(), "\n"); !strings.Contains(got, "20261106100000") {
		t.Errorf("warnings as the sweep starts = %q, want the owed replacement", got)
	}
}
