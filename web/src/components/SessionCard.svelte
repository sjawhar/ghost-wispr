<script lang="ts">
  import Markdown from '@humanspeak/svelte-markdown'
  import AudioPlayer from './AudioPlayer.svelte'
  import type { PresetMap, SessionDetailResponse, SessionSummary } from '../lib/types'

  let {
    session,
    detail,
    expanded,
    presets,
    onToggle,
    onLoadDetail,
    onResummarize,
  }: {
    session: SessionSummary
    detail: SessionDetailResponse | undefined
    expanded: boolean
    presets: PresetMap
    onToggle: () => void
    onLoadDetail: (id: string) => Promise<void>
    onResummarize: (sessionId: string, preset: string) => Promise<void>
  } = $props()

  let showPresetMenu = $state(false)

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

  async function handleResummarize(preset: string) {
    showPresetMenu = false
    await onResummarize(session.id, preset)
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

  {#if (session.summary_status === 'completed' || session.summary_status === 'failed') && Object.keys(presets).length > 0}
    <div class="resummarize-wrap">
      {#if Object.keys(presets).length === 1}
        <button
          type="button"
          class="resummarize-btn"
          onclick={() => handleResummarize(Object.keys(presets)[0])}
        >
          Resummarize
        </button>
      {:else}
        <button
          type="button"
          class="resummarize-btn"
          onclick={() => (showPresetMenu = !showPresetMenu)}
        >
          Resummarize ▾
        </button>
        {#if showPresetMenu}
          <div class="preset-menu">
            {#each Object.entries(presets) as [name, description] (name)}
              <button
                type="button"
                class="preset-option"
                onclick={() => handleResummarize(name)}
                title={description}
              >
                {name}
              </button>
            {/each}
          </div>
        {/if}
      {/if}
    </div>
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

<style>
  .resummarize-wrap {
    position: relative;
    margin-top: 0.5rem;
  }

  .resummarize-btn {
    font-size: 0.75rem;
    padding: 0.25rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: transparent;
    cursor: pointer;
  }

  .preset-menu {
    position: absolute;
    top: 100%;
    left: 0;
    z-index: 10;
    background: var(--surface, #fff);
    border: 1px solid var(--border);
    border-radius: 4px;
    margin-top: 0.25rem;
    min-width: 10rem;
  }

  .preset-option {
    display: block;
    width: 100%;
    text-align: left;
    padding: 0.5rem;
    border: none;
    background: transparent;
    cursor: pointer;
    font-size: 0.8rem;
  }

  .preset-option:hover {
    background: var(--hover, #f5f5f5);
  }
</style>
