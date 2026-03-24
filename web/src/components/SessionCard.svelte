<script lang="ts">
  import Markdown from '@humanspeak/svelte-markdown'
  import AudioPlayer from './AudioPlayer.svelte'
  import type { PresetMap, SessionDetailResponse, SessionSummary } from '../lib/types'
  import { copyText } from '../lib/clipboard'
  import { updateSessionTitle, retrySummary, retrySync, retryRefinement } from '../lib/api'

  let {
    session,
    detail,
    expanded,
    presets,
    onToggle,
    onLoadDetail,
    onResummarize,
    onDelete,
    selected = false,
    onToggleSelect = () => {},
  }: {
    session: SessionSummary
    detail: SessionDetailResponse | undefined
    expanded: boolean
    presets: PresetMap
    selected?: boolean
    onToggle: () => void
    onLoadDetail: (id: string) => Promise<void>
    onResummarize: (sessionId: string, preset: string) => Promise<void>
    onDelete: (id: string) => Promise<void>
    onToggleSelect?: () => void
  } = $props()

  let showPresetMenu = $state(false)
  let summaryCopied = $state(false)
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let retryingSummary = $state(false)
  let retryingSync = $state(false)
  let retryingRefinement = $state(false)

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
      // API failed — title stays unchanged in UI, user can retry
      titleDraft = session.title || ''
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

  async function handleRetrySummary(e: Event) {
    e.stopPropagation()
    retryingSummary = true
    try {
      await retrySummary(session.id)
    } finally {
      setTimeout(() => retryingSummary = false, 1000)
    }
  }

  async function handleRetrySync(e: Event) {
    e.stopPropagation()
    retryingSync = true
    try {
      await retrySync(session.id)
    } finally {
      setTimeout(() => retryingSync = false, 1000)
    }
  }

  async function handleRetryRefinement(e: Event) {
    e.stopPropagation()
    retryingRefinement = true
    try {
      await retryRefinement(session.id)
    } finally {
      setTimeout(() => retryingRefinement = false, 1000)
    }
  }
</script>

