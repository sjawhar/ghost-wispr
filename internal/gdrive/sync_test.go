package gdrive

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	driveapi "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/googleauthtest"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestDriveUsesAuthCredentials(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":"root"}`))
	}))
	t.Cleanup(server.Close)

	credentialPath := googleauthtest.X509CredentialFile(t, "fake-drive-token")
	credentials, err := googleauth.Detect(context.Background(), credentialPath, driveapi.DriveFileScope)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	syncer, err := newSyncerWithLogger(context.Background(), credentials, "root", slog.Default(), option.WithEndpoint(server.URL+"/drive/v3/"))
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	if _, err := syncer.service.Files.Get("root").Do(); err != nil {
		t.Fatalf("get Drive root: %v", err)
	}
	if authorization != "Bearer fake-drive-token" {
		t.Fatalf("Authorization = %q, want fake-drive-token", authorization)
	}
}

// RecordingsMatch reads Drive's md5Checksum rather than trusting a folder's
// presence or an audio file's name.
func TestSyncerRecordingsMatchDriveMD5Checksums(t *testing.T) {
	wire, syncer := newWireDrive(t)
	folder := wire.add("root", "2026-11-06 Standup", "application/vnd.google-apps.folder", nil, nil)
	recording := []byte("recording bytes")
	wire.add(folder, "audio-recording.mp3", "audio/mpeg", recording, nil)
	sum := md5.Sum(recording)
	checksums := map[string]string{"audio-recording.mp3": hex.EncodeToString(sum[:])}

	matches, err := syncer.RecordingsMatch(context.Background(), folder, checksums)
	if err != nil {
		t.Fatalf("match recordings: %v", err)
	}
	if !matches {
		t.Error("Drive's matching md5Checksum did not verify the recording")
	}

	checksums["audio-recording.mp3"] = "00000000000000000000000000000000"
	matches, err = syncer.RecordingsMatch(context.Background(), folder, checksums)
	if err != nil {
		t.Fatalf("compare different checksum: %v", err)
	}
	if matches {
		t.Error("Drive verified a recording with a different md5Checksum")
	}

	matches, err = syncer.RecordingsMatch(context.Background(), folder, map[string]string{"audio-absent.mp3": checksums["audio-recording.mp3"]})
	if err != nil {
		t.Fatalf("compare absent recording: %v", err)
	}
	if matches {
		t.Error("Drive verified an absent recording")
	}
}

