package gdrive

import (
	"context"
	"slices"
	"testing"
	"time"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// A restore names each recording its folder holds, in the local form a later
// merge or retry downloads it under; an audio file that is not a recording's
// Drive name is not a recording.
func TestRestoreFromDriveNamesTheFolderRecordings(t *testing.T) {
	wire, syncer := newSharedDriveWire(t)
	started := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	folder := wire.add(sharedDriveID, "2026-11-10-merged", "application/vnd.google-apps.folder", nil, nil)
	transcript := RenderTranscriptMarkdown(&SyncSession{ID: "20261110090000-merged", StartedAt: started}, []transcribe.Segment{{
		Speaker: 0, Text: "two recordings", Timestamp: started,
	}})
	wire.add(folder, "transcript.md", "application/vnd.google-apps.document", []byte(transcript), nil)
	wire.add(folder, recordingname.FileName("/audio/20261110100000.mp3"), "audio/mpeg", []byte("b"), nil)
	wire.add(folder, recordingname.FileName("/audio/20261110090000.mp3"), "audio/mpeg", []byte("a"), nil)
	wire.add(folder, "voice-memo.mp3", "audio/mpeg", []byte("not a recording"), nil)

	var restored []RestoredSession
	result, err := syncer.RestoreFromDrive(context.Background(), func(sess RestoredSession) error {
		restored = append(restored, sess)
		return nil
	})
	if err != nil || result.Restored != 1 || len(restored) != 1 {
		t.Fatalf("restore = %+v, %v, sessions %d; want one restored session", result, err, len(restored))
	}
	want := []string{"20261110090000.mp3", "20261110100000.mp3"}
	if !slices.Equal(restored[0].RecordingPaths, want) {
		t.Fatalf("restored recording names = %q, want %q", restored[0].RecordingPaths, want)
	}
}
