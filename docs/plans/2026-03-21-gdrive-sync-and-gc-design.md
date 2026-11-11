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
| `audio-<recording file name>` | MP3 or WAV (as-is) | Session audio recording, one file per recording (a merged session has several); folders synced before this naming have `audio.mp3` |

### Folder structure

```
<configured-root-folder>/
  2026-03-18-weekly-standup/
    summary.md
    transcript.md
    audio-20260318100000.mp3
  2026-03-18-design-review/
    summary.md
    transcript.md
    audio-20260318140000.mp3
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
2. **Periodic sweep (safety net):** Every 5 minutes, sync the ended sessions whose `sync_state` is `PENDING`, and those `RETRY_SCHEDULED` whose retry is due. Catches restarts, transient failures, documents changed after a sync, and any missed events.

### Sync tracking

New columns on the `sessions` table:

- `sync_status` — `pending` | `synced` | `failed`
- `sync_state` — `PENDING` | `SYNCING` | `SYNCED` | `RETRY_SCHEDULED` | `FAILED` | `REMOTE_DELETED`, with `retry_count` and `last_sync_attempt` for the backoff
- `sync_claim` — the claim of the sync that holds the session while it is `SYNCING`
- `documents_unverified` — 1 while Drive may lack the session's current summary and transcript though it is synced (below); the age policy never deletes such a session
- `error_message` — why the last sync failed; `summary_error` — why the last summary attempt failed, cleared when one completes. Earlier versions wrote summary failures into `error_message`; the first time this version opens a database (the `move-summary-failures-to-summary-error` migration) those messages move to `summary_error`, unless a summary has since completed (then they go) or `summary_error` already holds a later reason (then that stays)
- `sync_claimed_from` — while a sync holds the session, the sync state its claim came from (below)
- `gdrive_folder_id` — the Drive folder ID for this session's folder (for future restore/management)

Sessions start as `pending`. On successful upload of all its files, marked `synced`. On failure, retried with backoff by the sweep; once the retries run out, `failed`, and a manual retry (`POST /api/sessions/{id}/retry-sync`, the UI's retry button) starts again from there. The sweep runs whether sync is on or not: it syncs nothing while sync is off and picks up a syncer turned on in the settings.

**A folder deleted on Drive.** A sync into the session's own folder that Drive answers with 404 for the folder (the error seen through the Syncer's wrapping), or that finds the folder in the Drive trash, ends `REMOTE_DELETED`, keeping the folder's id. The Syncer checks the folder before and after a re-sync and after a first sync, so nothing is uploaded into a folder in the trash and a session is never marked synced from one. Drive also answers 404 for a file the app can no longer read, so a 404 counts as the folder gone only while the folder the app syncs into still reads; otherwise access to Drive is lost and the sync is retried. That folder reads when `files.get` answers for it (a folder in My Drive), or when a list of the shared drive it is the root of (`corpora=drive` with its id as `driveId`) answers: under the app's `drive.file` scope `files.get` answers 404 for a shared drive's root, since the app did not create it, and the list answers 404 "Shared drive not found" for an id Drive does not have, as it is taken to for a drive the app is no longer a member of (not measured: only the drive's Manager can remove the app). A list of the root's children without `corpora` cannot tell these apart: it answers 200 with no files for an id Drive does not have too (all three measured through the app on the office Pi on 2026-11-11). A 404 for the folder's list or for one file during an upload (it went since the folder was checked) asks Drive about the folder again: only the folder gone ends `REMOTE_DELETED`, and a file alone gone is retried, the retry making it again. The sweep does not sync a `REMOTE_DELETED` session, and no folder is ever made again for it. A retry (retry-sync, the UI's retry button, or the sync after a summary) asks Drive about the folder before it holds the session: while the folder is still in the trash or gone, or Drive does not answer, it changes nothing and the session stays `REMOTE_DELETED`, and retry-sync answers with why (the card shows it): 409 for a folder still in the trash or gone, 503 when Drive did not answer; once the user restores the folder, the retry holds the session and syncs it into the folder. If the app stops while that retry holds the session, startup recovery puts it back to `REMOTE_DELETED` (its claim came from there: `sync_claimed_from`), not to a retry, since Drive may have lost the folder again; the next retry asks Drive again. So neither a Drive outage nor the app stopping during a retry moves the session out of `REMOTE_DELETED` while its folder is not back. The session keeps its local copy: no GC path deletes it or removes its audio, and the session card says "Sync: deleted on Drive". A 404 while a first sync creates the session's folder is about the folder the app syncs into (the `gdrive_folder_id` setting: unshared or mistyped, or a shared drive the app is no longer a member of), not about the session, and is retried like any other failure, so a manual retry works once that folder is back. A first sync does not make a folder in a root in the trash; only a folder in My Drive can be in the trash, since a shared drive's root cannot.

**Which folder the app can sync into.** Under `drive.file` the folder the app syncs into must read in one of those two ways: a shared drive's id (the drive's root), or a folder in My Drive that `files.get` answers for, which under `drive.file` is one the app created. A folder that the app did not create, such as one a person made inside a shared drive or in My Drive and shared with the app, is expected to read as neither (not measured), so every 404 under it would count as access to Drive lost and GC would hold. A shared drive's root reads through the app's membership of the drive, not through the files the app made there. So if the app ever signed in as another account made a member of the drive, the drive would read while every folder the old account made answered 404 (not measured), and each such session would be taken as deleted on Drive: `REMOTE_DELETED`, kept on the unit with its audio. So the app keeps its one service account when the unit is rebuilt.

**One sync of a session at a time.** A sync first claims its session: in one statement it moves it to `SYNCING` under a claim of its own (`sync_claim`), only from the state it read and never from `SYNCING` (`ClaimSync`), and records that state (`sync_claimed_from`). A second sync of the session (retry-sync, the sync after a summary, the sweep) finds it claimed and starts nothing, and retry-sync answers 409, so two syncs never create two folders. Every state the sync writes afterwards (`MarkSynced`, and `EndSync` for a change during the upload, a retry, a failure or a deleted folder) applies only while the session is still `SYNCING` under its claim, so a sync that no longer holds its session never replaces what the session is now. A failure ends in one write (`SYNCING` to `RETRY_SCHEDULED`, or `FAILED` once the retries ran out), and so does an error while marking a session synced: the session goes to a retry with its folder kept, instead of staying `SYNCING`. A sync cut off by the app stopping, or one whose closing write fails too, leaves its session `SYNCING`; when the app starts, every such session goes to a retry (`RecoverInterruptedSyncs`), its folder kept, the cut-off sync counted as a failed attempt, except one claimed from `REMOTE_DELETED`, which goes back to `REMOTE_DELETED` with its retry count as it was. A session restored from Drive is stored `SYNCED`; one an earlier version restored with no sync state gets it once (`restored-sessions-synced`).

**A change after a sync goes to Drive.** A synced session's documents are its summary and transcript as a sync renders them, from its title, times, summary, preset and canonical transcript. A write that changes one of these (a summary made again, a title edited, a refinement retried) sends a synced session back to `pending` in the same statement (the `sessions_documents_changed` trigger), keeping its folder, so the sweep updates its documents there in place. Making a summary changes none of them until the new one is stored: while it runs, and when it fails, the session keeps its title and its last good summary (`StartSummary`, `FailSummary`), and a failure is recorded as `summary_status` `failed` and `summary_error`. A summary that comes back empty (an empty transcript) never replaces the one the session has. A change during a sync sends the session back to `pending` too: its sync sees the change when it marks the session (below), ends in `PENDING` with its retry count, and the next sweep uploads it as it is now. So from this version on, `synced` means Drive has the session's current documents, which is what GC's Drive check relies on.

**Sessions synced by an earlier version.** An earlier version left a session `synced` when its documents changed after its sync (it synced while its summary was running, or was summarized again, refined again or retitled), and nothing records what it uploaded. So the first time this version opens a database (the `mark-synced-documents-unverified` migration, run once and recorded in the `migrations` table), every `synced` session gets `documents_unverified = 1`; its sync state and folder stay as they are, and nothing is uploaded for it. The age policy never deletes a session so marked. Its audio is on Drive: the audio budget removes it once Drive answers that the session's folder is there out of the trash and that it holds every local recording with matching content; with sync off there is no Drive to ask, so the pass holds and the audio is kept. `GET /api/status` reports how many sessions are so marked (`unverified_sessions`, and a warning while there are any). The mark goes when a sync of the session completes: the next time its documents change, the trigger above sends it to sync, and marking it synced clears the mark. A session whose documents never change again stays marked, and kept, until a re-sync of its documents alone verifies it, which this version does not have.

## Garbage Collection

### What gets deleted

GC has two independent rules.

**Age (`gc_max_age_days`, the user's retention policy):** a session older than this is deleted in one pass:
- The local audio file (MP3/WAV), unless another session names it, and a raw `.pcm` beside it only once the saved recording is removed and no session names the `.pcm`
- The SQLite rows (session record, transcript segments, summary)

This is a clean "this session lives on Drive now" model. The web UI shows empty state for GC'd dates. Restore from Drive brings back the session, its transcript and its summary (its audio stays on Drive), and a restored session's age runs from its restore (`restored_at`), so the next pass does not delete it again; it is deleted `gc_max_age_days` after the restore.

**Audio budget (`gc_max_audio_size_mb`):** a budget for the audio GC may remove, not for the whole `audio_dir`. It removes audio files only, never a session, transcript or summary. The removable audio is the saved recordings of ended sessions (synced ones only; a session whose sync failed, permanently or not, keeps its file; not while a refinement reads it), plus a raw `.pcm` beside a saved recording. When those files together exceed the budget, GC removes them oldest session first until they fit or none are left. A file whose removal frees nothing (missing, empty, or refused) is skipped and logged.

GC records a removal before it removes anything, so a session never names a file GC removed. In one transaction it checks that the session still has the audio path it listed and may still have it removed (ended, no refinement running, synced), then takes out of the audio path the files it will remove, sets `audio_removed_at` (the UI shows "Audio removed to free space"), and marks a pending or failed refinement `no_audio` when no recording is left on disk. It takes out nothing else: an entry whose file it cannot see stays named, since that file may be on a disk not mounted for the moment, and a name dropped cannot be recovered. Only then does it remove the files. One that does not go is named in the row again (all of them: the row is as it was). If the transaction fails or the session changed, nothing is removed. When only part of a merged session's audio goes, the session names what is left and the UI says "Some audio removed to free space" and how many recordings are left. A refinement marks itself running only while the session still names the recording it will read (`ClaimRefinement`), so it never reads a file GC is removing and does not start on one GC removed.

Not counted against the budget, and never removed by GC: a raw `.pcm` that is the only copy of a session's audio (a recording that could not be saved, or a session the app stopped inside), the recording under way, unsynced audio, files no session names, and a file another session names too (a merge gives its sources' recordings to the merged session, so only an imported or hand-edited row makes one). `GET /api/status` reports the kept raw recordings (`kept_raw_recordings`, `kept_raw_bytes`, and a warning once they exceed a tenth of the budget, or as soon as there is one with no budget); unsynced audio is reported by the sync status. Neither is bounded by the budget, so the card can still fill with them.

**No limit.** Each limit's "no limit" is 0: `gc_max_age_days: 0` deletes no session for its age, `gc_max_audio_size_mb: 0` removes no audio to free space. A negative value in the config file is a mistake and becomes the default (30 days, 1024 MB) with a startup warning; `PATCH /api/config` refuses one. No value can wrap: the age cutoff is computed in days (a limit reaching back past year 1 matches no session), and the budget in bytes saturates at the largest `int64`.

**Refinements and missing files.** GC removes nothing a refinement is reading: the age policy skips a session whose refinement is running, and disk pressure refuses it in its transaction. A refinement left running by a stop of the app is marked `failed` when the app starts (it offers a retry), so it does not hold GC off for good. GC changes an audio path only for a file it removes itself: a session whose recordings are all missing from disk is left as it is, and an entry whose file it cannot see stays named beside one it removes, because a missing file may sit on a disk that is not mounted. An entry counts as naming no file (for `no_audio`) only when neither its path in `audio_dir` nor its own stored path has one.

**Drive sync of several recordings.** A merged session's audio path names several recordings; sync uploads each, named `audio-` and its own file name (`audio-20261106100000.mp3`), a single recording too. A name in the folder therefore identifies one recording, whatever the audio path names before or after: named by position (`audio-1`, `audio-2`, an earlier version of this change), a recording took another's name once the audio path shrank, and a re-sync took it as uploaded. Folders synced before keep their `audio.mp3` (a restore finds audio by its MIME type, not its name); a re-sync of such a folder (the migration below, or a change to the session's documents) uploads the recording again under its new name while it is still on disk, which for a merge target that `main` had synced with one recording means that recording a second time. Earlier versions stat'ed the joined path, uploaded no audio and still marked the session synced, so GC could remove audio Drive never had. The first time the store opens with this version (recorded in the `migrations` table), synced merged sessions go back to pending sync: those naming several recordings, and merge targets disk pressure had cut down to one. A session synced before keeps its Drive folder (`gdrive_folder_id`), and so does one whose first sync created its folder and then failed; syncing it again brings the folder up to date instead of creating a second one: a recording goes up unless the folder has a file of its name, and `summary.md` or `transcript.md` is updated in place when its content changed (below). A session is marked synced only when every recording it names was in the upload: one missing from disk when the upload's file list was built (its disk not mounted) fails the sync, even if it comes back during the upload, and a retry uploads it once it is back.

**Sync and GC changing a row at once.** A session is marked synced only while it is still exactly what the sync read and uploaded (`MarkSynced`, a compare-and-set read in the same transaction as the update): its audio path, and its summary and transcript as rendered. If GC changed the row during the upload (put back a recording it could not remove), or the summary or transcript changed (made again, refined), the sync is not done: the session goes back to pending sync, its folder and retry count kept, and the next sync uploads it as it is now. A sync that ran from start to end while GC had the row marked was marked synced with the row as it then was; when GC then puts back a recording, it reopens the sync (pending, folder kept), so the retry uploads it.

**Nothing on Drive is ever deleted or trashed.** A re-sync creates what a folder lacks and updates `summary.md` and `transcript.md` in place (`Files.Update` with new media on the existing file): the same file id, comments and share links, and Drive's revision history keeps the earlier text, any edit made on Drive included. Each document keeps the hash of the content this app last wrote in its `appProperties`; a document whose newest copy holds the same hash is left alone, and one with no hash (uploaded by an earlier version) is updated once. A folder an earlier version left with two copies of a document is read (by restore) and updated (by a re-sync) through its most recently modified copy. A test type-checks the module and fails if any code outside tests uses a Drive API method that deletes (any `Delete`, of a file, a revision, a comment or a permission, and `EmptyTrash`) or the call such a method returns, or writes a file's `Trashed` field, however it reaches it (an aliased service, a method value, an interface, a pointer to the field).

### Deletion criteria

A session is eligible for age-based GC when **all these conditions are true**:

1. **Synced:** `sync_status = 'synced'` (confirmed backed up to Drive, with its current summary and transcript), checked again in the statement that deletes it, so a session whose documents changed after GC listed it is kept until they are on Drive
2. **Documents verified:** `documents_unverified = 0`, also checked again in the deleting statement (sessions synced by an earlier version, above; the audio budget may still remove their audio once Drive confirms it)
3. **Still on Drive:** Drive answers, just before GC deletes the session or removes its audio, that it has the session's folder out of the trash (`FolderOnDrive`) and every local recording of the session under its recording name with the matching MD5 (`RecordingsMatch`). A folder in the trash (deleted in the Drive UI; Drive empties its trash after 30 days) or gone leaves the device with the only lasting copy: GC keeps the session and marks it `REMOTE_DELETED`. A synced session that records no folder is kept. A Drive that does not answer (or a 404 while the folder the app syncs into cannot be read either) stops the whole pass.
4. **Not `REMOTE_DELETED`:** no GC path deletes such a session or removes its audio.
5. **Age:** the session is older than `gc_max_age_days` (default: 30; 0 means never), counted from when it started or, if restored from Drive, from its restore, and its summary is complete.

The audio budget removes a session's audio only under the same Drive answer; a session it keeps no longer counts against the budget.

### When Drive sync is off, or Drive cannot answer

GC is always Drive-gated: the pass **holds**, deleting no session and removing no audio. The settings panel shows "Garbage collection is on hold until Google Drive sync is enabled.", and `GET /api/status` adds the warning "Garbage collection is on hold since <time>: <reason>. No session is deleted and no audio removed, so the disk fills; <disk use>. Fix Google Drive sync."

The hold's reason follows the setting and the syncer: with `gdrive_sync_enabled` off or no `gdrive_folder_id`, the pass gets no verifier and holds with `ErrDriveSyncOff`; with the setting on but no syncer running (unreadable credentials), it holds with `ErrDriveNotRunning`. Either way nothing can ask Drive whether it still has a session, so GC deletes no session and removes no audio. Such a pass, and one that stops because Drive did not answer, is a **GC hold**: `GET /api/status` reports it (`gc_hold`: the reason, since when, and the disk use of the audio directory's filesystem at the last pass) and adds a warning the web UI's warnings banner shows, so a broken sync is noticed before the disk fills. The next pass that can ask Drive clears it. Settings changes apply to the running app one at a time, each from the latest saved config (Drive sync, summarizer, batch transcriber, log level, Envoy, silence timeout), so two saved together (sync on, then off) cannot leave sync running while the config says off.

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
gc_max_age_days: 30                 # new — delete sessions older than this once Drive confirms the durable copy; 0 = never
gc_max_audio_size_mb: 1024          # new — budget for removable audio: past it, remove the oldest sessions' audio files (sessions kept); 0 = no budget
```

### Web UI changes

**Integrations settings panel (existing GDrive section):**
- Add sync enable/disable toggle
- Existing: folder ID input, credential upload

**New GC section in settings:**
- Enable/disable toggle
- Max age (days) input, 0 = never
- Audio budget (MB) input, 0 = no budget
- Warning banner when GC is enabled but Drive sync is off: "Garbage collection is on hold until Google Drive sync is enabled."

## Restore

`POST /api/restore/gdrive` rebuilds sessions from the session folders in the folder the app syncs into, ten folders at a time. For each folder it exports `transcript.md` (a folder without one is reported as an error) and `summary.md` when there is one, parses their frontmatter and transcript lines, and imports the session `SYNCED`, with the folder's id, its summary and its segments. A session already in the database is skipped. Audio is not downloaded: the recordings stay on Drive. The answer counts the sessions restored and skipped and lists the errors. The frontmatter's `schema_version` is written for a later reader; restore does not read it yet.

**A restore from a root that does not read.** Drive lists no folders, and no error, for an id it does not have, and so, it is taken, for a shared drive the app is not a member of. So a restore that lists no session folder asks whether the folder the app syncs into reads (as for a folder deleted on Drive, above) and fails when it does not, instead of reporting an empty Drive.

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
