<script lang="ts">
  import { appState, setActiveAudioSession } from '../lib/state.svelte'
  import type { Segment } from '../lib/types'

  let {
    sessionId,
    segments,
  }: {
    sessionId: string
    segments: Segment[]
  } = $props()

  let audioEl: HTMLAudioElement | null = null
  let currentTime = $state(0)
  let duration = $state(0)
  let loading = $state(true)
  let playing = $state(false)
  let error = $state('')

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
</div>
