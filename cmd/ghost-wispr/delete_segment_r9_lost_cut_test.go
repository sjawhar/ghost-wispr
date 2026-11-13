package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestR9RecoversLostSplicedRecordingForPendingReplacement fails if a missing
// local recording that Drive still owes a replacement cannot be restored from
// its pre-cut copy, spliced again, checked against its stored post-cut hash,
// and sent through the pending refinement path.
func TestR9RecoversLostSplicedRecordingForPendingReplacement(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	after := []byte("recording C after cut")
	refined := false
	outcomes, err := recoverPendingSplices(context.Background(), fixture.store, fixture.dir, fixture.drive, func(_ context.Context, path string, _, _ float64) (string, error) {
		if path == fixture.audioC {
			t.Fatalf("splicer edited the restored recording instead of its temporary copy: %s", path)
		}
		if err := os.WriteFile(path, after, 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest(after), nil
	}, func(_ context.Context, sessionID, audioPath string, _ time.Time) error {
		if sessionID != "merged" || !strings.Contains(audioPath, fixture.audioC) {
			t.Fatalf("refine = session:%q audio:%q, want restored C", sessionID, audioPath)
		}
		refined = true
		return nil
	})
	if err != nil || len(outcomes) != 1 || !outcomes[0].Recovered || outcomes[0].Err != nil {
		t.Fatalf("lost-cut recovery = outcomes:%+v err:%v, want one recovered splice", outcomes, err)
	}
	if !refined {
		t.Fatal("recovered cut did not continue to refinement")
	}
	contents, err := os.ReadFile(fixture.audioC)
	if err != nil || string(contents) != string(after) {
		t.Fatalf("recovered C = %q, %v; want checked post-cut bytes", contents, err)
	}
	if got := strings.Join(fixture.drive.downloaded, ","); !strings.Contains(got, "audio-c.mp3") {
		t.Fatalf("recovery did not download C's pre-cut Drive copy: %q", got)
	}
	session, err := fixture.store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if !session.DriveReplacePending {
		t.Fatal("recovering the local cut cleared the Drive replacement debt")
	}
}

// TestR9LostCutRecoveryWarnsAndKeepsReplacementOwed fails if a recovery whose
// re-splice does not reproduce the recorded hash leaves the pre-cut bytes on
// disk, clears the Drive replacement debt, or omits the session from status.
func TestR9LostCutRecoveryWarnsAndKeepsReplacementOwed(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	outcomes, err := recoverPendingSplices(context.Background(), fixture.store, fixture.dir, fixture.drive, func(_ context.Context, path string, _, _ float64) (string, error) {
		if err := os.WriteFile(path, []byte("wrong cut"), 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest([]byte("wrong cut")), nil
	}, func(context.Context, string, string, time.Time) error {
		t.Fatal("failed cut recovery ran refinement")
		return nil
	})
	if err != nil || len(outcomes) != 1 || outcomes[0].Err == nil {
		t.Fatalf("failed lost-cut recovery = outcomes:%+v err:%v, want one failed splice", outcomes, err)
	}
	outcome := outcomes[0]
	if got := strings.Join(fixture.drive.downloaded, ","); !strings.Contains(got, "audio-c.mp3") {
		t.Fatalf("failed recovery did not use C's controlled pre-cut restore: %q", got)
	}
	if _, err := os.Stat(fixture.audioC); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed recovery left C's pre-cut bytes on disk: %v", err)
	}
	registry := newWarningsRegistry()
	applyPendingSpliceRecoveryWarnings(registry, []pendingSpliceOutcome{outcome})
	warnings := strings.Join(statusWarnings(t, registry), "\n")
	if !strings.Contains(warnings, "merged") {
		t.Fatalf("status warning = %q, want the affected session", warnings)
	}
	session, err := fixture.store.GetSession("merged")
	if err != nil {
		t.Fatal(err)
	}
	if !session.DriveReplacePending {
		t.Fatal("failed recovery cleared the Drive replacement debt")
	}
}
