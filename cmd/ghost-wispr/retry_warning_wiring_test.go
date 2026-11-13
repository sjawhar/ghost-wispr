package main

import (
	"os"
	"strings"
	"testing"
)

func TestRetryRefinementHookRecordsFailureWarnings(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "return retryRefinementWithFailureWarning(ctx, warningRegistry, sessionID, func") {
		t.Fatal("RetryRefinement bypasses retryRefinementWithFailureWarning")
	}
}
