# Google Drive Sync & Garbage Collection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add per-session Google Drive backup (summary.md, transcript.md, audio) with sync tracking, local garbage collection for synced sessions, and full web UI configuration.

**Architecture:** Bottom-up layered approach — schema migration → sync engine → GC engine → config wiring → API endpoints → web UI. Each layer is independently testable. The sync engine hooks into the existing session lifecycle (post-summarization). GC runs on a periodic sweep.

**Tech Stack:** Go (backend), SQLite (storage), Google Drive API v3 (`google.golang.org/api/drive/v3`), Svelte 5 (frontend)

---

### Task 1: Add sync tracking columns to SQLite schema

**Files:**
- Modify: `internal/storage/sqlite.go`
- Modify: `internal/storage/sqlite_test.go`

**Step 1: Write the failing test**

Add a test that creates a session, ends it, then calls `UpdateSyncStatus` and `GetSessionsNeedingSync`:

```go
func TestSyncStatusTracking(t *testing.T) {
	store := newTestStore(t)
	sessionID := "sync-test-1"
	started := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)

	if err := store.CreateSession(sessionID, started); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.EndSession(sessionID, started.Add(30*time.Second), "data/audio/sync-test-1.mp3"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if err := store.UpdateSummary(sessionID, "Test Title", "Test summary", SummaryCompleted, "default"); err != nil {
		t.Fatalf("update summary: %v", err)
	}

	// Should appear in needs-sync list (ended + summary completed + sync pending).
	ids, err := store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatalf("get sessions needing sync: %v", err)
	}
	if len(ids) != 1 || ids[0] != sessionID {
		t.Fatalf("expected [%s], got %v", sessionID, ids)
	}

	// Mark as synced.
	if err := store.UpdateSyncStatus(sessionID, SyncSynced, "drive-folder-id-123"); err != nil {
		t.Fatalf("update sync status: %v", err)
	}

	// Should no longer appear.
	ids, err = store.GetSessionsNeedingSync()
	if err != nil {
		t.Fatalf("get sessions needing sync after update: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected empty, got %v", ids)
	}

	// Verify sync fields on GetSession.
	sess, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.SyncStatus != SyncSynced {
		t.Fatalf("expected sync_status %q, got %q", SyncSynced, sess.SyncStatus)
	}
	if sess.GDriveFolderID != "drive-folder-id-123" {
		t.Fatalf("expected gdrive_folder_id %q, got %q", "drive-folder-id-123", sess.GDriveFolderID)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/storage/ -run TestSyncStatusTracking -v -count=1`
Expected: FAIL — `UpdateSyncStatus` and `GetSessionsNeedingSync` don't exist, `SyncStatus`/`GDriveFolderID` fields missing from `Session`.

**Step 3: Implement schema changes and new methods**

In `internal/storage/sqlite.go`:

1. Add constants:
```go
const (
	SyncPending = "pending"
	SyncSynced  = "synced"
	SyncFailed  = "failed"
)
```

2. Add fields to `Session` struct:
```go
SyncStatus     string `json:"sync_status"`
GDriveFolderID string `json:"gdrive_folder_id"`
```

3. Add migration in `init()`:
```go
`ALTER TABLE sessions ADD COLUMN sync_status TEXT NOT NULL DEFAULT 'pending'`,
`ALTER TABLE sessions ADD COLUMN gdrive_folder_id TEXT NOT NULL DEFAULT ''`,
```

4. Update ALL `SELECT` queries and `scanSessions` to include the two new columns. This affects:
   - `GetSessionsByDate` query
   - `GetSession` query
   - `scanSessions` helper
   - `RecoverStaleSessions` (does not scan sessions, no change needed)

5. Add new methods:
```go
func (s *SQLiteStore) UpdateSyncStatus(sessionID, status, driveFolderID string) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET sync_status = ?, gdrive_folder_id = ? WHERE id = ?`,
		status, driveFolderID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("update sync status for session %s: %w", sessionID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update sync status rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteStore) GetSessionsNeedingSync() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM sessions WHERE status = 'ended' AND summary_status = 'completed' AND sync_status != 'synced'`,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions needing sync: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/storage/ -v -count=1`
Expected: ALL PASS (existing tests must still pass with the new columns)

