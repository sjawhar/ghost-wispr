package main

import (
	"os"
	"strings"
	"testing"
)

func TestRetryRefinementClosureRestoresMissingAudio(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "return restoreRefinementRecording(ctx, store, &sess, cfgStore.Get().AudioDir, deletionRunner().ensureRecordings)") {
		t.Fatal("retry refinement closure bypasses restoreRefinementRecording")
	}
}
