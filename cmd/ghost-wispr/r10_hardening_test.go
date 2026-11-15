package main

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestR10LostCutKeepsPrecutBytesOffTheRecordingPath fails if recovery puts a
// downloaded pre-cut recording at its real path before the checked re-splice
// result atomically replaces it.
func TestR10LostCutKeepsPrecutBytesOffTheRecordingPath(t *testing.T) {
	fixture := newR9MergedCutFixture(t)
	precut := []byte("recording C before cut")
	after := []byte("recording C after cut")
	var during []byte
	outcomes, err := recoverPendingSplices(context.Background(), fixture.store, fixture.dir, fixture.drive, func(_ context.Context, path string, _, _ float64) (string, error) {
		during, _ = os.ReadFile(fixture.audioC)
		if err := os.WriteFile(path, after, 0o600); err != nil {
			return "", err
		}
		return hashBytesForDeleteTest(after), nil
	}, nil)
	if err != nil || len(outcomes) != 1 || !outcomes[0].Recovered || outcomes[0].Err != nil {
		t.Fatalf("lost-cut recovery = outcomes:%+v err:%v", outcomes, err)
	}
	if bytes.Equal(during, precut) {
		t.Fatal("recovery exposed pre-cut C at its real path while re-splicing")
	}
}
