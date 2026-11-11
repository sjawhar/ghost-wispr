package storage

import (
	"runtime"
	"strings"
	"testing"
)

// TestR7CanonicalizeKeepsTheDeleteOutUntilItWrites holds Canonicalize between
// its read and write, then gives DeleteSegment the processor. The one-connection
// transaction must write before the delete can commit; otherwise the stale
// canonical text would land after the delete.
func TestR7CanonicalizeKeepsTheDeleteOutUntilItWrites(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldProcs) })

	store := newTestSQLiteStore(t)
	_, _ = segmentTestSession(t, store, "s")
	segments, err := store.GetSegments("s")
	if err != nil {
		t.Fatal(err)
	}

	read := make(chan struct{})
	allowWrite := make(chan struct{})
	type canonicalized struct {
		transcript string
		generation int
		err        error
	}
	canonicalizedResult := make(chan canonicalized, 1)
	go func() {
		transcript, generation, err := store.canonicalize("s", func() {
			close(read)
			<-allowWrite
		})
		canonicalizedResult <- canonicalized{transcript: transcript, generation: generation, err: err}
	}()
	<-read

	deleteStarted := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		close(deleteStarted)
		deleteDone <- store.DeleteSegment("s", segments[1].ID, DeletedSpan{
			AudioPath: "recording.mp3", Start: 2, End: 4, SHA256Before: "b", SHA256After: "a",
		})
	}()
	<-deleteStarted
	runtime.Gosched()
	close(allowWrite)

	result := <-canonicalizedResult
	if result.err != nil {
		t.Fatalf("canonicalize: %v", result.err)
	}
	if result.generation != 0 || !strings.Contains(result.transcript, "private deletion token") {
		t.Fatalf("canonicalize = (%q, generation %d), want the pre-delete transcript at generation 0", result.transcript, result.generation)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete segment: %v", err)
	}

	transcript, _, generation, err := store.GetCanonicalTranscriptWithGeneration("s")
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 || strings.Contains(transcript, "private deletion token") {
		t.Fatalf("canonical transcript=%q at generation %d, want the deleted text absent at generation 1", transcript, generation)
	}
}
