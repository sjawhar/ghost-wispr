package main

import (
	"context"
	"testing"
)

func TestRetryRefinementRestoresRecordingBeforeRefining(t *testing.T) {
	restored := false
	err := retryRefinementAfterRestoringRecording(
		context.Background(),
		"session-1",
		func(context.Context, string) error {
			restored = true
			return nil
		},
		func(context.Context, string) error {
			if !restored {
				t.Fatal("refinement began before the Drive recording was restored")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}
