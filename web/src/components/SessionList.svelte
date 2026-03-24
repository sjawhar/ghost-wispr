<script lang="ts">
  import SessionCard from './SessionCard.svelte'
  import type { PresetMap, SessionDetailResponse, SessionSummary } from '../lib/types'
  import { searchSessions, type SearchResult } from '../lib/api'

  let {
    dates,
    sessionsByDate,
    sessionDetails,
    presets,
    expandedSessionId,
    onToggleSession,
    onLoadDate,
    onLoadDetail,
    onResummarize,
    onDelete,
    onMerge,
  }: {
    dates: string[]
    sessionsByDate: Map<string, SessionSummary[]>
    sessionDetails: Map<string, SessionDetailResponse>
    presets: PresetMap
    expandedSessionId: string
    onToggleSession: (id: string) => void
    onLoadDate: (date: string) => Promise<void>
    onLoadDetail: (id: string) => Promise<void>
    onResummarize: (sessionId: string, preset: string) => Promise<void>
    onDelete: (id: string) => Promise<void>
    onMerge: (sessionIds: string[]) => Promise<void>
  } = $props()

  let loadedDates = $state(3)
  let showHidden = $state<Record<string, boolean>>({})
  let selectedIds = $state<Set<string>>(new Set())
  let dateFilter = $state('all')
  let statusFilter = $state('all')

  function toggleSelect(id: string) {
    const next = new Set(selectedIds)
    if (next.has(id)) {
      next.delete(id)
    } else {
      next.add(id)
    }
    selectedIds = next
  }

  function exitSelectMode() {
    selectedIds = new Set()
  }

  async function handleMerge() {
    if (selectedIds.size < 2) return
    await onMerge([...selectedIds])
    exitSelectMode()
  }
  function isShortSession(session: SessionSummary): boolean {
    if (!session.ended_at) return false
    const durationMs = Date.parse(session.ended_at) - Date.parse(session.started_at)
    const durationMins = durationMs / 60000
    if (durationMins >= 2) return false
    if (!session.summary || session.summary_status !== 'completed') return true
    const content = session.summary.replace(/^#{1,6}\s+.*$/gm, '').trim()
    return content.length < 50
  }
  const visibleDates = $derived.by(() => {
    let filtered = dates
    if (dateFilter === 'today') {
      const today = new Date().toISOString().split('T')[0]
      filtered = dates.filter(d => d === today)
    } else if (dateFilter === 'week') {
      const weekAgo = new Date()
      weekAgo.setDate(weekAgo.getDate() - 7)
      const weekAgoStr = weekAgo.toISOString().split('T')[0]
      filtered = dates.filter(d => d >= weekAgoStr)
    }
    return filtered.slice(0, loadedDates)
  })

  const missingVisibleDates = $derived.by(() =>
    visibleDates.filter((date) => !sessionsByDate.has(date)),
  )

  $effect(() => {
    for (const date of missingVisibleDates) {
      void onLoadDate(date)
    }
  })

  function headingForDate(date: string): string {
    const target = new Date(`${date}T00:00:00`)
    const today = new Date()
    const todayOnly = new Date(today.getFullYear(), today.getMonth(), today.getDate())
    const delta = Math.floor((todayOnly.getTime() - target.getTime()) / 86400000)
    if (delta === 0) {
      return 'Today'
    }
    if (delta === 1) {
      return 'Yesterday'
    }
    return target.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric' })
  }

  function loadPreviousDates() {
    loadedDates = Math.min(dates.length, loadedDates + 3)
  }
</script>

  let searchQuery = $state('')
  let searchResults = $state<SearchResult[]>([])
  let searchTimeout: ReturnType<typeof setTimeout> | null = null
  let isSearching = $state(false)

  function handleSearchInput(e: Event) {
    const value = (e.target as HTMLInputElement).value
    searchQuery = value
    if (searchTimeout) clearTimeout(searchTimeout)
    if (!value.trim()) {
      searchResults = []
      isSearching = false
      return
    }
    isSearching = true
    searchTimeout = setTimeout(async () => {
      try {
        searchResults = await searchSessions(value)
      } catch {
        searchResults = []
      } finally {
        isSearching = false
      }
    }, 300)
  }

<section class="history-panel" data-testid="history-panel">
  <header class="panel-head">
    <h2>Session History</h2>
    <div class="filter-controls">
      <input type="text" class="search-input" placeholder="Search sessions..." value={searchQuery} oninput={handleSearchInput} data-testid="search-input" />
      <select bind:value={dateFilter} class="filter-select" data-testid="date-filter">
        <option value="all">All Time</option>
        <option value="today">Today</option>
        <option value="week">This Week</option>
      </select>
      <select bind:value={statusFilter} class="filter-select" data-testid="status-filter">
        <option value="all">All Status</option>
        <option value="active">Active</option>
        <option value="completed">Completed</option>
        <option value="failed">Failed</option>
      </select>
    </div>
    {#if selectedIds.size > 0}
      <div class="select-controls">
        <span class="selection-count">{selectedIds.size} selected</span>
        {#if selectedIds.size >= 2}
          <button type="button" class="merge-btn" onclick={handleMerge}>Merge</button>
        {/if}
        <button type="button" class="select-action-btn" onclick={exitSelectMode}>Cancel</button>
      </div>
    {/if}
  </header>

  {#if searchQuery.trim() && searchResults.length > 0}
    <div class="search-results" data-testid="search-results">
      <h3>Search Results ({searchResults.length})</h3>
      {#each searchResults as result (result.session_id)}
        <div class="search-result-item" data-testid="search-result">
          <a href="#" onclick={(e) => { e.preventDefault(); onToggleSession(result.session_id) }}>
            <strong>{result.title}</strong>
          </a>
          <p class="search-snippet">{@html result.snippet}</p>
        </div>
      {/each}
    </div>
  {:else if searchQuery.trim() && !isSearching}
    <div class="search-results empty" data-testid="search-results-empty">
      <p>No results found for "{searchQuery}"</p>
    </div>
  {/if}

  {#if dates.length === 0}
    <div class="empty-state">
      <p>No sessions yet.</p>
      <p>Start speaking and Ghost Wispr will group your transcript automatically.</p>
    </div>
  {/if}

  {#each visibleDates as date (date)}
    {@const allSessions = sessionsByDate.get(date) ?? []}
    {@const statusFilteredSessions = allSessions.filter(s => {
      if (statusFilter === 'all') return true
      if (statusFilter === 'active') return !s.ended_at
      if (statusFilter === 'completed') return s.summary_status === 'completed'
      if (statusFilter === 'failed') return s.summary_status === 'failed' || s.sync_status === 'failed' || s.refinement_status === 'failed'
      return true
    })}
    {@const hiddenCount = showHidden[date] ? 0 : statusFilteredSessions.filter(isShortSession).length}
    {@const visibleSessions = showHidden[date]
      ? statusFilteredSessions
      : statusFilteredSessions.filter((s) => !isShortSession(s))}
      : allSessions.filter((s) => !isShortSession(s))}
    <section class="date-group">
      <h3>{headingForDate(date)}</h3>

      {#if allSessions.length > 0}
        {#if visibleSessions.length > 0}
          <div class="card-stack">
            {#each visibleSessions as session (session.id)}
              <SessionCard
                {session}
                detail={sessionDetails.get(session.id)}
                expanded={expandedSessionId === session.id}
                {presets}
                selected={selectedIds.has(session.id)}
                onToggle={() => onToggleSession(session.id)}
                onToggleSelect={() => toggleSelect(session.id)}
                {onLoadDetail}
                {onResummarize}
                {onDelete}
              />
            {/each}
          </div>
        {/if}

        {#if hiddenCount > 0}
          <button
            type="button"
            class="show-hidden"
            onclick={() => (showHidden = { ...showHidden, [date]: true })}
          >
            {hiddenCount} short session{hiddenCount > 1 ? 's' : ''} hidden
          </button>
        {/if}
      {:else}
        <p class="date-loading">Loading {date}...</p>
      {/if}
    </section>
  {/each}

  {#if loadedDates < dates.length}
    <button type="button" class="load-more" onclick={loadPreviousDates}>Load previous</button>
  {/if}
</section>

<style>
  .filter-controls {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.5rem;
    align-items: center;
  }

  .search-input {
    flex: 1;
    padding: 0.25rem 0.5rem;
    border: 1px solid var(--line);
    border-radius: 4px;
    background: var(--surface);
    color: var(--text);
    font-size: 0.875rem;
  }

  .search-input:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .filter-select {
    padding: 0.25rem 0.5rem;
    border: 1px solid var(--line);
    border-radius: 4px;
    background: var(--surface);
    color: var(--text);
    font-size: 0.875rem;
  }
</style>
