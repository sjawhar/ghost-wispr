<script lang="ts">
  import SessionCard from './SessionCard.svelte'
  import type { SessionDetailResponse, SessionSummary } from '../lib/types'

  let {
    dates,
    sessionsByDate,
    sessionDetails,
    expandedSessionId,
    onToggleSession,
    onLoadDate,
    onLoadDetail,
  }: {
    dates: string[]
    sessionsByDate: Map<string, SessionSummary[]>
    sessionDetails: Map<string, SessionDetailResponse>
    expandedSessionId: string
    onToggleSession: (id: string) => void
    onLoadDate: (date: string) => Promise<void>
    onLoadDetail: (id: string) => Promise<void>
  } = $props()

  let loadedDates = $state(3)

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
    <section class="date-group">
      <h3>{headingForDate(date)}</h3>

      {#if sessionsByDate.get(date)?.length}
        <div class="card-stack">
          {#each sessionsByDate.get(date) ?? [] as session (session.id)}
            <SessionCard
              {session}
              detail={sessionDetails.get(session.id)}
              expanded={expandedSessionId === session.id}
              onToggle={() => onToggleSession(session.id)}
              {onLoadDetail}
            />
          {/each}
        </div>
      {:else}
        <p class="date-loading">Loading {date}...</p>
      {/if}
    </section>
  {/each}

  {#if loadedDates < dates.length}
    <button type="button" class="load-more" onclick={loadPreviousDates}>Load previous</button>
  {/if}
</section>