**Step 5: Commit**

```
jj describe -m "feat(storage): add sync_status and gdrive_folder_id columns"
jj new
```

---

### Task 2: Add GC-eligible session query

**Files:**
- Modify: `internal/storage/sqlite.go`
- Modify: `internal/storage/sqlite_test.go`

**Step 1: Write the failing test**

```go
func TestGetGCEligibleSessions(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	old := now.Add(-45 * 24 * time.Hour) // 45 days ago

	// Old synced session — eligible.
	if err := store.CreateSession("gc-eligible", old); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-eligible", old.Add(time.Minute), "data/audio/gc-eligible.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSyncStatus("gc-eligible", SyncSynced, "folder-1"); err != nil {
		t.Fatal(err)
	}

	// Old unsynced session — NOT eligible.
	if err := store.CreateSession("gc-unsynced", old); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-unsynced", old.Add(time.Minute), "data/audio/gc-unsynced.mp3"); err != nil {
		t.Fatal(err)
	}

	// Recent synced session — NOT eligible (too new).
	if err := store.CreateSession("gc-recent", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession("gc-recent", now.Add(-30*time.Minute), "data/audio/gc-recent.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSyncStatus("gc-recent", SyncSynced, "folder-2"); err != nil {
		t.Fatal(err)
	}

	// Sync-gated: only old synced sessions.
	ids, err := store.GetGCEligibleSessions(30, true)
	if err != nil {
		t.Fatalf("get gc eligible: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gc-eligible" {
		t.Fatalf("expected [gc-eligible], got %v", ids)
	}

	// Non-sync-gated: old sessions regardless of sync status.
	ids, err = store.GetGCEligibleSessions(30, false)
	if err != nil {
		t.Fatalf("get gc eligible no gate: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %v", len(ids), ids)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/storage/ -run TestGetGCEligibleSessions -v -count=1`
Expected: FAIL — `GetGCEligibleSessions` doesn't exist.

**Step 3: Implement the method**

```go
// GetGCEligibleSessions returns session IDs eligible for garbage collection.
// Sessions must be ended and older than maxAgeDays.
// If syncGated is true, only synced sessions are returned.
// Results are ordered oldest first (for disk-pressure GC to delete oldest).
func (s *SQLiteStore) GetGCEligibleSessions(maxAgeDays int, syncGated bool) ([]string, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(maxAgeDays) * 24 * time.Hour).Format(time.RFC3339Nano)
	query := `SELECT id FROM sessions WHERE status = 'ended' AND started_at < ?`
	if syncGated {
		query += ` AND sync_status = 'synced'`
	}
	query += ` ORDER BY started_at ASC`

	rows, err := s.db.Query(query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query gc eligible sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return ids, nil
}
```

Also add a `DeleteSession` method for GC to use (cascading delete removes segments too thanks to foreign key):

```go
func (s *SQLiteStore) DeleteSession(id string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
```

**Step 4: Run tests**

Run: `go test ./internal/storage/ -v -count=1`
Expected: ALL PASS

**Step 5: Commit**

```
jj describe -m "feat(storage): add GC-eligible session query and DeleteSession"
jj new
```

---

### Task 3: Implement markdown rendering for sync

**Files:**
- Create: `internal/gdrive/markdown.go`
- Create: `internal/gdrive/markdown_test.go`

**Step 1: Write the failing tests**

