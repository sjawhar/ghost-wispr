package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type mergedHookStore struct {
	session storage.Session
	err     error
}

func (s *mergedHookStore) GetSession(string) (storage.Session, error) {
	return s.session, s.err
}

func TestOnSessionMergedRefinesAudioAndSummarizesOnce(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	var summaries int
	onSessionMerged(
		context.Background(),
		&mergedHookStore{session: storage.Session{ID: "merged", AudioPath: "/audio/a.mp3,/audio/b.mp3", StartedAt: started}},
		func(_ context.Context, id, audioPath string, gotStarted time.Time) error {
			if id != "merged" || audioPath != "/audio/a.mp3,/audio/b.mp3" || !gotStarted.Equal(started) {
				t.Fatalf("refine = id:%q audio:%q started:%s", id, audioPath, gotStarted)
			}
			// RefineSession regenerates the merged summary after it publishes.
			summaries++
			return nil
		},
		func(context.Context, string, time.Time) error {
			t.Fatal("hook summarized again after successful refinement")
			return nil
		},
		func(context.Context, string) error { return nil },
		func(context.Context, string, error) {},
		"merged",
	)
	if summaries != 1 {
		t.Fatalf("merged summary calls = %d, want one", summaries)
	}
}

func TestOnSessionMergedSummarizesWithoutAudioOrAfterRefinementFailure(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		audioPath string
		refineErr error
	}{
		{name: "without audio"},
		{name: "refinement failure", audioPath: "/audio/a.mp3", refineErr: errors.New("batch unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			refined, summarized := 0, 0
			onSessionMerged(
				context.Background(),
				&mergedHookStore{session: storage.Session{ID: "merged", AudioPath: test.audioPath, StartedAt: started}},
				func(context.Context, string, string, time.Time) error {
					refined++
					return test.refineErr
				},
				func(_ context.Context, id string, gotStarted time.Time) error {
					if id != "merged" || !gotStarted.Equal(started) {
						t.Fatalf("summarize = id:%q started:%s", id, gotStarted)
					}
					summarized++
					return nil
				},
				func(context.Context, string) error { return nil },
				func(context.Context, string, error) {},
				"merged",
			)
			if test.audioPath == "" && refined != 0 {
				t.Fatalf("refine calls = %d, want none without audio", refined)
			}
			if summarized != 1 {
				t.Fatalf("summary calls = %d, want one", summarized)
			}
		})
	}
}

func TestOnSessionMergedRestoresRemovedAudioBeforeRefining(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	retried, failed, summarized := 0, 0, 0
	onSessionMerged(
		context.Background(),
		&mergedHookStore{session: storage.Session{ID: "merged", AudioPath: "/audio/available.mp3", AudioRemovedAt: "2026-11-13T12:00:00Z", StartedAt: started}},
		func(context.Context, string, string, time.Time) error {
			t.Fatal("merge hook refined removed audio without restoring it")
			return nil
		},
		func(_ context.Context, id string, gotStarted time.Time) error {
			if id != "merged" || !gotStarted.Equal(started) {
				t.Fatalf("summary = id:%q started:%s", id, gotStarted)
			}
			summarized++
			return nil
		},
		func(_ context.Context, id string) error {
			if id != "merged" {
				t.Fatalf("retry session = %q", id)
			}
			retried++
			return errors.New("Drive restore failed")
		},
		func(_ context.Context, id string, err error) {
			if id != "merged" || err == nil {
				t.Fatalf("record failure = id:%q err:%v", id, err)
			}
			failed++
		},
		"merged",
	)
	if retried != 1 || failed != 1 || summarized != 1 {
		t.Fatalf("removed merge retry=%d failed=%d summarized=%d, want one each", retried, failed, summarized)
	}
}
