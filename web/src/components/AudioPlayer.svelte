<script lang="ts">
  import { tick } from 'svelte'
  import { appState, setActiveAudioSession } from '../lib/state.svelte'
  import {
    RequestError,
    deleteSessionSegment,
    requestErrorMessage,
    type SegmentDeleteMode,
  } from '../lib/api'
  import {
    RefinementStatus,
    SyncState,
    SyncStatus,
    type Segment,
    type SessionSummary,
  } from '../lib/types'
  import { copyText } from '../lib/clipboard'
  import { audioLeftNote, recordingsNamed } from '../lib/audio'

  let {
    sessionId,
    segments,
    session,
    onDeleted,
    onNotFound,
  }: {
    sessionId: string
    segments: Segment[]
    session?: SessionSummary
    onDeleted: (segmentID: number) => void | Promise<void>
    onNotFound: (segmentID: number) => boolean | Promise<boolean>
  } = $props()

  let audioEl: HTMLAudioElement | null = $state(null)
  let currentTime = $state(0)
  let duration = $state(0)
  let loading = $state(true)
  let playing = $state(false)
  let error = $state('')

  let transcriptCopied = $state(false)
  let visibleSegments = $derived(segments)
  const audioVersion = $derived(visibleSegments.map((segment) => segment.id ?? '').join('.'))
  let segmentDeleteError = $state('')
  let deletingSegmentID = $state<number | null>(null)
  // The segment whose delete actions are shown.
  let deleteMenuFor = $state<number | null>(null)

  // The two deletion actions (Sami, 2026-11-12): their names, the one
  // sentence that says what each does, and the question asked before it.
  const deleteActions: { mode: SegmentDeleteMode; name: string; note: string; ask: string }[] = [
    {
      mode: 'text',
      name: 'Delete text',
      note: 'Removes the words from the transcript, summary and search, while the recording and its Drive copy stay as they are.',
      ask: "Delete this segment's text? This cannot be undone.",
    },
    {
      mode: 'text_and_audio',
      name: 'Delete text and audio',
      note: 'Also cuts the words out of the recording and replaces the transcript, summary and recording in Drive, so their links change.',
      ask: "Delete this segment's text and audio? This cannot be undone.",
    },
  ]

  function closeDeleteMenuOnEscape(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      deleteMenuFor = null
    }
  }

  function closeDeleteMenuOnOutsideClick(event: MouseEvent): void {
    if (!(event.target instanceof Element) || !event.target.closest('.segment-delete-controls')) {
      deleteMenuFor = null
    }
  }

  const refinementInFlight = $derived(
    session?.refinement_status === RefinementStatus.Pending ||
      session?.refinement_status === RefinementStatus.Running,
  )

  // The completed default view is the timed segment table rebuilt from batch
  // words. Live segments remain readable while refinement runs, but cannot
  // identify a word occurrence safely enough to delete.
  const deletionAvailable = $derived(
    session?.refinement_status === RefinementStatus.Completed &&
      visibleSegments.length > 0 &&
      visibleSegments.every((segment) => (segment.transcript_revision ?? 0) > 0),
  )
  const deleteUnavailableNote = $derived.by(() => {
    if (session?.refinement_status === RefinementStatus.Failed) {
      return 'Refinement failed; retry refinement before deleting.'
    }
    if (session?.refinement_status === RefinementStatus.NoAudio) {
      if (
        session.audio_removed_at &&
        session.sync_status === SyncStatus.Synced &&
        session.sync_state !== SyncState.RemoteDeleted
      ) {
        return 'No recording is on this unit. Use Retry refinement to restore it from Drive, then delete.'
      }
      return 'No recording was saved for this conversation, so it cannot be refined or deleted.'
    }
    if (session?.refinement_status === RefinementStatus.Completed && !session.refined_revision) {
      return 'This conversation completed without timed words. Use Retry refinement before deleting.'
    }
    return 'This conversation is still being refined; delete once it finishes.'
  })

  // Disk pressure removed the session's audio to free space: all of it, or
  // part of it, when the audio path still names what is left.
  const audioRemoved = $derived(Boolean(session?.audio_removed_at))
  const audioLeft = $derived(audioRemoved && recordingsNamed(session?.audio_path) > 0)
  // The session ended without a saved recording: nothing to play or refine.
  const noRecording = $derived(session?.refinement_status === RefinementStatus.NoAudio)

  function segmentsAsText(): string {
    return visibleSegments
      .map((s) => `[${prettyTime(s.start_time)}] Speaker ${s.speaker}: ${s.text}`)
      .join('\n')
  }

  async function copyTranscript() {
    const text = segmentsAsText()
    const ok = await copyText(text)
    if (ok) {
      transcriptCopied = true
      setTimeout(() => (transcriptCopied = false), 2000)
    }
  }

  async function reloadAudio(): Promise<void> {
    currentTime = 0
    duration = 0
    loading = true
    playing = false
    error = ''

    if (!audioEl) {
      return
    }

    audioEl.pause()
    audioEl.currentTime = 0
    await tick()
    audioEl?.load()
  }

  async function handleDeleteSegment(
    segment: Segment,
    action: (typeof deleteActions)[number],
  ): Promise<void> {
    deleteMenuFor = null
    const segmentID = segment.id
    const revision = segment.transcript_revision
    if (
      segmentID === undefined ||
      revision === undefined ||
      !deletionAvailable ||
      deletingSegmentID !== null
    ) {
      return
    }
    if (!confirm(action.ask)) {
      return
    }
    deletingSegmentID = segmentID
    segmentDeleteError = ''
    try {
      try {
        await deleteSessionSegment(sessionId, segmentID, action.mode, revision)
      } catch (error) {
        const message = requestErrorMessage(error)
        if (error instanceof RequestError && [403, 409, 422, 503].includes(error.status)) {
          segmentDeleteError = message
          return
        }
        segmentDeleteError = message
        try {
          const clearLocally = await onNotFound(segmentID)
          if (clearLocally) {
            visibleSegments = visibleSegments.filter((candidate) => candidate.id !== segmentID)
            await reloadAudio()
          }
        } catch {
          // Keep the delete response visible when the reconciliation request fails.
        }
        return
      }
      visibleSegments = visibleSegments.filter((candidate) => candidate.id !== segmentID)
      await reloadAudio()
      await onDeleted(segmentID)
    } catch (error) {
      segmentDeleteError = `The session could not be reloaded: ${requestErrorMessage(error)}`
    } finally {
      deletingSegmentID = null
    }
  }

  const activeSegmentIndex = $derived.by(() => {
    if (visibleSegments.length === 0) {
      return -1
    }

    let lo = 0
    let hi = visibleSegments.length - 1
    while (lo <= hi) {
      const mid = Math.floor((lo + hi) / 2)
      const segment = visibleSegments[mid]
      if (currentTime < segment.start_time) {
        hi = mid - 1
      } else if (currentTime >= segment.end_time) {
        lo = mid + 1
      } else {
        return mid
      }
    }
    return -1
  })

  function prettyTime(seconds: number): string {
    const safe = Number.isFinite(seconds) ? Math.max(0, Math.floor(seconds)) : 0
    const mm = String(Math.floor(safe / 60)).padStart(2, '0')
    const ss = String(safe % 60).padStart(2, '0')
    return `${mm}:${ss}`
  }

  function togglePlay() {
    if (!audioEl) {
      return
    }

    if (audioEl.paused) {
      setActiveAudioSession(sessionId)
      void audioEl.play()
    } else {
      audioEl.pause()
    }
  }

  function seekTo(seconds: number) {
    if (!audioEl) {
      return
    }
    audioEl.currentTime = seconds
    setActiveAudioSession(sessionId)
  }

  function onLoadedMetadata() {
    if (!audioEl) {
      return
    }
    loading = false
    duration = audioEl.duration
  }

  function onTimeUpdate() {
    if (!audioEl) {
      return
    }
    currentTime = audioEl.currentTime
  }

  $effect(() => {
    if (appState.activeAudioSessionId !== sessionId && audioEl && !audioEl.paused) {
      audioEl.pause()
    }
  })
