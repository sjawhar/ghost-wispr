<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import Controls from './components/Controls.svelte'
  import LivePanel from './components/LivePanel.svelte'
  import SessionList from './components/SessionList.svelte'
  import {
    appState,
    setDates,
    setPaused,
    setSessionDetail,
    setSessionsForDate,
    setWarnings,
  } from './lib/state.svelte'
  import {
    fetchDates,
    fetchSession,
    fetchSessions,
    fetchStatus,
    pauseRecording,
    resumeRecording,
  } from './lib/api'
  import { connect, disconnect } from './lib/ws.svelte'

  let expandedSessionId = $state('')
  let loadingError = $state('')

  async function loadDate(date: string): Promise<void> {
    if (appState.sessionsByDate.has(date)) {
      return
    }

    const sessions = await fetchSessions(date)
    setSessionsForDate(date, sessions)
  }

  async function loadSession(id: string): Promise<void> {
    if (appState.sessionDetails.has(id)) {
      return
    }

    const detail = await fetchSession(id)
    setSessionDetail(detail)
  }

  async function togglePause(): Promise<void> {
    if (appState.paused) {
      await resumeRecording()
      setPaused(false)
      return
    }

    await pauseRecording()
    setPaused(true)
  }

  function onToggleSession(id: string): void {
    expandedSessionId = expandedSessionId === id ? '' : id
    if (expandedSessionId) {
      void loadSession(expandedSessionId)
    }
  }

  onMount(() => {
    connect()

    let mounted = true
    const bootstrap = async () => {
      try {
        const [status, dates] = await Promise.all([fetchStatus(), fetchDates()])
        if (!mounted) {
          return
        }

        setPaused(status.paused)
        setWarnings(status.warnings)
        setDates(dates)

        for (const date of dates.slice(0, 3)) {
          await loadDate(date)
        }
      } catch (error) {
        loadingError = error instanceof Error ? error.message : 'Failed to load app data'
      }
    }

    void bootstrap()

    const refreshTimer = setInterval(() => {
      void fetchStatus()
        .then((status) => {
          setPaused(status.paused)
          setWarnings(status.warnings)
        })
        .catch((error) => {
          void error
        })
    }, 5000)

    return () => {
      mounted = false
      clearInterval(refreshTimer)
      disconnect()
    }
  })

  onDestroy(() => {
    disconnect()
  })
</script>

<main class="app-shell">
  <header class="hero">
    <div class="title-wrap">
      <p class="eyebrow">Realtime Transcript Appliance</p>
      <h1>Ghost Wispr</h1>
      <p class="subtitle">Live capture first, session memory second.</p>
    </div>
    <Controls connected={appState.connected} paused={appState.paused} onToggle={togglePause} />
  </header>

  {#if loadingError}
    <p class="load-error">{loadingError}</p>
  {/if}

  {#if appState.warnings.length > 0}
    <aside class="warnings-banner" data-testid="warnings-banner">
      {#each appState.warnings as warning (warning)}
        <p class="warning-item">{warning}</p>
      {/each}
    </aside>
  {/if}

  <section class="layout">
    <LivePanel
      segments={appState.liveSegments}
      connected={appState.connected}
      activeSessionStartedAt={appState.activeSessionStartedAt}
    />

    <SessionList
      dates={appState.dates}
      sessionsByDate={appState.sessionsByDate}
      sessionDetails={appState.sessionDetails}
      {expandedSessionId}
      {onToggleSession}
      onLoadDate={loadDate}
      onLoadDetail={loadSession}
    />
  </section>
</main>
