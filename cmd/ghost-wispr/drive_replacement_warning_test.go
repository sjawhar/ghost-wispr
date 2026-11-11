package main

import (
	"slices"
	"testing"
)

// TestDriveReplacementWarningsReachStatusAndClear fails if the app's hooks
// for a failing Drive replacement do not put one warning per session on the
// registry GET /api/status lists, or do not take it off again.
func TestDriveReplacementWarningsReachStatusAndClear(t *testing.T) {
	registry := newWarningsRegistry()
	set, clear := driveReplacementWarnings(registry)

	set("20261106100000", "Drive still holds deleted words of session 20261106100000")
	set("20261106110000", "Drive still holds deleted words of session 20261106110000")
	set("20261106100000", "Drive still holds deleted words of session 20261106100000, again")
	if got := registry.List(); !slices.Equal(got, []string{
		"Drive still holds deleted words of session 20261106100000, again",
		"Drive still holds deleted words of session 20261106110000",
	}) {
		t.Fatalf("warnings = %q, want the latest one for each session", got)
	}

	clear("20261106100000")
	if got := registry.List(); !slices.Equal(got, []string{"Drive still holds deleted words of session 20261106110000"}) {
		t.Fatalf("warnings after the replacement finished = %q, want only the other session's", got)
	}
}
