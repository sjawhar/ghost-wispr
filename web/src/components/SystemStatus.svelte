<script lang="ts">
  import { onMount } from 'svelte'
  import { appState } from '../lib/state.svelte'
  import { fetchHealth } from '../lib/api'
  import { ComponentStatus, HealthStatus } from '../lib/types'

  const deepgramStatus = $derived(appState.componentStatuses['deepgram'])
  const syncStatus = $derived(appState.componentStatuses['sync'])
  const micStatus = $derived(appState.componentStatuses['mic'])

  onMount(async () => {
    try {
      const health = await fetchHealth()
      // Map health response to componentStatuses format
      if (health.deepgram) {
        appState.componentStatuses['deepgram'] = {
          status:
            health.deepgram === ComponentStatus.Connected
              ? ComponentStatus.Connected
              : ComponentStatus.Disconnected,
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
      if (health.mic) {
        appState.componentStatuses['mic'] = {
          status:
            health.mic === ComponentStatus.Open
              ? ComponentStatus.Connected
              : ComponentStatus.Closed,
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
      if (health.db) {
        appState.componentStatuses['db'] = {
          status:
            health.db === ComponentStatus.Ok ? ComponentStatus.Connected : ComponentStatus.Error,
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
      // Map db status to sync for display purposes
      if (health.db) {
        appState.componentStatuses['sync'] = {
          status: health.db === ComponentStatus.Ok ? ComponentStatus.Synced : ComponentStatus.Error,
          message: '',
          timestamp: new Date().toISOString(),
        }
      }
    } catch (error) {
      console.error('Failed to fetch health:', error)
    }
  })

  function getStatusColor(status?: ComponentStatus) {
    switch (status) {
      case ComponentStatus.Connected:
      case ComponentStatus.Synced:
      case ComponentStatus.Open:
      case ComponentStatus.Ok:
        return 'var(--success)'
      case ComponentStatus.Disconnected:
      case ComponentStatus.Error:
      case ComponentStatus.Closed:
        return 'var(--danger)'
      case ComponentStatus.Reconnecting:
      case ComponentStatus.Draining:
        return 'var(--warning)'
      default:
        return 'var(--muted)'
    }
  }

  const overallHealth = $derived.by(() => {
    const statuses = [deepgramStatus?.status, syncStatus?.status, micStatus?.status]
    const hasAnyStatus = statuses.some((s) => s !== undefined)
    if (!hasAnyStatus) return HealthStatus.Loading
    if (statuses.includes(ComponentStatus.Error) || statuses.includes(ComponentStatus.Disconnected))
      return HealthStatus.Error
    if (
      statuses.includes(ComponentStatus.Reconnecting) ||
      statuses.includes(ComponentStatus.Draining)
    )
      return HealthStatus.Degraded
    if (
      statuses.every((s) => s === ComponentStatus.Connected || s === ComponentStatus.Synced || !s)
    )
      return HealthStatus.Healthy
    return 'unknown'
  })

  function getHealthColor(health: HealthStatus | 'unknown') {
    switch (health) {
      case HealthStatus.Healthy:
        return 'var(--success)'
      case HealthStatus.Degraded:
        return 'var(--warning)'
      case HealthStatus.Error:
        return 'var(--danger)'
      case HealthStatus.Loading:
        return 'var(--muted)'
      default:
        return 'var(--muted)'
    }
  }
</script>

<div class="system-status" data-testid="system-status-header">
  <div class="status-item" data-testid="status-overall">
    <span class="status-dot" style="background-color: {getHealthColor(overallHealth)}"></span>
    <span class="status-label" style="color: {getHealthColor(overallHealth)}"
      >System: {overallHealth}</span
    >
  </div>
  <div class="status-divider">|</div>
  <div class="status-item" data-testid="status-deepgram">
    <span class="status-dot" style="background-color: {getStatusColor(deepgramStatus?.status)}"
    ></span>
    <span class="status-label" style="color: {getStatusColor(deepgramStatus?.status)}"
      >Deepgram: {deepgramStatus?.status || 'unknown'}</span
    >
  </div>
  <div class="status-item" data-testid="status-sync">
    <span class="status-dot" style="background-color: {getStatusColor(syncStatus?.status)}"></span>
    <span class="status-label" style="color: {getStatusColor(syncStatus?.status)}"
      >Drive Sync: {syncStatus?.status || 'unknown'}</span
    >
  </div>
  <div class="status-item" data-testid="status-mic">
    <span class="status-dot" style="background-color: {getStatusColor(micStatus?.status)}"></span>
    <span class="status-label" style="color: {getStatusColor(micStatus?.status)}"
      >Mic: {micStatus?.status || 'unknown'}</span
    >
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
