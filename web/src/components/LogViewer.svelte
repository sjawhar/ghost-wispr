<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { fetchLogs, type LogEntry } from '../lib/api'

  let isOpen = $state(false)
  let logs = $state<LogEntry[]>([])
  let filterLevel = $state('ALL')
  let autoScroll = $state(true)
  let logContainer = $state<HTMLElement | null>(null)
  let pollInterval: number | null = null

  const levels = ['ALL', 'ERROR', 'WARN', 'INFO', 'DEBUG']

  async function loadLogs() {
    if (!isOpen) return
    try {
      logs = await fetchLogs(filterLevel, 100)
      if (autoScroll && logContainer) {
        setTimeout(() => {
          if (logContainer) logContainer.scrollTop = logContainer.scrollHeight
        }, 0)
      }
    } catch (e) {
      console.error('Failed to load logs', e)
    }
  }

  function toggle() {
    isOpen = !isOpen
    if (isOpen) {
      loadLogs()
      pollInterval = window.setInterval(loadLogs, 5000)
    } else {
      if (pollInterval) {
        clearInterval(pollInterval)
        pollInterval = null
      }
    }
  }

  function setFilter(level: string) {
    filterLevel = level
    loadLogs()
  }

  function handleScroll() {
    if (!logContainer) return
    const { scrollTop, scrollHeight, clientHeight } = logContainer
    const isAtBottom = scrollHeight - scrollTop - clientHeight < 10
    autoScroll = isAtBottom
  }

  onMount(() => {
    return () => {
      if (pollInterval) clearInterval(pollInterval)
    }
  })

  function getLevelColor(level: string) {
    switch (level.toUpperCase()) {
      case 'ERROR': return 'var(--danger)'
      case 'WARN': return 'var(--warning)'
      case 'INFO': return 'var(--success)'
      case 'DEBUG': return 'var(--muted)'
      default: return 'var(--muted)'
    }
  }
</script>

<div class="log-viewer" class:open={isOpen} data-testid="log-viewer">
  <div class="log-header" role="button" tabindex="0" onclick={toggle} onkeydown={(e) => e.key === 'Enter' && toggle()} data-testid="log-viewer-toggle">
    <span class="title">System Logs</span>
    <span class="toggle-icon">{isOpen ? '▼' : '▲'}</span>
  </div>

  {#if isOpen}
    <div class="log-controls">
      <div class="filters">
        {#each levels as level}
          <button
            class="filter-btn"
            class:active={filterLevel === level}
            onclick={() => setFilter(level)}
            data-testid={`log-filter-${level.toLowerCase()}`}
          >
            {level}
          </button>
        {/each}
      </div>
      <label class="auto-scroll">
        <input type="checkbox" bind:checked={autoScroll} />
        Auto-scroll
      </label>
    </div>

    <div class="log-content" bind:this={logContainer} onscroll={handleScroll}>
      {#each logs as log}
        <div class="log-entry" data-testid="log-entry">
          <span class="log-time">{new Date(log.timestamp).toLocaleTimeString()}</span>
          <span class="log-level" style="color: {getLevelColor(log.level)}">[{log.level}]</span>
          {#if log.module}
            <span class="log-module">[{log.module}]</span>
          {/if}
          <span class="log-message">{log.message}</span>
        </div>
      {/each}
      {#if logs.length === 0}
        <div class="no-logs">No logs found</div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .log-viewer {
    position: fixed;
    bottom: 0;
    left: 0;
    right: 0;
    background: var(--surface);
    border-top: 1px solid var(--line);
    z-index: 100;
    display: flex;
    flex-direction: column;
    max-height: 50vh;
  }

  .log-header {
    padding: 0.5rem 1rem;
    display: flex;
    justify-content: space-between;
    align-items: center;
    cursor: pointer;
    background: var(--surface-hover);
    font-weight: 500;
    font-size: 0.875rem;
  }

  .log-controls {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 0.5rem 1rem;
    border-bottom: 1px solid var(--line);
    background: var(--bg);
  }

  .filters {
    display: flex;
    gap: 0.5rem;
  }

  .filter-btn {
    background: transparent;
    border: 1px solid var(--line);
    border-radius: 4px;
    padding: 0.25rem 0.5rem;
    font-size: 0.75rem;
    cursor: pointer;
    color: var(--text);
  }

  .filter-btn.active {
    background: var(--accent);
    color: white;
    border-color: var(--accent);
  }

  .auto-scroll {
    font-size: 0.75rem;
    display: flex;
    align-items: center;
    gap: 0.25rem;
  }

  .log-content {
    flex: 1;
    overflow-y: auto;
    padding: 0.5rem 1rem;
    font-family: monospace;
    font-size: 0.75rem;
    background: #1e1e1e;
    color: #d4d4d4;
    min-height: 200px;
  }

  .log-entry {
    margin-bottom: 0.25rem;
    word-break: break-all;
  }

  .log-time {
    color: #858585;
    margin-right: 0.5rem;
  }

  .log-level {
    font-weight: bold;
    margin-right: 0.5rem;
  }

  .log-module {
    color: #569cd6;
    margin-right: 0.5rem;
  }

  .log-message {
    color: #d4d4d4;
  }

  .no-logs {
    color: #858585;
    font-style: italic;
    text-align: center;
    padding: 1rem;
  }
</style>
