<script lang="ts">
  import { onDestroy } from 'svelte'
  import type { LiveTranscriptEvent } from '../lib/types'

  let {
    segments,
    connected,
    activeSessionStartedAt,
    interimText,
    interimSpeaker,
  }: {
    segments: LiveTranscriptEvent[]
    connected: boolean
    activeSessionStartedAt: number
    interimText: string
    interimSpeaker: number
  } = $props()

  let container: HTMLDivElement | null = null
  let stickToBottom = $state(true)
  let now = $state(Date.now())

  const timer = setInterval(() => {
    now = Date.now()
  }, 1000)

  onDestroy(() => {
    clearInterval(timer)
  })

  function speakerClass(speaker: number): string {
    if (speaker === 0) return 'speaker-0'
    if (speaker === 1) return 'speaker-1'
    if (speaker === 2) return 'speaker-2'
    if (speaker === 3) return 'speaker-3'
    return 'speaker--1'
  }

  function handleScroll() {
    if (!container) {
      return
    }

    const offset = container.scrollHeight - container.scrollTop - container.clientHeight
    stickToBottom = offset < 24
  }

  function prettyTime(ts: string): string {
    const d = new Date(ts)
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
  }

  const liveDuration = $derived.by(() => {
    if (activeSessionStartedAt <= 0) {
      return '00:00'
    }

    const secs = Math.max(0, Math.floor((now - activeSessionStartedAt) / 1000))
    const mm = String(Math.floor(secs / 60)).padStart(2, '0')
    const ss = String(secs % 60).padStart(2, '0')
    return `${mm}:${ss}`
  })

  $effect(() => {
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions -- reactive dependency tracking
    segments.length
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions -- reactive dependency tracking
    interimText
    if (stickToBottom && container) {
      requestAnimationFrame(() => {
        if (container) {
          container.scrollTop = container.scrollHeight
        }
      })
    }
  })
</script>

<section class="live-panel" data-testid="live-panel">
  <header class="panel-head">
    <h2>Live Transcription</h2>
    <span class="timer">{liveDuration}</span>
  </header>

  <div class="live-stream" bind:this={container} onscroll={handleScroll}>
    {#if segments.length === 0}
      <div class="idle-state">
        <span class="pulse" class:connected></span>
        <span>{connected ? 'Listening...' : 'Waiting for connection...'}</span>
      </div>
    {/if}

    {#each segments as segment (segment.timestamp + segment.text + segment.start_time)}
      <article class="segment-row">
        <span class="segment-time">{prettyTime(segment.timestamp)}</span>
        <strong class={`segment-speaker ${speakerClass(segment.speaker)}`}>
          Speaker {segment.speaker}
        </strong>
        <span class="segment-text">{segment.text}</span>
      </article>
    {/each}

    {#if interimText}
      <article class="segment-row interim">
        <span class="segment-time"></span>
        <strong class={`segment-speaker ${speakerClass(interimSpeaker)}`}>
          {interimSpeaker >= 0 ? `Speaker ${interimSpeaker}` : '...'}
        </strong>
        <span class="segment-text">{interimText}</span>
      </article>
    {/if}
  </div>
</section>