func TestBuildSyncFiles(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	sess := SyncSession{
		ID:            "20260321-143022",
		Title:         "Weekly Standup",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "Team discussed sprint progress.",
		SummaryPreset: "default",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-mp3"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, folderName, err := BuildSyncFiles(&sess, segments, audioPath)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}

	if folderName != "2026-03-21-weekly-standup" {
		t.Errorf("expected folder name %q, got %q", "2026-03-21-weekly-standup", folderName)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	names := map[string]string{}
	for _, f := range files {
		names[f.Name] = f.MimeType
	}
	if names["summary.md"] != "application/vnd.google-apps.document" {
		t.Error("summary.md should convert to Google Doc")
	}
	if names["transcript.md"] != "application/vnd.google-apps.document" {
		t.Error("transcript.md should convert to Google Doc")
	}
	if _, ok := names["audio-test.mp3"]; !ok {
		t.Error("missing audio-test.mp3")
	}
}

func TestBuildSyncFilesNoAudio(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	sess := SyncSession{
		ID:            "20260321-143022",
		Title:         "Quick Chat",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "Brief discussion.",
		SummaryPreset: "default",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	files, _, err := BuildSyncFiles(&sess, segments, "")
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (no audio), got %d", len(files))
	}
}

func TestBuildSyncFilesNoSummary(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	sess := SyncSession{
		ID:        "20260321-143022",
		Title:     "No Summary Session",
		StartedAt: started,
		EndedAt:   &ended,
		Summary:   "",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-mp3"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, _, err := BuildSyncFiles(&sess, segments, audioPath)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (no summary), got %d", len(files))
	}
	for _, f := range files {
		if f.Name == "summary.md" {
			t.Error("should not include summary.md when summary is empty")
		}
	}
}

// Every recording a session's audio path names is uploaded, as "audio-" and
// its own file name: a name in the folder identifies one recording, whatever
// the audio path names before or after, so a re-sync that skips a file by
// name can only skip the recording it uploaded before. (Named by position,
// audio-1, audio-2, a recording took another's name once the audio path
// shrank, and was taken as uploaded.) Before, the whole joined path was
// stat'ed, failed, and the session synced with no audio, so GC gated on sync
// could remove audio that never reached Drive.
func TestBuildSyncFilesUploadsEveryRecording(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	sess := SyncSession{ID: "20260321143022-merged", Title: "Merged", StartedAt: started}
	dir := t.TempDir()
	first := filepath.Join(dir, "20260321143022.mp3")
	second := filepath.Join(dir, "20260321150000.wav")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	audio := func(audioPath string) map[string]string {
		t.Helper()
		files, _, err := BuildSyncFiles(&sess, nil, audioPath)
		if err != nil {
			t.Fatalf("build sync files: %v", err)
		}
		names := map[string]string{}
		for _, f := range files {
			if f.LocalPath != "" {
				names[f.Name] = f.LocalPath
			}
		}
		return names
	}

	want := map[string]string{"audio-20260321143022.mp3": first, "audio-20260321150000.wav": second}
	if got := audio(first + ", " + second); !maps.Equal(got, want) {
		t.Errorf("uploaded audio %v, want %v", got, want)
	}
	// The same recording keeps its name with the other gone, and alone.
	if got := audio(second); !maps.Equal(got, map[string]string{"audio-20260321150000.wav": second}) {
		t.Errorf("with the other recording gone, uploaded audio %v, want audio-20260321150000.wav from %s", got, second)
	}
}

// A re-sync into a session's folder creates a recording only if the folder
// has no file of its name, creates a document the folder lacks, and updates
// in place a document whose content changed since it was uploaded (or that
// was uploaded before documents carried their content's hash); one with the
// same content stays. Nothing is ever deleted. Of two copies of a document,
// the newest decides and is the one updated.
func TestPlanUpload(t *testing.T) {
	summary := SyncFile{Name: "summary.md", Content: []byte("the summary, re-summarized")}
	transcript := SyncFile{Name: "transcript.md", Content: []byte("the transcript")}
	audio := SyncFile{Name: "audio-20261106100000.mp3", LocalPath: "/audio/20261106100000.mp3"}
	other := SyncFile{Name: "audio-20261106101000.mp3", LocalPath: "/mnt/20261106101000.mp3"}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	names := func(files []SyncFile) []string {
		var names []string
		for i := range files {
			names = append(names, files[i].Name)
		}
		return names
	}
	updated := func(update []docUpdate) []string {
		var ids []string
		for i := range update {
			ids = append(ids, update[i].ID+"="+update[i].File.Name)
		}
		return ids
	}

	create, update := planUpload([]driveFile{
		{ID: "old-summary", Name: "summary.md", ContentHash: contentHash(&SyncFile{Content: []byte("the summary")})},
		{ID: "transcript", Name: "transcript.md", ContentHash: contentHash(&transcript)},
		{ID: "audio", Name: "audio-20261106100000.mp3"},
	}, []SyncFile{summary, transcript, audio, other})
	if want := []string{"audio-20261106101000.mp3"}; !slices.Equal(names(create), want) {
		t.Errorf("creates %v, want %v", names(create), want)
	}
	if want := []string{"old-summary=summary.md"}; !slices.Equal(updated(update), want) {
		t.Errorf("updates %v, want %v", updated(update), want)
	}

	// A document uploaded before hashes were kept has no hash: updated.
	create, update = planUpload([]driveFile{{ID: "unhashed", Name: "transcript.md"}}, []SyncFile{transcript, summary})
	if !slices.Equal(names(create), []string{"summary.md"}) || !slices.Equal(updated(update), []string{"unhashed=transcript.md"}) {
		t.Errorf("for a document with no hash: creates %v, updates %v; want [summary.md] and [unhashed=transcript.md]", names(create), updated(update))
	}

	// Two copies: the newest decides, and is the one updated.
	create, update = planUpload([]driveFile{
		{ID: "older", Name: "transcript.md", ContentHash: contentHash(&transcript), ModifiedTime: at},
		{ID: "newer", Name: "transcript.md", ModifiedTime: at.Add(time.Hour)},
	}, []SyncFile{transcript})
	if len(create) != 0 || !slices.Equal(updated(update), []string{"newer=transcript.md"}) {
		t.Errorf("with an older current and a newer stale copy: creates %v, updates %v; want none and [newer=transcript.md]", names(create), updated(update))
	}
	create, update = planUpload([]driveFile{
		{ID: "older", Name: "transcript.md", ModifiedTime: at},
		{ID: "newer", Name: "transcript.md", ContentHash: contentHash(&transcript), ModifiedTime: at.Add(time.Hour)},
	}, []SyncFile{transcript})
	if len(create) != 0 || len(update) != 0 {
		t.Errorf("with a newer current copy: creates %v, updates %v; want nothing", names(create), updated(update))
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Weekly Standup", "weekly-standup"},
		{"Design Review: Q1 Planning", "design-review-q1-planning"},
		{"  Spaces  Everywhere  ", "spaces-everywhere"},
		{"ALLCAPS", "allcaps"},
		{"special!@#chars$%^", "specialchars"},
		{"", "untitled"},
	}
	for _, tc := range tests {
		got := slugify(tc.input)
		if got != tc.expected {
			t.Errorf("slugify(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestShouldUseResumableUpload(t *testing.T) {
	tests := []struct {
		name     string
		fileSize int64
		want     bool
	}{
		{name: "below threshold uses regular upload", fileSize: 5*1024*1024 - 1, want: false},
		{name: "exact threshold uses regular upload", fileSize: 5 * 1024 * 1024, want: false},
		{name: "above threshold uses resumable", fileSize: 5*1024*1024 + 1, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldUseResumableUpload(tc.fileSize)
			if got != tc.want {
				t.Fatalf("shouldUseResumableUpload(%d) = %v, want %v", tc.fileSize, got, tc.want)
			}
		})
	}
}
