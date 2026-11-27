package main

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestARawKeptAfterAStopIsSavedAtItsTrueDuration fails if the app's recorder
// does not keep the rate of a session's raw recording with the session in the
// database, so that the next start saves a raw recording the stop kept at the
// rate it was captured at. Before, 2 s captured at 48000 Hz were saved as 6 s.
func TestARawKeptAfterAStopIsSavedAtItsTrueDuration(t *testing.T) {
	runParallel(t)
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "ghost-wispr.db")
	audioDir := t.TempDir()
	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const id = "20261118100000"
	if err := store.CreateSession(id, time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	stopped := newSessionRecorder(audioDir, store)
	if err := stopped.SetSampleRate(48000); err != nil {
		t.Fatal(err)
	}
	if err := stopped.StartSession(id); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, err := stopped.Writer(nopWriter{}).Write(make([]byte, 2*2*48000)); err != nil {
		t.Fatal(err)
	}
	_ = store.Close() // the power cut

	reopened, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	saved, err := newSessionRecorder(audioDir, reopened).SaveKept(context.Background(), id)
	if err != nil {
		t.Fatalf("SaveKept: %v", err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", saved).Output()
	if err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(seconds-2) > 0.1 {
		t.Fatalf("the recording saved from the kept raw recording lasts %.2f s, want 2 s", seconds)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestADefaultDeepgramBufferHoldsSixtySecondsAtTheMicsRate fails if, with
// deepgram_buffer_size unset, the buffer for live audio while Deepgram
// reconnects is not 60 s at the rate the microphone opened at: the default was
// 1,920,000 bytes, 60 s at 16000 Hz, so 20 s for a microphone opened at 48000.
func TestADefaultDeepgramBufferHoldsSixtySecondsAtTheMicsRate(t *testing.T) {
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, rate := range []int{48000, 16000} {
		rc := transcribe.NewResilientClient(ctx, nil, liveTranscriptionConfig(&cfg, rate), nil, nil)
		if got, want := rc.Config.BufferSize, 60*rate*2; got != want {
			t.Errorf("at %d Hz the reconnect buffer holds %d bytes (%.1f s), want %d (60 s)", rate, got, float64(got)/float64(2*rate), want)
		}
	}
}
