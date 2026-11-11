package gdrive

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/gc"
)

type recordingNameVerifier struct {
	checked []string
}

func (*recordingNameVerifier) FolderOnDrive(context.Context, string) (bool, error) {
	return true, nil
}

func (v *recordingNameVerifier) RecordingsMatch(_ context.Context, _ string, checksums map[string]string) (bool, error) {
	v.checked = slices.Sorted(maps.Keys(checksums))
	return true, nil
}

func TestSyncAndGCUseTheSameRecordingFileName(t *testing.T) {
	store := openStore(t)
	audioDir := t.TempDir()
	const id = "20260901100000"
	summarizedSession(t, store, audioDir, id, time.Now().UTC().Add(-40*24*time.Hour), "The summary.")
	syncTo(t, store, id, "SYNCED", "folder", 0, "")

	sess, err := store.GetSession(id)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	segments, err := store.GetSegments(id)
	if err != nil {
		t.Fatalf("read segments: %v", err)
	}
	syncSession := toSyncSession(&sess)
	files, _, err := BuildSyncFiles(&syncSession, segments, sess.AudioPath)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}
	var uploaded []string
	for _, file := range files {
		if file.LocalPath != "" {
			uploaded = append(uploaded, file.Name)
		}
	}
	slices.Sort(uploaded)

	verifier := &recordingNameVerifier{}
	if _, err := gc.New(store, gc.Config{MaxAgeDays: 30, AudioDir: audioDir, Drive: verifier}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(); err != nil {
		t.Fatalf("run GC: %v", err)
	}
	if !slices.Equal(verifier.checked, uploaded) {
		t.Fatalf("GC checked recording names %v, sync uploaded %v", verifier.checked, uploaded)
	}
}
