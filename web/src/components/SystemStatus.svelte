<script lang="ts">
  import { onMount } from 'svelte'
  import { appState } from '../lib/state.svelte'
  import { fetchHealth } from '../lib/api'

  const deepgramStatus = $derived(appState.componentStatuses['deepgram'])
  const syncStatus = $derived(appState.componentStatuses['sync'])
  const micStatus = $derived(appState.componentStatuses['mic'])

  onMount(async () => {
    try {
      const health = await fetchHealth()
      // Map health response to componentStatuses format
      if (health.deepgram) {
        appState.componentStatuses['deepgram'] = {
          status: health.deepgram === 'connected' ? 'connected' : 'disconnected',
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
      if (health.mic) {
        appState.componentStatuses['mic'] = {
          status: health.mic === 'open' ? 'connected' : 'closed',
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
      if (health.db) {
        appState.componentStatuses['db'] = {
          status: health.db === 'ok' ? 'connected' : 'error',
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
      // Map db status to sync for display purposes
      if (health.db) {
        appState.componentStatuses['sync'] = {
          status: health.db === 'ok' ? 'synced' : 'error',
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
    } catch (error) {
      console.error('Failed to fetch health:', error)
    }
  })

  function getStatusColor(status?: string) {
    switch (status) {
      case 'connected':
        return 'var(--success)'
      case 'disconnected':
      case 'error':
        return 'var(--danger)'
      case 'reconnecting':
      case 'draining':
        return 'var(--warning)'
      default:
        return 'var(--muted)'
    }
  }

  const overallHealth = $derived.by(() => {
    const statuses = [deepgramStatus?.status, syncStatus?.status, micStatus?.status]
    const hasAnyStatus = statuses.some(s => s !== undefined)
    if (!hasAnyStatus) return 'loading'
    if (statuses.includes('error') || statuses.includes('disconnected')) return 'error'
    if (statuses.includes('reconnecting') || statuses.includes('draining')) return 'degraded'
    if (statuses.every(s => s === 'connected' || s === 'synced' || !s)) return 'healthy'
    return 'unknown'
  })

  function getHealthColor(health: string) {
    switch (health) {
      case 'healthy': return 'var(--success)'
      case 'degraded': return 'var(--warning)'
      case 'error': return 'var(--danger)'
      case 'loading': return 'var(--muted)'
      default: return 'var(--muted)'
    }
  }
</script>

<div class="system-status" data-testid="system-status-header">
  <div class="status-item" data-testid="status-overall">
    <span class="status-dot" style="background-color: {getHealthColor(overallHealth)}"></span>
    <span class="status-label">System: {overallHealth}</span>
  </div>
  <div class="status-divider">|</div>
  <div class="status-item" data-testid="status-deepgram">
    <span class="status-dot" style="background-color: {getStatusColor(deepgramStatus?.status)}"></span>
    <span class="status-label">Deepgram: {deepgramStatus?.status || 'unknown'}</span>
  </div>
  <div class="status-item" data-testid="status-sync">
    <span class="status-dot" style="background-color: {getStatusColor(syncStatus?.status)}"></span>
    <span class="status-label">Drive Sync: {syncStatus?.status || 'unknown'}</span>
  </div>
  <div class="status-item" data-testid="status-mic">
    <span class="status-dot" style="background-color: {getStatusColor(micStatus?.status)}"></span>
    <span class="status-label">Mic: {micStatus?.status || 'unknown'}</span>
  </div>
</div>

<style>
  .system-status {
    display: flex;
    gap: 1rem;
    padding: 0.5rem 1rem;
    background: var(--surface);
    border-bottom: 1px solid var(--line);
    font-size: 0.75rem;
    color: var(--muted);
  }

  .status-item {
    display: flex;
    align-items: center;
    gap: 0.375rem;
  }

  .status-divider {
    color: var(--line);
  }
  .status-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    display: inline-block;
  }

  .status-label {
    text-transform: capitalize;
  }
</style>
