---
title: "Schema Migration Safety: Preventing Data Loss on Deploy"
category: general
tags:
  - sqlite
  - migration
  - data-safety
  - deploy
  - fts5
date: 2026-03-24
status: active
module: storage
symptoms:
  - "sessions disappeared after deploy"
  - "historical data missing"
  - "database empty after upgrade"
  - "FTS5 rebuild failed"
---

# Schema Migration Safety: Preventing Data Loss on Deploy

## The Incident

During the production hardening deploy, 561 historical sessions disappeared from the database. The migration code uses additive `ALTER TABLE ADD COLUMN` with duplicate-column error suppression — which should be non-destructive. But the data was lost.

A backup from 2 days prior existed at `ghost-wispr.db.bak` and we restored from it. But those 2 days of data between the backup and the deploy were gone.

## Root Cause (Partial)

The exact mechanism wasn't definitively identified. The migration code looks correct:

```go
// This is a no-op if the table exists (which it did)
CREATE TABLE IF NOT EXISTS sessions (...)

// These add columns, ignoring "duplicate column" errors
ALTER TABLE sessions ADD COLUMN refined_transcript TEXT NOT NULL DEFAULT ''
ALTER TABLE sessions ADD COLUMN canonical_transcript TEXT NOT NULL DEFAULT ''
// ... more columns

// FTS5 virtual table creation
CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
    title, summary, canonical_transcript,
    content='sessions', content_rowid='rowid'
)

// FTS5 index rebuild
INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')
```

The most likely explanation involves FTS5 interaction: the virtual table references `canonical_transcript` in its content definition, and the `rebuild` command reads from the sessions table. If there was a timing issue where the column didn't exist when the FTS5 table was created, or if the rebuild encountered an error that cascaded, data could have been lost.

## What MUST Be Done

### 1. Pre-Migration Backup (NOT YET IMPLEMENTED)

Before `init()` runs any schema changes, copy the DB file:

```go
func (s *SQLiteStore) init() error {
    // BACKUP FIRST
    if err := backupDB(s.dbPath); err != nil {
        return fmt.Errorf("pre-migration backup: %w", err)
    }
    // ... then run migrations
}
```

This is the most important safety net. If migrations corrupt data, you can restore.

### 2. Default Values for Restored Data

When importing historical sessions into a schema with new columns, the columns default to their `DEFAULT` values. This can cause confusing UI states:

- `refinement_status` defaults to `'pending'` — but old sessions will never be refined. They should be `'completed'` with `transcript_source = 'streaming'`.
- `sync_state` defaults to `'PENDING'` — but old sessions may already be synced.

After any bulk import or migration, run:
```sql
UPDATE sessions SET refinement_status = 'completed', transcript_source = 'streaming'
WHERE refinement_status = 'pending' AND status IN ('ended', 'discarded');
```

### 3. FTS5 Rebuild Runs at Startup

The code currently does `INSERT INTO sessions_fts(sessions_fts) VALUES('rebuild')` on every startup. This rebuilds the entire FTS5 index from the sessions table. For small databases (< 10K sessions) this is fine. For larger databases, this will block startup.

Consider making the rebuild conditional — only run if the index is empty or a migration was applied.

## Gotchas

1. **The API defaults to today's date.** `GET /api/sessions` filters by today. After restoring historical data, it looks like nothing is there until you query with `?date=2026-03-20`.

2. **Backup location matters.** The backup at `ghost-wispr.db.bak` existed because a previous deploy created it. There's no automated backup rotation. If the backup itself is corrupted or too old, you're out of luck.

3. **SQLite WAL mode complicates backups.** If the DB is in WAL mode (it is), you need to copy both `ghost-wispr.db` and `ghost-wispr.db-wal` for a consistent backup, or run `PRAGMA wal_checkpoint(TRUNCATE)` first.
