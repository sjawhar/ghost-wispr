<script lang="ts">
  import type { ComponentStatus } from '../lib/state.svelte'

  let {
    connected,
    componentStatuses,
  }: {
    connected: boolean
    componentStatuses: Record<string, ComponentStatus>
  } = $props()

  const overallLevel = $derived.by(() => {
    if (!connected) return 'error'

    const statuses = Object.values(componentStatuses)
    if (statuses.some((s) => s.status === 'error' || s.status === 'disconnected')) return 'error'
    if (statuses.some((s) => s.status === 'reconnecting' || s.status === 'draining'))
      return 'warning'
    return 'ok'
  })

  const lastError = $derived.by(() => {
    if (!connected) return 'WebSocket disconnected'

    const errorStatuses = Object.entries(componentStatuses).filter(
      ([, s]) => s.status === 'error' || s.status === 'disconnected',
    )
    if (errorStatuses.length > 0) {
      return errorStatuses.map(([, s]) => s.message).join('; ')
    }

    const reconnecting = Object.entries(componentStatuses).filter(
      ([, s]) => s.status === 'reconnecting',
    )
    if (reconnecting.length > 0) {
      return reconnecting.map(([, s]) => s.message).join('; ')
    }

    return ''
  })

  const componentEntries = $derived(Object.entries(componentStatuses))

  function statusIcon(status: string): string {
    switch (status) {
      case 'connected':
        return '●'
      case 'disconnected':
      case 'error':
        return '○'
      case 'reconnecting':
      case 'draining':
        return '◐'
      default:
        return '?'
    }
  }

  function statusColor(status: string): string {
    switch (status) {
      case 'connected':
        return 'var(--status-green)'
      case 'reconnecting':
      case 'draining':
        return 'var(--status-yellow)'
      case 'disconnected':
      case 'error':
        return 'var(--status-red)'
      default:
        return 'var(--muted)'
    }
  }
</script>

{#if overallLevel !== 'ok'}
  <aside
    class="status-banner"
    class:status-error={overallLevel === 'error'}
    class:status-warning={overallLevel === 'warning'}
    data-testid="status-banner"
    role="status"
    aria-live="polite"
  >
    <div class="banner-main">
      <span class="banner-indicator" class:error={overallLevel === 'error'} class:warning={overallLevel === 'warning'}></span>
      <span class="banner-text">{lastError}</span>
    </div>

    {#if componentEntries.length > 0}
      <div class="component-breakdown" data-testid="component-breakdown">
        {#each componentEntries as [name, cs] (name)}
          <span class="component-chip" title={cs.message}>
            <span class="chip-dot" style="color: {statusColor(cs.status)}">{statusIcon(cs.status)}</span>
            <span class="chip-label">{name}</span>
          </span>
        {/each}
      </div>
    {/if}
  </aside>
{/if}

<style>
  .status-banner {
    --status-green: #22c55e;
    --status-yellow: #eab308;
    --status-red: #ef4444;

    display: flex;
    flex-direction: column;
    gap: 0.35rem;
    padding: 0.5rem 0.75rem;
    border-radius: 0.4rem;
    font-size: 0.82rem;
    line-height: 1.3;
  }

  .status-error {
    background: rgba(239, 68, 68, 0.12);
    border: 1px solid rgba(239, 68, 68, 0.35);
    color: #fca5a5;
  }

  .status-warning {
    background: rgba(234, 179, 8, 0.1);
    border: 1px solid rgba(234, 179, 8, 0.3);
    color: #fde68a;
  }

  .banner-main {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .banner-indicator {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .banner-indicator.error {
    background: var(--status-red);
    box-shadow: 0 0 6px rgba(239, 68, 68, 0.5);
  }

  .banner-indicator.warning {
    background: var(--status-yellow);
    box-shadow: 0 0 6px rgba(234, 179, 8, 0.4);
    animation: pulse-yellow 2s ease-in-out infinite;
  }

  @keyframes pulse-yellow {
    0%,
    100% {
      opacity: 1;
    }
    50% {
      opacity: 0.5;
    }
  }

  .banner-text {
    flex: 1;
  }

  .component-breakdown {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    padding-left: 1.1rem;
  }

  .component-chip {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    font-size: 0.75rem;
    opacity: 0.85;
  }

  .chip-dot {
    font-size: 0.65rem;
    line-height: 1;
  }

  .chip-label {
    text-transform: capitalize;
  }
</style>