```go
package gdrive

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestRenderSummaryMarkdown(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	md := RenderSummaryMarkdown(SyncSession{
		ID:            "20260321-143022",
		Title:         "Weekly Standup",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "Team discussed sprint progress.",
		SummaryPreset: "default",
	})

	if !strings.Contains(md, "schema_version: 1") {
		t.Error("missing schema_version")
	}
	if !strings.Contains(md, `id: "20260321-143022"`) {
		t.Error("missing id in frontmatter")
	}
	if !strings.Contains(md, "# Weekly Standup") {
		t.Error("missing title heading")
	}
	if !strings.Contains(md, "Team discussed sprint progress.") {
		t.Error("missing summary body")
	}
}

func TestRenderTranscriptMarkdown(t *testing.T) {
	started := time.Date(2026, 3, 21, 14, 30, 22, 0, time.UTC)
	ended := started.Add(32 * time.Minute)

	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Hello everyone", StartTime: 0.0, EndTime: 1.5, Timestamp: started},
		{Speaker: 1, Text: "Hi, let's begin", StartTime: 2.0, EndTime: 3.5, Timestamp: started.Add(2 * time.Second)},
	}

	md := RenderTranscriptMarkdown(SyncSession{
		ID:        "20260321-143022",
		Title:     "Weekly Standup",
		StartedAt: started,
		EndedAt:   &ended,
	}, segments)

	if !strings.Contains(md, "schema_version: 1") {
		t.Error("missing schema_version")
	}
	if !strings.Contains(md, "# Transcript") {
		t.Error("missing transcript heading")
	}
	if !strings.Contains(md, "Speaker 1") {
		t.Error("missing speaker label")
	}
	if !strings.Contains(md, "Hello everyone") {
		t.Error("missing segment text")
	}
	// Check speakers list in frontmatter.
	if !strings.Contains(md, "- \"Speaker 1\"") {
		t.Error("missing speaker in frontmatter")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/gdrive/ -run TestRender -v -count=1`
Expected: FAIL — `RenderSummaryMarkdown`, `RenderTranscriptMarkdown`, `SyncSession` don't exist.

**Step 3: Implement the rendering functions**

Create `internal/gdrive/markdown.go`:

```go
package gdrive

import (
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// SyncSession holds the session data needed for rendering markdown.
type SyncSession struct {
	ID            string
	Title         string
	StartedAt     time.Time
	EndedAt       *time.Time
	Summary       string
	SummaryPreset string
}

// RenderSummaryMarkdown produces the summary.md content with YAML frontmatter.
func RenderSummaryMarkdown(s SyncSession) string {
	var b strings.Builder

	b.WriteString("---\n")
	b.WriteString("schema_version: 1\n")
	fmt.Fprintf(&b, "id: %q\n", s.ID)
	fmt.Fprintf(&b, "started_at: %q\n", s.StartedAt.UTC().Format(time.RFC3339))
	if s.EndedAt != nil {
		fmt.Fprintf(&b, "ended_at: %q\n", s.EndedAt.UTC().Format(time.RFC3339))
	}
	if s.SummaryPreset != "" {
		fmt.Fprintf(&b, "summary_preset: %q\n", s.SummaryPreset)
	}
	b.WriteString("---\n\n")

	title := s.Title
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	b.WriteString(s.Summary)
	b.WriteString("\n")

	return b.String()
}

// RenderTranscriptMarkdown produces the transcript.md content with YAML frontmatter.
func RenderTranscriptMarkdown(s SyncSession, segments []transcribe.Segment) string {
	var b strings.Builder

	// Collect unique speakers.
	speakerSet := make(map[int]struct{})
	for _, seg := range segments {
		speakerSet[seg.Speaker] = struct{}{}
	}
	speakers := make([]int, 0, len(speakerSet))
	for sp := range speakerSet {
		speakers = append(speakers, sp)
	}

	b.WriteString("---\n")
	b.WriteString("schema_version: 1\n")
	fmt.Fprintf(&b, "id: %q\n", s.ID)
	fmt.Fprintf(&b, "started_at: %q\n", s.StartedAt.UTC().Format(time.RFC3339))
	if s.EndedAt != nil {
		fmt.Fprintf(&b, "ended_at: %q\n", s.EndedAt.UTC().Format(time.RFC3339))
	}
	if len(speakers) > 0 {
		b.WriteString("speakers:\n")
		for _, sp := range speakers {
			fmt.Fprintf(&b, "  - \"Speaker %d\"\n", sp+1)
		}
	}
	b.WriteString("---\n\n")

	b.WriteString("# Transcript\n\n")

	for _, seg := range segments {
		ts := seg.Timestamp.UTC().Format("15:04:05")
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "**[%s] Speaker %d:** %s\n\n", ts, seg.Speaker+1, text)
	}

	return b.String()
}
```

**Step 4: Run tests**

Run: `go test ./internal/gdrive/ -run TestRender -v -count=1`
Expected: PASS

**Step 5: Commit**

```
jj describe -m "feat(gdrive): add markdown rendering for summary and transcript"
jj new
```

---

