<script lang="ts">
  import Markdown from '@humanspeak/svelte-markdown'
  import AudioPlayer from './AudioPlayer.svelte'
  import type { SessionDetailResponse, SessionSummary } from '../lib/types'

  let {
    session,
    detail,
    expanded,
    onToggle,
    onLoadDetail,
  }: {
    session: SessionSummary
    detail: SessionDetailResponse | undefined
    expanded: boolean
    onToggle: () => void
    onLoadDetail: (id: string) => Promise<void>
  } = $props()

  const timeRange = $derived.by(() => {
    const start = new Date(session.started_at)
    const end = session.ended_at ? new Date(session.ended_at) : null
    const startLabel = start.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    const endLabel = end
      ? end.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
      : 'Active'
    return `${startLabel} - ${endLabel}`
  })

  const durationLabel = $derived.by(() => {
    const start = Date.parse(session.started_at)
    const end = session.ended_at ? Date.parse(session.ended_at) : Date.now()
    const secs = Math.max(0, Math.floor((end - start) / 1000))
    const mm = String(Math.floor(secs / 60)).padStart(2, '0')
    const ss = String(secs % 60).padStart(2, '0')
    return `${mm}:${ss}`
  })

  function summaryPreview(summary: string): string {
    const lines = summary
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean)
    return lines.slice(0, 2).join(' ')
  }

  async function openCard() {
    onToggle()
    if (!expanded && !detail) {
      await onLoadDetail(session.id)
    }
  }
</script>

<article class="session-card">
  <button type="button" class="session-header" onclick={openCard}>
    <div>
      <h4>{timeRange}</h4>
      <p class="session-duration">Duration {durationLabel}</p>
    </div>
    <span class={`summary-badge ${session.summary_status}`}>{session.summary_status}</span>
  </button>

  {#if session.summary_status === 'completed' && session.summary}
    <p class="summary-preview">{summaryPreview(session.summary)}</p>
  {:else if session.summary_status === 'running' || session.summary_status === 'pending'}
    <p class="summary-preview">Summarizing...</p>
  {:else if session.summary_status === 'failed'}
    <p class="summary-preview">Summary unavailable</p>
  {/if}

  {#if expanded}
    <div class="session-details">
      {#if detail}
        <AudioPlayer sessionId={session.id} segments={detail.segments} />

        {#if session.summary_status === 'completed' && session.summary}
          <div class="summary-markdown prose">
            <Markdown source={session.summary} />
          </div>
        {/if}
      {:else}
        <p class="summary-preview">Loading session...</p>
      {/if}
    </div>
  {/if}
</article>
