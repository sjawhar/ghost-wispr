package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// TestABacklogGoesOnlineWhenBatchJobsAreOff fails if a backlog is sent as a
// batch job while summarization.batch_jobs is off (the office Pi's project
// refuses every Vertex AI batch job create), if it is not summarized online,
// session by session, instead, or if a backlog too small for a batch job is
// sent as one while batch jobs are on.
func TestABacklogGoesOnlineWhenBatchJobsAreOff(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	for _, c := range []struct {
		name      string
		batchJobs bool
		ids       []string
		wantBatch bool
	}{
		{"batch jobs off", false, ids, false},
		{"batch jobs on", true, ids, true},
		{"batch jobs on, a backlog under the minimum", true, ids[:3], false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var submitted []string
			online := make(chan []string, 1)
			batch, count := summarizeBacklog(context.Background(), c.batchJobs, c.ids, 4,
				func(_ context.Context, ids []string) (string, int, error) {
					submitted = ids
					return "batches/1", len(ids), nil
				},
				func(ids []string) { online <- ids })
			if count != len(c.ids) {
				t.Fatalf("summarizeBacklog = %q, %d; want %d sessions", batch, count, len(c.ids))
			}
			if c.wantBatch {
				if batch != "batches/1" || !slices.Equal(submitted, c.ids) {
					t.Fatalf("batch %q of %q; want the backlog as one batch job", batch, submitted)
				}
				select {
				case got := <-online:
					t.Fatalf("a backlog sent as a batch job was also summarized online: %q", got)
				case <-time.After(50 * time.Millisecond):
				}
				return
			}
			if batch != "" || submitted != nil {
				t.Fatalf("batch %q of %q; want no batch job", batch, submitted)
			}
			select {
			case got := <-online:
				if !slices.Equal(got, c.ids) {
					t.Fatalf("summarized online %q, want %q", got, c.ids)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the backlog was not summarized online")
			}
		})
	}
}

// TestARefusedBatchJobGoesOnline fails if a backlog whose batch job is not
// taken (the provider refuses it, or takes none) is reported as a batch job,
// or is not summarized online instead.
func TestARefusedBatchJobGoesOnline(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	for _, refused := range []error{errors.New("permission denied"), errNoBatchProvider} {
		online := make(chan []string, 1)
		batch, count := summarizeBacklog(context.Background(), true, ids, 4,
			func(context.Context, []string) (string, int, error) { return "", 0, refused },
			func(ids []string) { online <- ids })
		if batch != "" || count != len(ids) {
			t.Fatalf("%v: summarizeBacklog = %q, %d; want no batch job and %d sessions", refused, batch, count, len(ids))
		}
		select {
		case got := <-online:
			if !slices.Equal(got, ids) {
				t.Fatalf("%v: summarized online %q, want %q", refused, got, ids)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%v: the backlog was not summarized online", refused)
		}
	}
}
