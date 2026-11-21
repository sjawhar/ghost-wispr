---
title: "Dual-Pass Transcription and Canonicalization"
category: general
tags:
  - transcription
  - deepgram
  - batch-refinement
  - canonical-transcript
  - summarization
date: 2026-03-24
status: active
module: transcribe
symptoms:
  - "summary based on wrong transcript"
  - "refined transcript not used"
  - "refinement stuck at pending"
  - "canonical transcript empty"
---

# Dual-Pass Transcription and Canonicalization

## The Architecture

Ghost Whisper uses two transcription passes:

1. **Streaming** (live): Deepgram nova-3 WebSocket, real-time segments stored in `segments` table. Used for live display in the UI.

2. **Batch refinement** (post-session): After session ends and audio is encoded, the saved audio file is submitted to Deepgram's pre-recorded REST API. Result stored in `refined_transcript` column.

The **canonical transcript** is the best available version:
- If batch refinement completed: canonical = refined transcript
- If batch refinement failed or pending: canonical = assembled streaming segments

The summarizer, FTS5 search index, and Google Drive sync all use the canonical transcript — not raw segments. The summarizer reads its words with each segment's speaker label (`Speaker 0: …`), so the speaker ids it names are the segments' own, not guesses from turn order; the stored canonical transcript, search and Drive have no labels.

## The Flow

```
Session ends
  → Audio encoded to MP3
  → Batch refinement starts (background goroutine)
  → Summarizer waits up to 30s for refinement
  → If refinement completes in time: canonicalize with refined transcript
  → If timeout: canonicalize with streaming segments, summarize anyway
  → If refinement completes later: re-canonicalize, optionally re-summarize
```

## Key Implementation Details

### Batch Transcriber Interface

```go
type BatchTranscriber interface {
    Transcribe(ctx context.Context, audioPath string) (string, error)
}
```

Only Deepgram is implemented: `batch_transcription.model` names a provider/model whose provider speaks the Deepgram API (`config.RoleBatchTranscription`). The interface is ready for other providers.

### Canonicalization

`storage.Canonicalize(sessionID)` checks `refinement_status`:
- If `completed` and refined transcript is non-empty: use refined, set `transcript_source = 'refined'`
- Otherwise: assemble streaming segments, set `transcript_source = 'streaming'`

It returns that chosen text with the deletion generation from the same
transaction that writes it. The store has one SQLite connection, so segment
deletion waits for the transaction; canonicalization does not retry after a
deletion. Callers use the returned text and generation together rather than
reading the canonical transcript again.

`storage.CanonicalizeForSummary(sessionID)` is `Canonicalize` that also
returns, from the same transaction, the transcript every summary path reads
(the session's end, Resummarize, a merged session, the backlog online, a
summary batch job, a preset's test): each segment on a line of its own after
its speaker's label (`transcribe.Segment.SpeakerLine`). The segments hold the
canonical words: the live ones, or the refinement's, which replace them when
it is published. A session refined before refinements kept their words
(`refined_revision` 0) still has its live segments, which are not the refined
text, so it is summarized from the refined text without labels. The
summarizer counts words and makes a title from the transcript without the
labels (`transcribe.WithoutSpeakerLabels`).

### FTS5 Index

FTS5 indexes `canonical_transcript`, not segments. Triggers keep it in sync. When canonicalization updates the canonical transcript, the UPDATE trigger automatically re-indexes.

## Gotchas

1. **Historical sessions show "refine pending"**: Sessions created before batch refinement was added default to `refinement_status = 'pending'`. They'll never be refined. After importing old data, run:
   ```sql
   UPDATE sessions SET refinement_status = 'completed', transcript_source = 'streaming'
   WHERE refinement_status = 'pending' AND status IN ('ended', 'discarded');
   ```

2. **Batch transcription is optional**: If `batch_transcription.model` (by default `transcription.model`) is empty, or its provider's key is not set, the manager's `batchTranscriber` is nil. Summarization proceeds immediately with streaming segments. No error is raised.

3. **30-second timeout is hardcoded**: The wait for batch refinement before summarizing is 30 seconds. For very long recordings (>1 hour), batch refinement may take longer. The summarizer will use the streaming transcript, and re-canonicalization will happen later when batch completes.

4. **Re-canonicalization doesn't re-summarize by default**: When batch refinement completes late, `Canonicalize()` is called again, but the summary is NOT re-generated automatically. A user would need to click the retry-summary button to get a summary based on the refined transcript.

5. **No recording, no refinement**: A session that ends without a saved recording (its recording could not be saved after five attempts, or the app stopped mid-session and recovered it at startup) gets `refinement_status = 'no_audio'`. This is terminal unless a recording is available locally or the audio was removed to free space after a successful Drive sync: the session must have `audio_removed_at`, a `synced` status, a non-`REMOTE_DELETED` sync state, and its Drive folder. Sessions imported from Drive are also marked this way, because their recording remains in their Drive folder. `POST /api/sessions/{id}/retry-refinement` checks those conditions and answers 409 with a reason when none holds. Otherwise it answers 202, then refines in the background, restoring from Drive only when the recording is not on disk.

6. **A stop cuts a refinement off; the next start retries it once**: a refinement still `running` when the app stops (a deploy, a restart, a power cut) is marked `failed` at the next start (`recoverAtStart`, `RecoverStaleRefinements`), so GC is not held off, and the store records that the start owes it one automatic retry (`refinement_resume = 'pending'`). Once the recording directories are cleaned up, the start runs each owed retry with the Retry refinement action, one at a time, oldest first (`stopRecovery.retry`). Each retry is recorded (`attempted`) before it starts, so if it crashes or hangs the process, or another stop cuts it off, the next start does not run it again: the refinement stays `failed` (`exhausted`), and every start shows Retry's warning for it on `/api/status` until a person's Retry claims the refinement. A retry the start had not reached when a stop came stays owed at the next start. A retry that fails stays `failed` with the retry's status warning, as a pressed Retry does. The stop (`stopRecovery.stop`) waits, within its shutdown deadline, for the retries it cancelled.
