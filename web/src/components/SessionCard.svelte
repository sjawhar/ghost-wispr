<script lang="ts">
  import Markdown from '@humanspeak/svelte-markdown'
  import AudioPlayer from './AudioPlayer.svelte'
  import type { PresetMap, SessionDetailResponse, SessionSummary } from '../lib/types'
  import { copyText } from '../lib/clipboard'
  import { updateSessionTitle } from '../lib/api'

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
  let summaryCopied = $state(false)
  let editingTitle = $state(false)
  let titleDraft = $state('')

  async function copySummary() {
    if (!session.summary) return
    const ok = await copyText(session.summary)
    if (ok) {
      summaryCopied = true
      setTimeout(() => (summaryCopied = false), 2000)
    }
  }

  function startEditTitle(event: Event) {
    event.stopPropagation()
    editingTitle = true
    titleDraft = session.title || ''
  }

  async function saveTitle() {
    editingTitle = false
    const newTitle = titleDraft.trim()
    if (newTitle === (session.title || '')) return
    try {
      const updated = await updateSessionTitle(session.id, newTitle)
      session.title = updated.title
    } catch {
      // revert on failure
    }
  }

  function handleTitleKeydown(event: KeyboardEvent) {
    if (event.key === 'Enter') {
      event.preventDefault()
      void saveTitle()
    } else if (event.key === 'Escape') {
      editingTitle = false
    }
  }

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
      {#if editingTitle}
        <!-- svelte-ignore a11y_autofocus -->
        <input
          class="title-input"
          type="text"
          bind:value={titleDraft}
          onblur={saveTitle}
          onkeydown={handleTitleKeydown}
          onclick={(e) => e.stopPropagation()}
          autofocus
        />
      {:else}
        <h4>
          {#if session.title}
            <span>{session.title}</span>
            <span class="session-time-sub">{timeRange}</span>
          {:else}
            {timeRange}
          {/if}
          <button type="button" class="edit-title-btn" onclick={startEditTitle} title="Edit title">
            &#9998;
          </button>
        </h4>
      {/if}
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
          <div class="summary-section">
            <div class="section-header">
              <span class="section-label">Summary</span>
              <button type="button" class="copy-btn" onclick={copySummary}>
                {summaryCopied ? 'Copied!' : 'Copy summary'}
              </button>
            </div>
            <div class="summary-markdown prose">
              <Markdown source={session.summary} />
            </div>
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
    border: 1px solid var(--line);
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
    border: 1px solid var(--line);
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

  .title-input {
    font-family: var(--font-serif);
    font-size: 1rem;
    font-weight: 400;
    border: 1px solid var(--accent);
    border-radius: 0.3rem;
    padding: 0.15rem 0.4rem;
    width: 100%;
    max-width: 20rem;
    outline: none;
    box-shadow: 0 0 0 2px var(--accent-soft);
  }

  .session-time-sub {
    font-size: 0.75rem;
    color: var(--muted);
    font-weight: 400;
    font-family: var(--font-sans);
    margin-left: 0.5rem;
  }

  .edit-title-btn {
    border: none;
    background: none;
    cursor: pointer;
    font-size: 0.75rem;
    color: var(--muted);
    padding: 0 0.2rem;
    opacity: 0;
    transition: opacity 0.15s ease;
    vertical-align: middle;
  }

  .session-header:hover .edit-title-btn {
    opacity: 1;
  }

  .edit-title-btn:hover {
    color: var(--accent);
  }
</style>
