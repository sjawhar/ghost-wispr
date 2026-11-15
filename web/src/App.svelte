<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import ConsentRule from './components/ConsentRule.svelte'
  import Controls from './components/Controls.svelte'
  import SettingsPage from './components/SettingsPage.svelte'
  import LivePanel from './components/LivePanel.svelte'
  import SessionList from './components/SessionList.svelte'
  import StatusBanner from './components/StatusBanner.svelte'
  import SystemStatus from './components/SystemStatus.svelte'
  import LogViewer from './components/LogViewer.svelte'
  import { filterVisibleWarnings } from './lib/warnings'
  import {
    appState,
    applySegmentDeletion,
    beginSessionDetailRequest,
    removeSession,
    setCapturing,
    setDates,
    setLiveSegmentsForSession,
    setPaused,
    setPresets,
    setSessionDetailIfCurrent,
    setSessionsForDate,
    setWarnings,
  } from './lib/state.svelte'
  import {
    RequestError,
    endSession,
    fetchDates,
    fetchPresets,
    fetchSession,
    fetchSessions,
    fetchStatus,
    pauseRecording,
    resummarize,
    resumeRecording,
    deleteSession,
    mergeSessions,
    startSession,
    stopCurrentSession,
  } from './lib/api'
  import { connect, disconnect } from './lib/ws.svelte'

  let expandedSessionId = $state('')
  let loadingError = $state('')
  let currentView = $state<'main' | 'settings'>('main')

  const visibleWarnings = $derived(filterVisibleWarnings(appState.warnings))

  async function loadDate(date: string): Promise<void> {
    if (appState.sessionsByDate.has(date)) {
      return
    }

    const sessions = await fetchSessions(date)
    setSessionsForDate(date, sessions)
  }

  async function fetchAndStoreSession(id: string): Promise<void> {
    const requestVersion = beginSessionDetailRequest(id)
    const detail = await fetchSession(id)
    setSessionDetailIfCurrent(detail, requestVersion)
  }

  async function loadSession(id: string): Promise<void> {
    if (appState.sessionDetails.has(id)) {
      return
    }

    await fetchAndStoreSession(id)
  }

  async function togglePause(): Promise<void> {
    if (appState.paused) {
      await resumeRecording()
      // The microphone comes back a moment later; its mic status event, or the
      // next status poll, sets capturing.
      setPaused(false)
      return
    }

    await pauseRecording()
    setPaused(true)
    setCapturing(false)
  }

  async function handleStartSession(): Promise<void> {
    await startSession()
  }

  async function handleStopSession(): Promise<void> {
    await stopCurrentSession()
  }

  async function handleResummarize(sessionId: string, preset: string): Promise<void> {
    await resummarize(sessionId, preset)
  }

  function onToggleSession(id: string): void {
    expandedSessionId = expandedSessionId === id ? '' : id
    if (expandedSessionId) {
      void loadSession(expandedSessionId)
    }
  }

  async function handleDeleteSession(id: string): Promise<void> {
    await deleteSession(id)
    removeSession(id)
  }

  async function refreshSessionList(sessionIds: readonly string[]): Promise<void> {
    for (const [date, sessions] of appState.sessionsByDate) {
      if (sessions.some((session) => sessionIds.includes(session.id))) {
        setSessionsForDate(date, await fetchSessions(date))
      }
    }
  }

  async function handleRefreshSession(id: string): Promise<void> {
    await refreshSessionList([id])
    try {
      await fetchAndStoreSession(id)
    } catch (error) {
      if (!(error instanceof RequestError && error.status === 404)) {
        throw error
      }
      removeSession(id)
    }
  }

  function handleSegmentDeleted(sessionID: string, segmentID: number): void {
    applySegmentDeletion(sessionID, segmentID)
  }

  async function handleMerge(sessionIds: string[]): Promise<void> {
    await mergeSessions(sessionIds)
    await refreshSessionList(sessionIds)
  }

  onMount(() => {
    connect()

    let mounted = true
    const bootstrap = async () => {
      try {
        const [status, dates, presets] = await Promise.all([
          fetchStatus(),
          fetchDates(),
          fetchPresets(),
        ])
        if (!mounted) {
          return
        }

        setPaused(status.paused)
        setCapturing(status.capturing)
        setWarnings(status.warnings)
        setDates(dates)
        setPresets(presets)

        // Restore active session's live transcript if one exists
        if (status.active_session_id) {
          appState.activeSessionId = status.active_session_id
          appState.activeSessionStartedAt = Date.parse(status.active_session_started_at)
          setLiveSegmentsForSession(status.active_session_id, [])

          try {
            const requestVersion = beginSessionDetailRequest(status.active_session_id)
            const detail = await fetchSession(status.active_session_id)
            if (setSessionDetailIfCurrent(detail, requestVersion)) {
              setLiveSegmentsForSession(
                status.active_session_id,
                detail.segments.map((seg) => ({
                  type: 'live_transcript' as const,
                  version: 1,
                  timestamp: seg.timestamp,
                  session_id: status.active_session_id,
                  speaker: seg.speaker,
                  text: seg.text,
                  start_time: seg.start_time,
                  end_time: seg.end_time,
                })),
              )
            }
          } catch {
            // Session may have ended between status and detail fetch — that's OK
          }
        }

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
          setCapturing(status.capturing)
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
  <SystemStatus />
  <header class="hero">
    <div class="title-wrap">
      <p class="eyebrow">Realtime Transcript Appliance</p>
      <h1>Ghost Wispr</h1>
      <p class="subtitle">Live capture first, session memory second.</p>
    </div>
    <div class="header-actions">
      <ConsentRule />
      <Controls
        connected={appState.connected}
        paused={appState.paused}
        capturing={appState.capturing}
        activeSessionId={appState.activeSessionId}
        onToggle={togglePause}
        onEndSession={endSession}
        onStartSession={handleStartSession}
        onStopSession={handleStopSession}
      />
      <button
        class="settings-btn"
        type="button"
        title="Settings"
        data-testid="settings-toggle"
        onclick={() => {
          currentView = currentView === 'main' ? 'settings' : 'main'
        }}
      >
        &#9881;
      </button>
    </div>
  </header>

  {#if loadingError}
    <p class="load-error">{loadingError}</p>
  {/if}

  {#if currentView === 'settings'}
    <SettingsPage
      onBack={() => {
        currentView = 'main'
      }}
    />
  {:else}
    <StatusBanner connected={appState.connected} componentStatuses={appState.componentStatuses} />

    {#if visibleWarnings.length > 0}
      <aside class="warnings-banner" data-testid="warnings-banner">
        {#each visibleWarnings as warning (warning)}
          <p class="warning-item">{warning}</p>
        {/each}
      </aside>
    {/if}

    <section class="layout">
      <LivePanel
        segments={appState.liveSegments}
        connected={appState.connected}
        activeSessionStartedAt={appState.activeSessionStartedAt}
        interimText={appState.interimText}
        interimSpeaker={appState.interimSpeaker}
      />

      <SessionList
        dates={appState.dates}
        sessionsByDate={appState.sessionsByDate}
        sessionDetails={appState.sessionDetails}
        presets={appState.presets}
        {expandedSessionId}
        {onToggleSession}
        onLoadDate={loadDate}
        onLoadDetail={loadSession}
        onResummarize={handleResummarize}
        onDelete={handleDeleteSession}
        onMerge={handleMerge}
        onRefreshSession={handleRefreshSession}
        onSegmentDeleted={handleSegmentDeleted}
      />
    </section>
  {/if}

  <LogViewer />
</main>

<style>
  .header-actions {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }

  .settings-btn {
    background: none;
    border: 1px solid var(--line);
    border-radius: 0.4rem;
    padding: 0.3rem 0.5rem;
    font-size: 1.15rem;
    cursor: pointer;
    color: var(--muted);
    line-height: 1;
  }

  .settings-btn:hover {
    background: var(--accent-soft);
    color: var(--accent);
  }
</style>