### Task 4: Rewrite the GDrive syncer for per-session upload

**Files:**
- Modify: `internal/gdrive/sync.go`
- Create: `internal/gdrive/sync_test.go`

**Step 1: Write the failing test**

Test the public `SyncSession` method using a mock Drive service (or test the folder creation and file upload logic via integration-style test with a stubbed HTTP transport). Since the Drive API is external, test the orchestration logic:

```go
package gdrive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestSyncerBuildsSyncData(t *testing.T) {
	// Test that BuildSyncFiles produces the expected file set.
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

	files, folderName, err := BuildSyncFiles(sess, segments, audioPath)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}

	if folderName != "2026-03-21-weekly-standup" {
		t.Errorf("expected folder name %q, got %q", "2026-03-21-weekly-standup", folderName)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	// Verify file names and MIME types.
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
	if _, ok := names["audio.mp3"]; !ok {
		t.Error("missing audio.mp3")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/gdrive/ -run TestSyncerBuilds -v -count=1`
Expected: FAIL — `BuildSyncFiles` doesn't exist.

**Step 3: Implement the new syncer**

Rewrite `internal/gdrive/sync.go` to:

1. Keep `NewSyncer` constructor (same credentials/auth pattern).
2. Replace the old `Sync(localPath, date)` with:
   - `BuildSyncFiles(sess, segments, audioPath)` — pure function that produces the file set and folder name.
   - `Upload(ctx, folderName string, files []SyncFile)` — creates the folder on Drive and uploads files.
3. `SyncFile` struct: `Name string`, `MimeType string` (target MIME on Drive), `Content []byte` or `LocalPath string` (for audio).
4. Folder creation: use `service.Files.Create` with `MimeType: "application/vnd.google-apps.folder"`.
5. Markdown upload: set `MimeType: "application/vnd.google-apps.document"` on the Drive `File` metadata — the Drive API auto-converts when the target MIME is a Google Workspace type.
6. Audio upload: set appropriate MIME type (`audio/mpeg` for MP3, `audio/wav` for WAV).
7. Folder slug: `YYYY-MM-DD-<slugified-title>` — lowercase, spaces to hyphens, strip non-alphanumeric.
8. Return the created folder's Drive ID (for sync tracking).

**Step 4: Run tests**

Run: `go test ./internal/gdrive/ -v -count=1`
Expected: PASS

**Step 5: Commit**

```
jj describe -m "feat(gdrive): rewrite syncer for per-session upload with folder structure"
jj new
```

---

### Task 5: Add sync and GC config fields

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Step 1: Write the failing test**

```go
func TestSyncAndGCDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.GDriveSyncEnabled {
		t.Error("gdrive_sync_enabled should default to false")
	}
	if cfg.GCEnabled {
		t.Error("gc_enabled should default to false")
	}
	if cfg.GCMaxAgeDays != 30 {
		t.Errorf("expected gc_max_age_days 30, got %d", cfg.GCMaxAgeDays)
	}
	if cfg.GCMaxAudioSizeMB != 1024 {
		t.Errorf("expected gc_max_audio_size_mb 1024, got %d", cfg.GCMaxAudioSizeMB)
	}
}

func TestSyncAndGCEnvOverrides(t *testing.T) {
	t.Setenv(EnvPrefix+"GDRIVE_SYNC_ENABLED", "true")
	t.Setenv(EnvPrefix+"GC_ENABLED", "true")
	t.Setenv(EnvPrefix+"GC_MAX_AGE_DAYS", "60")
	t.Setenv(EnvPrefix+"GC_MAX_AUDIO_SIZE_MB", "512")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GDriveSyncEnabled {
		t.Error("expected gdrive_sync_enabled true")
	}
	if !cfg.GCEnabled {
		t.Error("expected gc_enabled true")
	}
	if cfg.GCMaxAgeDays != 60 {
		t.Errorf("expected gc_max_age_days 60, got %d", cfg.GCMaxAgeDays)
	}
	if cfg.GCMaxAudioSizeMB != 512 {
		t.Errorf("expected gc_max_audio_size_mb 512, got %d", cfg.GCMaxAudioSizeMB)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestSyncAndGC -v -count=1`
Expected: FAIL — fields don't exist.

**Step 3: Add config fields**

