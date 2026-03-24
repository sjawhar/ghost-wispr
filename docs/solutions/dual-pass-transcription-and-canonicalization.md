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

The summarizer, FTS5 search index, and Google Drive sync all use the canonical transcript — not raw segments.

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

Only Deepgram is implemented. Config supports `batch_transcription.provider` (deepgram/groq/openai) but groq/openai return errors. The interface is ready for future providers.

### Canonicalization

`storage.Canonicalize(sessionID)` checks `refinement_status`:
- If `completed` and refined transcript is non-empty: use refined, set `transcript_source = 'refined'`
- Otherwise: assemble streaming segments, set `transcript_source = 'streaming'`

### FTS5 Index

FTS5 indexes `canonical_transcript`, not segments. Triggers keep it in sync. When canonicalization updates the canonical transcript, the UPDATE trigger automatically re-indexes.

## Gotchas

1. **Historical sessions show "refine pending"**: Sessions created before batch refinement was added default to `refinement_status = 'pending'`. They'll never be refined. After importing old data, run:
   ```sql
   UPDATE sessions SET refinement_status = 'completed', transcript_source = 'streaming'
   WHERE refinement_status = 'pending' AND status IN ('ended', 'discarded');
   ```

2. **Batch transcription is optional**: If `batch_transcription.provider` is empty or credentials are missing, the manager's `batchTranscriber` is nil. Summarization proceeds immediately with streaming segments. No error is raised.

3. **30-second timeout is hardcoded**: The wait for batch refinement before summarizing is 30 seconds. For very long recordings (>1 hour), batch refinement may take longer. The summarizer will use the streaming transcript, and re-canonicalization will happen later when batch completes.

4. **Re-canonicalization doesn't re-summarize by default**: When batch refinement completes late, `Canonicalize()` is called again, but the summary is NOT re-generated automatically. A user would need to click the retry-summary button to get a summary based on the refined transcript.
