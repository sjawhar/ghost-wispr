<script lang="ts">
  import SessionCard from './SessionCard.svelte'
  import type { PresetMap, SessionDetailResponse, SessionSummary } from '../lib/types'

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
  } = $props()

  let loadedDates = $state(3)
  let showHidden = $state<Record<string, boolean>>({})

  function isShortSession(session: SessionSummary): boolean {
    if (!session.ended_at) return false
    const durationMs = Date.parse(session.ended_at) - Date.parse(session.started_at)
    const durationMins = durationMs / 60000
    if (durationMins >= 2) return false
    if (!session.summary || session.summary_status !== 'completed') return true
    const content = session.summary.replace(/^#{1,6}\s+.*$/gm, '').trim()
    return content.length < 50
  }
  const visibleDates = $derived.by(() => dates.slice(0, loadedDates))
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

<section class="history-panel" data-testid="history-panel">
  <header class="panel-head">
    <h2>Session History</h2>
  </header>

  {#if dates.length === 0}
    <div class="empty-state">
      <p>No sessions yet.</p>
      <p>Start speaking and Ghost Wispr will group your transcript automatically.</p>
    </div>
  {/if}

  {#each visibleDates as date (date)}
    {@const allSessions = sessionsByDate.get(date) ?? []}
    {@const hiddenCount = showHidden[date] ? 0 : allSessions.filter(isShortSession).length}
    {@const visibleSessions = showHidden[date] ? allSessions : allSessions.filter((s) => !isShortSession(s))}
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
                onToggle={() => onToggleSession(session.id)}
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