In `internal/config/config.go`:

1. Add to `Config` struct:
```go
GDriveSyncEnabled bool `yaml:"gdrive_sync_enabled"`
GCEnabled         bool `yaml:"gc_enabled"`
GCMaxAgeDays      int  `yaml:"gc_max_age_days"`
GCMaxAudioSizeMB  int  `yaml:"gc_max_audio_size_mb"`
```

2. Add defaults:
```go
GCMaxAgeDays:     30,
GCMaxAudioSizeMB: 1024,
```

3. Add env overrides in `applyEnvOverrides`:
```go
if v := os.Getenv(EnvPrefix + "GDRIVE_SYNC_ENABLED"); v != "" {
	cfg.GDriveSyncEnabled = strings.EqualFold(v, "true") || v == "1"
}
if v := os.Getenv(EnvPrefix + "GC_ENABLED"); v != "" {
	cfg.GCEnabled = strings.EqualFold(v, "true") || v == "1"
}
if v := os.Getenv(EnvPrefix + "GC_MAX_AGE_DAYS"); v != "" {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		cfg.GCMaxAgeDays = n
	}
}
if v := os.Getenv(EnvPrefix + "GC_MAX_AUDIO_SIZE_MB"); v != "" {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		cfg.GCMaxAudioSizeMB = n
	}
}
```

4. Add validation in `validate`:
```go
if cfg.GCMaxAgeDays <= 0 {
	warnings = append(warnings, "gc_max_age_days must be positive — using default 30.")
	cfg.GCMaxAgeDays = 30
}
if cfg.GCMaxAudioSizeMB <= 0 {
	warnings = append(warnings, "gc_max_audio_size_mb must be positive — using default 1024.")
	cfg.GCMaxAudioSizeMB = 1024
}
```

**Step 4: Run tests**

Run: `go test ./internal/config/ -v -count=1`
Expected: ALL PASS

**Step 5: Update the example config file**

In `ghost-wispr.yaml.example`, add after the existing gdrive section:

```yaml
# Google Drive sync
# gdrive_folder_id:
# google_credentials_file: ./service-account.json
# gdrive_sync_enabled: false

# Garbage collection
# gc_enabled: false
# gc_max_age_days: 30
# gc_max_audio_size_mb: 1024
```

**Step 6: Commit**

```
jj describe -m "feat(config): add gdrive sync and garbage collection config fields"
jj new
```

---

### Task 6: Implement the GC engine

**Files:**
- Create: `internal/gc/gc.go`
- Create: `internal/gc/gc_test.go`

**Step 1: Write the failing test**

```go
package gc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mockStore struct {
	gcEligible    []sessionInfo
	deletedIDs    []string
	deleteErr     error
}

type sessionInfo struct {
	id        string
	audioPath string
}

func (m *mockStore) GetGCEligibleSessions(maxAgeDays int, syncGated bool) ([]string, error) {
	ids := make([]string, len(m.gcEligible))
	for i, s := range m.gcEligible {
		ids[i] = s.id
	}
	return ids, nil
}

func (m *mockStore) GetSession(id string) (sessionInfo, error) {
	for _, s := range m.gcEligible {
		if s.id == id {
			return s, nil
		}
	}
	return sessionInfo{}, os.ErrNotExist
}

func (m *mockStore) DeleteSession(id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deletedIDs = append(m.deletedIDs, id)
	return nil
}

func TestGCDeletesSyncedOldSessions(t *testing.T) {
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(audioPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &mockStore{
		gcEligible: []sessionInfo{
			{id: "s1", audioPath: audioPath},
		},
	}

	collector := New(store, Config{
		MaxAgeDays:     30,
		MaxAudioSizeMB: 1024,
		SyncGated:      true,
	})

	deleted, err := collector.Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "s1" {
		t.Errorf("expected [s1] deleted, got %v", deleted)
	}
	if _, err := os.Stat(audioPath); !os.IsNotExist(err) {
		t.Error("expected audio file to be deleted")
	}
	if len(store.deletedIDs) != 1 {
		t.Error("expected store.DeleteSession called")
	}
}
```

Note: The actual `gc.Store` interface will reference `storage.Session`. The mock above is simplified for the plan — the real test will use the actual `storage.Session` type. Adjust the interface to match during implementation.

