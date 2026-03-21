# Google Drive Sync & Local Garbage Collection

## Overview

Add automatic Google Drive backup for session data (summaries, transcripts, audio) and local garbage collection to manage disk usage. All settings are configurable from the web UI.

## Google Drive Sync

### What gets synced

Each completed session produces three files uploaded to Google Drive:

| File | Format on Drive | Content |
|------|----------------|---------|
| `summary.md` | Google Doc (converted on upload) | YAML frontmatter + AI-generated title and summary |
| `transcript.md` | Google Doc (converted on upload) | YAML frontmatter + full timestamped, speaker-labeled transcript |
| `audio.mp3` | MP3 (as-is) | Session audio recording |

### Folder structure

```
<configured-root-folder>/
  2026-03-18-weekly-standup/
    summary.md
    transcript.md
    audio.mp3
  2026-03-18-design-review/
    summary.md
    transcript.md
    audio.mp3
```

Each session gets its own folder. The folder name is `<date>-<slugified-title>`. Date prefix keeps them chronologically sortable when scrolling.

### Markdown format

Both markdown files use YAML frontmatter with structured metadata sufficient to reconstruct the session in SQLite.

**`summary.md`:**

```markdown
---
schema_version: 1
id: "20260318-143022"
started_at: "2026-03-18T14:30:22Z"
ended_at: "2026-03-18T15:02:11Z"
summary_preset: "default"
---

# Weekly Standup

Team discussed sprint progress. Key decisions: move deadline to Friday.
Action items: Alice to update the spec, Bob to review PR #42.
```

**`transcript.md`:**

```markdown
---
schema_version: 1
id: "20260318-143022"
started_at: "2026-03-18T14:30:22Z"
ended_at: "2026-03-18T15:02:11Z"
speakers:
  - "Speaker 1"
  - "Speaker 2"
---

# Transcript

**[14:30:22] Speaker 1:** Hello everyone, let's get started.

**[14:30:25] Speaker 2:** Hi, I've got updates on the sprint.
```

The `schema_version` field allows a future restore tool to handle format changes without migration issues.

### Sync triggers

Dual-path for reliability:

1. **Event-driven (primary):** When a session ends and summarization completes, sync that session immediately.
2. **Periodic sweep (safety net):** Every 5 minutes, query for sessions where `status = 'ended'` and `sync_status != 'synced'`. Catches restarts, transient failures, and any missed events.

### Sync tracking

New columns on the `sessions` table:

- `sync_status` — `pending` | `synced` | `failed`
- `gdrive_folder_id` — the Drive folder ID for this session's folder (for future restore/management)

Sessions start as `pending`. On successful upload of all three files, marked `synced`. On failure, marked `failed` (retry on next sweep).

## Garbage Collection

### What gets deleted

When a session qualifies for GC, both are removed in the same pass:
- The local audio file (MP3/WAV)
- The SQLite rows (session record, transcript segments, summary)

This is a clean "this session lives on Drive now" model. The web UI shows empty state for GC'd dates. Restore from Drive brings everything back.

### Deletion criteria

A session is eligible for GC when **both conditions are true**:

1. **Sync-gated:** `sync_status = 'synced'` (confirmed backed up to Drive)
2. **Threshold exceeded (either one):**
   - Session is older than `gc_max_age_days` (default: 30)
   - Total `audio_dir` disk usage exceeds `gc_max_audio_size_mb` (default: 1024). Oldest synced sessions are deleted first until under the limit.

### When GDrive sync is not configured

GC still runs but **skips the sync-gate check**. A prominent warning banner is displayed in the web UI: "Garbage collection is deleting files without a Google Drive backup configured."

### GC schedule

Runs on a periodic sweep (e.g., every hour). Not triggered on session end.

## Configuration

All new settings are editable from the web UI, with YAML and environment variable support for bootstrap.

### New config fields

```yaml
# Google Drive sync
gdrive_folder_id: ""                # existing
google_credentials_file: ""         # existing
gdrive_sync_enabled: false          # new — master toggle for sync

# Garbage collection
gc_enabled: false                   # new — master toggle for GC
gc_max_age_days: 30                 # new — delete synced sessions older than this
gc_max_audio_size_mb: 1024          # new — delete oldest synced sessions when audio dir exceeds this
```

### Web UI changes

**Integrations settings panel (existing GDrive section):**
- Add sync enable/disable toggle
- Existing: folder ID input, credential upload

**New GC section in settings:**
- Enable/disable toggle
- Max age (days) input
- Max disk size (MB) input
- Warning banner when GC is enabled but GDrive sync is not configured

## Restore (future, not in this pass)

The sync format is designed to support a future restore flow:
- Download session folders from Drive
- Parse YAML frontmatter from `summary.md` and `transcript.md`
- Rehydrate SQLite session rows, segment rows, and summary data
- Copy audio files back to `audio_dir`

The `schema_version` field in frontmatter ensures forward compatibility. Not implementing now — shipping backup first will inform the restore design.

## Existing code changes

### `internal/gdrive/sync.go`

Currently uploads the SQLite DB file as a Google Doc every 5 minutes. Will be reworked to:
- Upload per-session files (summary.md, transcript.md, audio)
- Create per-session folders on Drive
- Convert markdown files to Google Docs on upload
- Track sync status per session

### `internal/config/config.go`

Add new config fields: `gdrive_sync_enabled`, `gc_enabled`, `gc_max_age_days`, `gc_max_audio_size_mb`.

### `internal/storage/sqlite.go`

Add `sync_status` and `gdrive_folder_id` columns to sessions table. Add queries for GC-eligible sessions and sync-pending sessions.

### `internal/server/api.go`

Expose new config fields in the GET/PATCH config endpoints.

### Web UI (`web/src/components/IntegrationsSettings.svelte`)

Add sync toggle to existing GDrive section. Add new GC settings section.
