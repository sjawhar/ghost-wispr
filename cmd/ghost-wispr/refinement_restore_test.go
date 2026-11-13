package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type refinementRestoreStoreFake struct {
	called   bool
	id       string
	before   string
	restored string
	ok       bool
	err      error
}

func (s *refinementRestoreStoreFake) RestoreAudioForRefinement(id, before, restored string) (bool, error) {
	s.called = true
	s.id = id
	s.before = before
	s.restored = restored
	return s.ok, s.err
}

func TestRestoreRefinementRecordingSkipsDriveForLocalAudio(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(audio, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &refinementRestoreStoreFake{}
	err := restoreRefinementRecording(context.Background(), store, &storage.Session{ID: "s1", AudioPath: audio}, t.TempDir(), func(context.Context, storage.Session) (deletionRecordingSet, error) {
		t.Fatal("tried to list Drive for a recording already on disk")
		return deletionRecordingSet{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.called {
		t.Fatal("recorded local audio as a Drive restoration")
	}
}

func TestRestoreRefinementRecordingStoresDownloadedAudio(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "restored.mp3")
	store := &refinementRestoreStoreFake{ok: true}
	err := restoreRefinementRecording(context.Background(), store, &storage.Session{ID: "s1"}, filepath.Dir(audio), func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: []string{audio}, Downloaded: []string{audio}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !store.called || store.id != "s1" || store.before != "" || store.restored != audio {
		t.Fatalf("restoration write = called:%t id:%q before:%q restored:%q", store.called, store.id, store.before, store.restored)
	}
}

func TestRetryRefinementFailureWarnsBySession(t *testing.T) {
	registry := newWarningsRegistry()
	failure := errors.New("Drive listing timed out")
	if err := retryRefinementWithFailureWarning(context.Background(), registry, "s1", func(context.Context, string) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("retry error = %v, want %v", err, failure)
	}
	warnings := strings.Join(statusWarnings(t, registry), "\n")
	if !strings.Contains(warnings, "s1") || !strings.Contains(warnings, "Drive listing timed out") {
		t.Fatalf("retry failure warning = %q", warnings)
	}
	if err := retryRefinementWithFailureWarning(context.Background(), registry, "s1", func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if warnings := statusWarnings(t, registry); len(warnings) != 0 {
		t.Fatalf("successful retry left warnings: %q", warnings)
	}
}