**Step 2: Run test to verify it fails**

Run: `go test ./internal/gc/ -run TestGC -v -count=1`
Expected: FAIL — package doesn't exist.

**Step 3: Implement the GC engine**

Create `internal/gc/gc.go`:

```go
package gc

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type Store interface {
	GetGCEligibleSessions(maxAgeDays int, syncGated bool) ([]string, error)
	GetSession(id string) (storage.Session, error)
	DeleteSession(id string) error
}

type Config struct {
	MaxAgeDays     int
	MaxAudioSizeMB int
	SyncGated      bool
	AudioDir       string
}

type Collector struct {
	store  Store
	config Config
}

func New(store Store, config Config) *Collector {
	return &Collector{store: store, config: config}
}

func (c *Collector) Run() ([]string, error) {
	ids, err := c.store.GetGCEligibleSessions(c.config.MaxAgeDays, c.config.SyncGated)
	if err != nil {
		return nil, fmt.Errorf("query gc eligible: %w", err)
	}

	// Check disk pressure: if audio dir exceeds max size, we'll GC even
	// sessions within the age threshold. For now, age-based only in this pass.
	// Disk-pressure GC uses the same sorted-oldest-first list.
	diskPressure := c.checkDiskPressure()

	var deleted []string
	for _, id := range ids {
		sess, err := c.store.GetSession(id)
		if err != nil {
			log.Printf("gc: skip session %s: %v", id, err)
			continue
		}

		// Delete audio file if it exists.
		if sess.AudioPath != "" {
			audioPath := sess.AudioPath
			if !filepath.IsAbs(audioPath) && c.config.AudioDir != "" {
				// audio_path may be relative; resolve against audio_dir parent.
			}
			if err := os.Remove(audioPath); err != nil && !os.IsNotExist(err) {
				log.Printf("gc: failed to remove audio %s: %v", audioPath, err)
				continue // Don't delete DB row if audio deletion failed.
			}
		}

		// Delete session from DB (cascades to segments).
		if err := c.store.DeleteSession(id); err != nil {
			log.Printf("gc: failed to delete session %s: %v", id, err)
			continue
		}

		deleted = append(deleted, id)

		// If we were in disk pressure mode but now under the limit, stop.
		if diskPressure && !c.checkDiskPressure() {
			break
		}
	}

	return deleted, nil
}

func (c *Collector) checkDiskPressure() bool {
	if c.config.MaxAudioSizeMB <= 0 || c.config.AudioDir == "" {
		return false
	}
	size, err := dirSize(c.config.AudioDir)
	if err != nil {
		return false
	}
	return size > int64(c.config.MaxAudioSizeMB)*1024*1024
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
```

**Step 4: Run tests**

Run: `go test ./internal/gc/ -v -count=1`
Expected: PASS

**Step 5: Commit**

```
jj describe -m "feat(gc): implement garbage collection engine"
jj new
```

---

### Task 7: Wire sync and GC into main.go

**Files:**
- Modify: `cmd/ghost-wispr/main.go`
- Modify: `internal/session/types.go`
- Modify: `internal/session/manager.go`

**Step 1: Add a Syncer interface to session types**

In `internal/session/types.go`, add:

```go
type SessionSyncer interface {
	SyncSession(ctx context.Context, sessionID string) error
}
```

**Step 2: Add syncer field to Manager and trigger after summarization**

In `internal/session/manager.go`:

1. Add `syncer SessionSyncer` field to the `Manager` struct.
2. Add `SetSyncer(s SessionSyncer)` method.
3. At the end of `generateSummary`, after successful summarization (status == `SummaryCompleted`), trigger sync:
```go
if m.syncer != nil {
	go m.syncer.SyncSession(context.Background(), sessionID)
}
```

**Step 3: Wire everything in main.go**

Replace the existing gdrive ticker block (lines 489-511) with:

1. Create a `SyncOrchestrator` that wraps the gdrive `Syncer` and storage store — responsible for:
   - Fetching session + segments from store
   - Calling `BuildSyncFiles`
   - Calling `syncer.Upload`
   - Calling `store.UpdateSyncStatus`