<article class="session-card" class:selected>
  <div
    class="session-header"
    role="button"
    tabindex="0"
    onclick={openCard}
    onkeydown={(e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        openCard()
      }
    }}
  >
    <div class="header-left">
      <input
        type="checkbox"
        class="session-checkbox"
        checked={selected}
        onclick={(e) => {
          e.stopPropagation()
          if (onToggleSelect) onToggleSelect()
        }}
      />
      <div class="title-time-wrapper">
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
              <span>Untitled Session</span>
              <span class="session-time-sub">{timeRange}</span>
            {/if}
            <button
              type="button"
              class="edit-title-btn"
              onclick={startEditTitle}
              title="Edit title"
            >
              &#9998;
            </button>
          </h4>
        {/if}
        <p class="session-duration">Duration {durationLabel}</p>
      </div>
    </div>
    <div class="header-right">
      <div class="status-badges">
        {#if !session.ended_at}
          <span class="recording-dot" title="Recording active" data-testid="recording-dot"></span>
        {/if}
        
        <div class="badge-group">
          <span class={`status-badge ${session.summary_status}`} data-testid="summary-badge">
            Summary: {session.summary_status}
          </span>
          {#if session.summary_status === 'failed'}
            <button class="retry-btn" onclick={handleRetrySummary} disabled={retryingSummary} data-testid="retry-summary-btn">
              {retryingSummary ? '...' : '↻'}
            </button>
          {/if}
        </div>

        {#if session.sync_status}
          <div class="badge-group">
            <span class={`status-badge ${session.sync_status}`} data-testid="sync-badge">
              Sync: {session.sync_status}
            </span>
            {#if session.sync_status === 'failed'}
              <button class="retry-btn" onclick={handleRetrySync} disabled={retryingSync} data-testid="retry-sync-btn">
                {retryingSync ? '...' : '↻'}
              </button>
            {/if}
          </div>
        {/if}

        {#if session.refinement_status}
          <div class="badge-group">
            <span class={`status-badge ${session.refinement_status}`} data-testid="refinement-badge">
              Refine: {session.refinement_status}
            </span>
            {#if session.refinement_status === 'failed'}
              <button class="retry-btn" onclick={handleRetryRefinement} disabled={retryingRefinement} data-testid="retry-refinement-btn">
                {retryingRefinement ? '...' : '↻'}
              </button>
            {/if}
          </div>
        {/if}

        {#if session.transcript_source}
          <span class="status-badge info" data-testid="source-badge">
            {session.transcript_source}
          </span>
        {/if}
      </div>
      <button
        type="button"
        class="quick-delete-btn"
        title="Delete session"
        onclick={async (e) => {
          e.stopPropagation()
          if (confirm('Delete this session? This cannot be undone.')) {
            await onDelete(session.id)
          }
        }}
      >
        ×
      </button>
    </div>
  </div>

  {#if session.summary_status === 'completed' && session.summary}
    <div class="summary-preview-md prose">
      <Markdown source={session.summary} />
    </div>
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
        <div class="session-actions">
          <button
            type="button"
            class="delete-btn"
            onclick={async (e) => {
              e.stopPropagation()
              if (confirm('Delete this session? This cannot be undone.')) {
                await onDelete(session.id)
              }
            }}
          >
            Delete session
          </button>
        </div>
      {:else}
        <p class="summary-preview">Loading session...</p>
      {/if}
    </div>
  {/if}
</article>

<style>
  .session-card.selected {
    border-color: var(--accent);
    background: var(--accent-soft);
  }

  .status-badges {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    margin-right: 0.5rem;
  }

  .badge-group {
    display: flex;
    align-items: center;
    gap: 0.25rem;
  }

  .status-badge {
    font-size: 0.65rem;
    padding: 0.15rem 0.4rem;
    border-radius: 1rem;
    text-transform: uppercase;
    font-weight: 600;
    letter-spacing: 0.02em;
    background: var(--surface-hover);
    color: var(--muted);
  }

  .status-badge.completed, .status-badge.synced {
    background: var(--success-soft);
    color: var(--success);
  }

  .status-badge.pending, .status-badge.running, .status-badge.syncing {
    background: var(--warning-soft);
    color: var(--warning);
  }

  .status-badge.failed {
    background: var(--danger-soft);
    color: var(--danger);
  }

  .status-badge.info {
    background: var(--accent-soft);
    color: var(--accent);
  }

  .retry-btn {
    background: none;
    border: none;
    color: var(--muted);
    cursor: pointer;
    font-size: 0.8rem;
    padding: 0.1rem 0.3rem;
    border-radius: 4px;
  }

  .retry-btn:hover:not(:disabled) {
    background: var(--surface-hover);
    color: var(--text);
  }

  .retry-btn:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .recording-dot {
    width: 8px;
    height: 8px;
    background-color: var(--danger);
    border-radius: 50%;
    animation: pulse 1.5s infinite;
  }

  @keyframes pulse {
    0% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(239, 68, 68, 0.7); }
    70% { transform: scale(1); box-shadow: 0 0 0 6px rgba(239, 68, 68, 0); }
    100% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(239, 68, 68, 0); }
  }

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
    background: var(--surface);
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
    background: var(--hover);
  }

  .title-input {
    font-family: var(--font-sans);
    font-size: 0.875rem;
    font-weight: 500;
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

  .session-actions {
    border-top: 1px solid var(--line);
    padding: 0.5rem 1rem;
    display: flex;
    justify-content: flex-end;
  }

  .delete-btn {
    font-size: 0.75rem;
    padding: 0.25rem 0.5rem;
    border: 1px solid var(--danger);
    border-radius: 4px;
    background: transparent;
    color: var(--danger);
    cursor: pointer;
  }

  .delete-btn:hover {
    background: var(--danger);
    color: #fff;
  }
</style>