</script>

<svelte:window onkeydown={closeDeleteMenuOnEscape} />
<svelte:document onclick={closeDeleteMenuOnOutsideClick} />

<div class="audio-player" data-testid="audio-player">
  {#if audioRemoved && !audioLeft}
    <p class="audio-note">
      The audio was removed to free space. The transcript and summary are kept.
    </p>
  {:else if noRecording && !audioLeft}
    <p class="audio-note">
      No recording was saved for this session, so it cannot be played or refined.
    </p>
  {:else}
    {#if audioLeft && session}
      <p class="audio-note">
        Part of the audio was removed to free space; {audioLeftNote(session)}.
      </p>
    {/if}
    <audio
      bind:this={audioEl}
      src={`/api/sessions/${encodeURIComponent(sessionId)}/audio?v=${audioVersion}`}
      preload="metadata"
      onloadedmetadata={onLoadedMetadata}
      ontimeupdate={onTimeUpdate}
      onplay={() => (playing = true)}
      onpause={() => (playing = false)}
      onerror={() => {
        loading = false
        error = 'Audio unavailable'
      }}
    ></audio>

    <div class="audio-controls">
      <button type="button" class="audio-btn" onclick={togglePlay}>
        {playing ? 'Pause Audio' : 'Play Audio'}
      </button>
      <span class="audio-time">{prettyTime(currentTime)} / {prettyTime(duration)}</span>
    </div>

    {#if loading}
      <p class="audio-note">Loading audio...</p>
    {:else if error}
      <p class="audio-error">{error}</p>
    {/if}
  {/if}

  <div class="transcript-header">
    <span class="transcript-label">Transcript</span>
    <div class="transcript-header-actions">
      {#if refinementInFlight}
        <span class="refining-indicator" data-testid="refining-indicator">Refining…</span>
      {/if}
      <button type="button" class="copy-btn" onclick={copyTranscript}>
        {transcriptCopied ? 'Copied!' : 'Copy transcript'}
      </button>
    </div>
  </div>

  {#if segmentDeleteError}
    <p class="segment-delete-error" role="alert">{segmentDeleteError}</p>
  {/if}

  <div class="transcript-sync">
    {#each visibleSegments as segment, index (segment.id ?? segment.timestamp + segment.text + index)}
      <div class="transcript-line">
        <button
          type="button"
          class={`line ${index === activeSegmentIndex ? 'active' : ''}`}
          onclick={() => seekTo(segment.start_time)}
        >
          <span class="line-time">{prettyTime(segment.start_time)}</span>
          <span class="line-text" id={`segment-${segment.id ?? index}`}>{segment.text}</span>
        </button>
        {#if segment.id !== undefined}
          <div class="segment-delete-controls">
            <button
              type="button"
              class="segment-delete-btn"
              aria-label="Delete segment"
              aria-describedby={`segment-${segment.id ?? index}`}
              aria-expanded={deletionAvailable && deleteMenuFor === segment.id}
              title="Delete segment"
              disabled={!deletionAvailable || deletingSegmentID !== null}
              onclick={() => {
                if (deletionAvailable) {
                  deleteMenuFor = deleteMenuFor === segment.id ? null : segment.id!
                }
              }}
            >
              {deletingSegmentID === segment.id ? 'Deleting…' : 'Delete'}
            </button>
            {#if !deletionAvailable}
              <span class="segment-delete-note">{deleteUnavailableNote}</span>
            {:else if deleteMenuFor === segment.id}
              <div class="segment-delete-menu" role="group" aria-label="Delete options">
                {#each deleteActions as action (action.mode)}
                  <div class="segment-delete-option">
                    <button
                      type="button"
                      class="segment-delete-action"
                      aria-describedby={`segment-${segment.id}-${action.mode}`}
                      onclick={() => void handleDeleteSegment(segment, action)}
                    >
                      {action.name}
                    </button>
                    <span class="segment-delete-note" id={`segment-${segment.id}-${action.mode}`}
                      >{action.note}</span
                    >
                  </div>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      </div>
    {/each}
  </div>
</div>