2. Pass the orchestrator to `manager.SetSyncer()` for event-driven sync.
3. Start a periodic sweep goroutine (5 min) that calls `store.GetSessionsNeedingSync()` and syncs each.
4. Start a GC sweep goroutine (1 hour) that creates a `gc.Collector` with current config and calls `Run()`.
5. Register `cfgStore.OnChange` to update sync/GC config dynamically.

**Step 4: Run all tests**

Run: `go test ./... -count=1`
Expected: ALL PASS

**Step 5: Commit**

```
jj describe -m "feat: wire gdrive sync and GC into session lifecycle and main loop"
jj new
```

---

### Task 8: Expose sync and GC config in the API

**Files:**
- Modify: `internal/server/api.go`
- Modify: `internal/server/api_test.go`

**Step 1: Write the failing test**

Add a test that GETs config and verifies the new fields are present:

```go
func TestConfigIncludesSyncAndGC(t *testing.T) {
	// Use existing test pattern — build a stub, call handleGetConfig.
	// Verify response includes gdrive.sync_enabled, gc.enabled, gc.max_age_days, gc.max_audio_size_mb.
}
```

Also test PATCH with the new fields.

**Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestConfigIncludesSyncAndGC -v -count=1`
Expected: FAIL

**Step 3: Implement API changes**

1. Update `configGDriveResponse` to include:
```go
SyncEnabled bool `json:"sync_enabled"`
```

2. Add `configGCResponse` struct:
```go
type configGCResponse struct {
	Enabled        bool `json:"enabled"`
	MaxAgeDays     int  `json:"max_age_days"`
	MaxAudioSizeMB int  `json:"max_audio_size_mb"`
}
```

3. Add `GC configGCResponse` to `configResponse`.

4. Update `handleGetConfig` to populate the new fields from config.

5. Add `gcPatch` struct:
```go
type gcPatch struct {
	Enabled        *bool `json:"enabled,omitempty"`
	MaxAgeDays     *int  `json:"max_age_days,omitempty"`
	MaxAudioSizeMB *int  `json:"max_audio_size_mb,omitempty"`
}
```

6. Add `GC *gcPatch` to `configPatch`.

7. Update `gdrivePatch` to include:
```go
SyncEnabled *bool `json:"sync_enabled,omitempty"`
```

8. Update `applyConfigPatch` to handle the new fields with validation (positive integers for thresholds).

**Step 4: Run tests**

Run: `go test ./internal/server/ -v -count=1`
Expected: ALL PASS

**Step 5: Commit**

```
jj describe -m "feat(api): expose sync and GC config in GET/PATCH endpoints"
jj new
```

---

### Task 9: Update the web UI — GDrive sync toggle

**Files:**
- Modify: `web/src/lib/config-api.ts`
- Modify: `web/src/components/IntegrationsSettings.svelte`

**Step 1: Update TypeScript types**

In `config-api.ts`, add to the config response type:

```typescript
gdrive: {
	folder_id: string
	has_credentials: boolean
	sync_enabled: boolean    // new
}
gc: {                        // new
	enabled: boolean
	max_age_days: number
	max_audio_size_mb: number
}
```

**Step 2: Add sync toggle to IntegrationsSettings.svelte**

In the existing GDrive section, add a toggle/checkbox for sync enable/disable:

```svelte
<div class="field">
	<label>
		<input type="checkbox" bind:checked={syncEnabled} onchange={saveSyncEnabled} />
		Enable automatic sync
	</label>
