package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// gateTranscriber closes entered when a refinement reads its recording, and
// answers with result and err once release is closed.
type gateTranscriber struct {
	entered chan struct{}
	release chan struct{}
	result  transcribe.BatchResult
	err     error
}

func (g *gateTranscriber) Transcribe(context.Context, string) (transcribe.BatchResult, error) {
	close(g.entered)
	<-g.release
	return g.result, g.err
}

// A refinement that read the recording before a segment deletion, and
// finishes after the deletion's own refinement claimed the session, would
// bring the deleted words back (a completed one) or mark the newer
// refinement failed (a failed one). The refinement stores its result only at
// the transcript generation it claimed, so the newer refinement keeps the
// session.
func TestARefinementFromBeforeADeletionLeavesTheNewerRefinement(t *testing.T) {
	for _, old := range []struct {
		name   string
		result transcribe.BatchResult
		err    error
	}{
		{"completed", transcribe.BatchResult{Transcript: "before bridge private deletion token after bridge", Model: "nova-old-run"}, nil},
		{"failed", transcribe.BatchResult{}, errors.New("deepgram: 503")},
	} {
		t.Run(old.name, func(t *testing.T) {
			store, segmentID := r3SessionStore(t)
			transcriber := &gateTranscriber{entered: make(chan struct{}), release: make(chan struct{}), result: old.result, err: old.err}
			manager := NewManager(store, nil, nil, nil, nil, 0)
			manager.SetBatchTranscriber(transcriber)

			done := make(chan struct{})
			go func() { manager.runBatchRefinement(context.Background(), "r3", "recording.mp3"); close(done) }()
			waitR3(t, transcriber.entered, "the refinement did not read the recording")
			deleteR3Session(t, store, segmentID)
			if _, claimed, err := store.ClaimRefinement("r3", "recording.mp3"); err != nil || !claimed {
				t.Fatalf("the deletion's refinement did not claim the session: %v, %v", claimed, err)
			}
			close(transcriber.release)
			waitR3(t, done, "the refinement did not finish")

			sess, err := store.GetSession("r3")
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if strings.Contains(sess.RefinedTranscript, "private") || strings.Contains(sess.CanonicalTranscript, "private") {
				t.Errorf("refined transcript %q, canonical transcript %q: the deleted words came back", sess.RefinedTranscript, sess.CanonicalTranscript)
			}
			if sess.RefinementStatus != storage.RefinementRunning || sess.RefinementModel != "" {
				t.Errorf("refinement status %q, model %q; want the newer refinement's: %q, no model", sess.RefinementStatus, sess.RefinementModel, storage.RefinementRunning)
			}
		})
	}
}
