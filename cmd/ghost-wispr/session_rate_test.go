package main

import (
	"context"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// probeDuration returns the duration ffprobe reads from the audio file at
// path.
func probeDuration(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe %s printed %q: %v", path, out, err)
	}
	return seconds
}

// setRate tells recorder the microphone opened at rate, as startMicPipeline
// does.
func setRate(t *testing.T, recorder interface{ SetSampleRate(int) error }, rate int) {
	t.Helper()
	if err := recorder.SetSampleRate(rate); err != nil {
		t.Fatalf("SetSampleRate(%d): %v", rate, err)
	}
}

// TestASessionStartedBeforeTheMicOpensIsSavedAtTheMicsRate fails if a
// session that starts before the microphone first opens is labelled, and
// saved, at the recorder's rate when it started rather than the rate the
// microphone then opened at. POST /api/sessions/start can start one while no
// microphone is open: during the start-up retry, while the light is not
// confirmed red, or on a unit that booted paused before its first open. The
// raw file holds no audio until the microphone opens, so all its audio is at
// that rate (ReviewPR78 on #78, finding 1). It runs the real Manager,
// Recorder and SQLiteStore, as the reviewer's probe did, both ways: the
// default 16000 Hz, then an open at 48000 Hz; and an open at 48000 Hz that
// the light refused, then a retry that opened at 16000 Hz. The control sets
// the rate before the session starts.
func TestASessionStartedBeforeTheMicOpensIsSavedAtTheMicsRate(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}
	for _, tc := range []struct {
		name   string
		before int // the recorder's rate when the session starts; 0 leaves the default
		opened int // the rate the microphone then opens at
	}{
		{name: "control: the microphone opened before the session", before: 48000, opened: 48000},
		{name: "16000 Hz, then an open at 48000 Hz", opened: 48000},
		{name: "48000 Hz refused on the light, then a retry at 16000 Hz", before: 48000, opened: 16000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			recorder := newSessionRecorder(t.TempDir(), store)
			manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(1<<62), 0)
			if tc.before > 0 {
				setRate(t, recorder, tc.before)
			}

			id, err := manager.ManualStartSession(context.Background(), "")
			if err != nil {
				t.Fatalf("start session: %v", err)
			}
			setRate(t, recorder, tc.opened)
			if _, err := recorder.Writer(io.Discard).Write(make([]byte, 2*2*tc.opened)); err != nil {
				t.Fatal(err)
			}
			if rate, err := store.SessionSampleRate(id); err != nil || rate != tc.opened {
				t.Errorf("the session's row holds %d Hz (%v), want %d: a raw kept after a stop would be saved at the wrong rate", rate, err, tc.opened)
			}
			if err := manager.ForceEndSession(context.Background()); err != nil {
				t.Fatalf("end session: %v", err)
			}
			sess, err := store.GetSession(id)
			if err != nil || sess.AudioPath == "" {
				t.Fatalf("the ended session names %q (%v), want its saved recording", sess.AudioPath, err)
			}
			if got := probeDuration(t, sess.AudioPath); math.Abs(got-2) > 0.1 {
				t.Errorf("the saved recording lasts %.2f s, want 2 s, the audio captured at %d Hz", got, tc.opened)
			}
		})
	}
}