</div>
```

The save function follows the same pattern as `saveGDrive` — calls `patchConfig({ gdrive: { sync_enabled: syncEnabled } })`.

**Step 3: Run frontend tests**

Run: `cd web && npm test`
Expected: PASS (update existing config mock to include new fields)

**Step 4: Commit**

```
jj describe -m "feat(web): add GDrive sync toggle to integrations settings"
jj new
```

---

### Task 10: Update the web UI — GC settings section

**Files:**
- Modify: `web/src/components/IntegrationsSettings.svelte` (or create a new `GCSettings.svelte` component)
- Modify: `web/src/components/SettingsPage.svelte`

**Step 1: Add GC settings UI**

Add a new section (below GDrive) with:
- Enable/disable toggle
- Max age (days) number input
- Max disk size (MB) number input
- Warning banner when GC is enabled but GDrive sync is not

Follow the `GeneralSettings.svelte` pattern: local state, `$effect` to sync with config, save function that builds a minimal patch.

```svelte
<section class="gc-section">
	<h3>Garbage Collection</h3>

	{#if gcEnabled && !config.gdrive.sync_enabled}
		<div class="warning-banner">
			⚠️ Garbage collection is deleting files without a Google Drive backup configured.
		</div>
	{/if}

	<div class="field">
		<label>
			<input type="checkbox" bind:checked={gcEnabled} />
			Enable garbage collection
		</label>
	</div>

	{#if gcEnabled}
		<div class="field">
			<label for="gc-max-age">Delete sessions older than (days)</label>
			<input id="gc-max-age" type="number" min="1" bind:value={gcMaxAgeDays} />
		</div>
		<div class="field">
			<label for="gc-max-size">Max audio storage (MB)</label>
			<input id="gc-max-size" type="number" min="100" bind:value={gcMaxAudioSizeMB} />
		</div>
	{/if}

	<div class="actions">
		<button class="save-btn" type="button" onclick={saveGC} disabled={gcSaving}>
			{gcSaving ? 'Saving...' : 'Save'}
		</button>
		{#if gcFeedback}
			<span class="feedback">{gcFeedback}</span>
		{/if}
	</div>
</section>
```

**Step 2: Run frontend tests**

Run: `cd web && npm test`
Expected: PASS

**Step 3: Commit**

```
jj describe -m "feat(web): add garbage collection settings to web UI"
jj new
```

---

### Task 11: Integration test — full sync flow

**Files:**
- Create: `internal/gdrive/integration_test.go` (build-tagged, skipped without credentials)

**Step 1: Write an integration test**

This test is `//go:build integration` tagged so it only runs manually with real credentials:

```go
//go:build integration

package gdrive

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func TestSyncToRealDrive(t *testing.T) {
	credPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	folderID := os.Getenv("GDRIVE_TEST_FOLDER_ID")
	if credPath == "" || folderID == "" {
		t.Skip("GOOGLE_APPLICATION_CREDENTIALS and GDRIVE_TEST_FOLDER_ID required")
	}

	ctx := context.Background()
	syncer, err := NewSyncer(ctx, credPath, folderID)
	if err != nil {
		t.Fatalf("create syncer: %v", err)
	}

	started := time.Now().UTC()
	ended := started.Add(5 * time.Minute)

	sess := SyncSession{
		ID:            "integration-test-" + started.Format("20060102-150405"),
		Title:         "Integration Test Session",
		StartedAt:     started,
		EndedAt:       &ended,
		Summary:       "This is an integration test summary.",
		SummaryPreset: "default",
	}
	segments := []transcribe.Segment{
		{Speaker: 0, Text: "Integration test segment", StartTime: 0.0, EndTime: 1.0, Timestamp: started},
	}

	// Create a fake audio file.
	tmpAudio := t.TempDir() + "/test.mp3"
	if err := os.WriteFile(tmpAudio, []byte("fake-audio-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, folderName, err := BuildSyncFiles(sess, segments, tmpAudio)
	if err != nil {
		t.Fatalf("build sync files: %v", err)
	}

	driveFolderID, err := syncer.Upload(ctx, folderName, files)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	t.Logf("Created Drive folder: %s (ID: %s)", folderName, driveFolderID)
}
```

**Step 2: Run unit tests (confirm nothing broke)**

Run: `go test ./... -count=1`
Expected: ALL PASS (integration test is skipped by default)

**Step 3: Commit**

```
jj describe -m "test(gdrive): add integration test for real Drive upload"
jj new
```

---

### Task 12: End-to-end verification

**Step 1: Run all backend tests**

Run: `go test ./... -count=1 -v`
Expected: ALL PASS

**Step 2: Run all frontend tests**

Run: `cd web && npm test`
Expected: ALL PASS

**Step 3: Run linter**

Run: `golangci-lint run ./...`
Expected: PASS (or only pre-existing warnings)

**Step 4: Verify config example**

Run: `cat ghost-wispr.yaml.example`
Verify: New fields are documented with comments.

**Step 5: Commit final state**

```
jj describe -m "feat: Google Drive sync and garbage collection — complete"
jj new
```
