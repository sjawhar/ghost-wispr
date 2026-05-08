<script lang="ts">
  import { appState, setActiveAudioSession } from '../lib/state.svelte'
  import { RefinementStatus, type Segment, type SessionSummary } from '../lib/types'
  import { copyText } from '../lib/clipboard'

  let {
    sessionId,
    segments,
    session,
  }: {
    sessionId: string
    segments: Segment[]
    session?: SessionSummary
  } = $props()

  let audioEl: HTMLAudioElement | null = null
  let currentTime = $state(0)
  let duration = $state(0)
  let loading = $state(true)
  let playing = $state(false)
  let error = $state('')

  let transcriptCopied = $state(false)

  // Refined transcript: prefer the explicit refined_transcript, fall back to
  // canonical_transcript when the source is 'refined' (covers historical sessions
  // where the backend canonicalized into canonical_transcript).
  const refinedText = $derived.by(() => {
    if (!session) return ''
    const refined = session.refined_transcript?.trim()
    if (refined) return refined
    if (session.transcript_source === 'refined') {
      return session.canonical_transcript?.trim() ?? ''
    }
    return ''
  })

  const refinementCompleted = $derived(
    session?.refinement_status === RefinementStatus.Completed && refinedText.length > 0,
  )

  const refinementInFlight = $derived(
    session?.refinement_status === RefinementStatus.Pending ||
      session?.refinement_status === RefinementStatus.Running,
  )

  type View = 'refined' | 'segments'
  // Default to refined view whenever a refined transcript is available.
  let view = $state<View>('segments')
  $effect(() => {
    if (refinementCompleted) {
      view = 'refined'
    } else {
      view = 'segments'
    }
  })

  function segmentsAsText(): string {
    return segments
      .map((s) => `[${prettyTime(s.start_time)}] Speaker ${s.speaker}: ${s.text}`)
      .join('\n')
  }

  async function copyTranscript() {
    const text = view === 'refined' && refinedText ? refinedText : segmentsAsText()
    const ok = await copyText(text)
    if (ok) {
      transcriptCopied = true
      setTimeout(() => (transcriptCopied = false), 2000)
    }
  }

  const activeSegmentIndex = $derived.by(() => {
    if (segments.length === 0) {
      return -1
    }

    let lo = 0
    let hi = segments.length - 1
    while (lo <= hi) {
      const mid = Math.floor((lo + hi) / 2)
      const segment = segments[mid]
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

<div class="audio-player" data-testid="audio-player">
  <audio
    bind:this={audioEl}
    src={`/api/sessions/${encodeURIComponent(sessionId)}/audio`}
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

  <div class="transcript-header">
    <span class="transcript-label">Transcript</span>
    <div class="transcript-header-actions">
      {#if refinementCompleted}
        <div
          class="transcript-toggle"
          role="group"
          aria-label="Transcript view"
          data-testid="transcript-view-toggle"
        >
          <button
            type="button"
            class={`toggle-btn ${view === 'refined' ? 'active' : ''}`}
            aria-pressed={view === 'refined'}
            onclick={() => (view = 'refined')}
          >
            Refined
          </button>
          <button
            type="button"
            class={`toggle-btn ${view === 'segments' ? 'active' : ''}`}
            aria-pressed={view === 'segments'}
            onclick={() => (view = 'segments')}
          >
            Segments
          </button>
        </div>
      {:else if refinementInFlight}
        <span class="refining-indicator" data-testid="refining-indicator">Refining…</span>
      {/if}
      <button type="button" class="copy-btn" onclick={copyTranscript}>
        {transcriptCopied ? 'Copied!' : 'Copy transcript'}
      </button>
    </div>
  </div>

  {#if view === 'refined' && refinedText}
    <div class="transcript-refined" data-testid="transcript-refined">
      {refinedText}
    </div>
  {:else}
    <div class="transcript-sync">
      {#each segments as segment, index (segment.timestamp + segment.text + index)}
        <button
          type="button"
          class={`line ${index === activeSegmentIndex ? 'active' : ''}`}
          onclick={() => seekTo(segment.start_time)}
        >
          <span class="line-time">{prettyTime(segment.start_time)}</span>
          <span class="line-text">{segment.text}</span>
        </button>
      {/each}
    </div>
  {/if}
</div>
