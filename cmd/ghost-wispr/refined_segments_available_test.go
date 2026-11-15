package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// TestRefinedSegmentsAvailableRefinesAnOldCompletedSession fails if a
// deletion from a session without timed words is not refused, or if the
// refusal does not start the batch refinement of a completed session with a
// recording, or starts one for any other session.
func TestRefinedSegmentsAvailableRefinesAnOldCompletedSession(t *testing.T) {
	started := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		session storage.Session
		refused bool
		refines bool
	}{
		{"timed words", storage.Session{RefinementStatus: storage.RefinementCompleted, RefinedRevision: 2, AudioPath: "a.mp3"}, false, false},
		{"completed without timed words", storage.Session{RefinementStatus: storage.RefinementCompleted, AudioPath: "a.mp3", StartedAt: started}, true, true},
		{"completed without a recording", storage.Session{RefinementStatus: storage.RefinementCompleted, AudioPath: " "}, true, false},
		{"refinement failed", storage.Session{RefinementStatus: storage.RefinementFailed, AudioPath: "a.mp3"}, true, false},
	}
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			refined := make(chan string, 1)
			runner := &segmentDeleteRunner{refine: func(_ context.Context, sessionID, audioPath string, startedAt time.Time) error {
				refined <- sessionID + " " + audioPath + " " + startedAt.Format(time.RFC3339)
				return nil
			}}
			err := runner.refinedSegmentsAvailable("s1", &tc.session)
			if refused := errors.Is(err, storage.ErrRefinedSegmentsUnavailable); refused != tc.refused || (err != nil && !refused) {
				t.Fatalf("refinedSegmentsAvailable = %v, want refused %v", err, tc.refused)
			}
			if !tc.refines {
				select {
				case got := <-refined:
					t.Fatalf("started refinement %q, want none", got)
				case <-time.After(50 * time.Millisecond):
				}
				return
			}
			select {
			case got := <-refined:
				if want := "s1 a.mp3 " + started.Format(time.RFC3339); got != want {
					t.Fatalf("started refinement %q, want %q", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no refinement started")
			}
		})
	}
}
