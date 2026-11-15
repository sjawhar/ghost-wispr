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

func (*refinementRestoreStoreFake) DriveRestoreAllowed(string) (bool, error) { return true, nil }

func (*refinementRestoreStoreFake) RefinementRecordingPaths(string) ([]string, error) {
	return nil, nil
}

type refinementRestoreStoreReaderFake struct {
	*refinementRestoreStoreFake
	initial storage.Session
	current storage.Session
	reads   int
}

func (s *refinementRestoreStoreReaderFake) GetSession(string) (storage.Session, error) {
	s.reads++
	if s.reads == 1 {
		return s.initial, nil
	}
	return s.current, nil
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

func TestRestoreRefinementRecordingRejectsAChangedAudioPath(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "restored.mp3")
	store := &refinementRestoreStoreFake{ok: false}
	err := restoreRefinementRecording(context.Background(), store, &storage.Session{ID: "s1"}, filepath.Dir(audio), func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: []string{audio}, Downloaded: []string{audio}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "recording changed") {
		t.Fatalf("restore with changed audio path = %v, want refusal", err)
	}
	if !store.called {
		t.Fatal("changed-audio restoration did not attempt its fenced write")
	}
}

func TestRestoreRefinementRecordingRestoresMissingNamedAudio(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "missing.mp3")
	store := &refinementRestoreStoreFake{ok: true}
	restoreCalled := false
	err := restoreRefinementRecording(context.Background(), store, &storage.Session{ID: "s1", AudioPath: audio}, filepath.Dir(audio), func(context.Context, storage.Session) (deletionRecordingSet, error) {
		restoreCalled = true
		return deletionRecordingSet{Paths: []string{audio}, Downloaded: []string{audio}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restoreCalled {
		t.Fatal("missing named audio skipped the Drive restoration")
	}
}

func TestRestoreRefinementRemovesDownloadsAfterARequiredRecordingIsMissing(t *testing.T) {
	dir := t.TempDir()
	downloaded := filepath.Join(dir, "restored.mp3")
	if err := os.WriteFile(downloaded, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.mp3")
	session := &storage.Session{ID: "s1", AudioPath: downloaded + "," + missing, AudioRemovedAt: "2026-11-13T12:00:00Z"}
	store := &refinementRestoreStoreFake{}
	err := restoreRefinementRecording(context.Background(), store, session, dir, func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: []string{downloaded}, Downloaded: []string{downloaded}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "did not restore") {
		t.Fatalf("partial restore = %v, want missing-recording refusal", err)
	}
	if _, err := os.Stat(downloaded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial restore left downloaded recording: %v", err)
	}
}

func TestRestoreRefinementKeepsDownloadsNamedByTheWinningWrite(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "restored.mp3")
	if err := os.WriteFile(audio, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := storage.Session{ID: "s1", AudioRemovedAt: "2026-11-13T12:00:00Z"}
	store := &refinementRestoreStoreReaderFake{
		refinementRestoreStoreFake: &refinementRestoreStoreFake{ok: false},
		initial:                    initial,
		current:                    storage.Session{ID: "s1", AudioPath: audio},
	}
	err := restoreRefinementRecording(context.Background(), store, &initial, filepath.Dir(audio), func(context.Context, storage.Session) (deletionRecordingSet, error) {
		return deletionRecordingSet{Paths: []string{audio}, Downloaded: []string{audio}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "recording changed") {
		t.Fatalf("raced restore = %v, want changed-recording refusal", err)
	}
	if _, err := os.Stat(audio); err != nil {
		t.Fatalf("raced restore removed recording named by the winning write: %v", err)
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
