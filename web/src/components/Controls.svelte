<script lang="ts">
  let {
    connected,
    paused,
    activeSessionId,
    onToggle,
    onEndSession,
  }: {
    connected: boolean
    paused: boolean
    activeSessionId: string
    onToggle: () => Promise<void>
    onEndSession: () => Promise<void>
  } = $props()

  let busy = $state(false)
  let endBusy = $state(false)

  async function handleToggle() {
    if (busy) return
    busy = true
    try {
      await onToggle()
    } finally {
      busy = false
    }
  }

  async function handleEndSession() {
    if (endBusy) return
    endBusy = true
    try {
      await onEndSession()
    } finally {
      endBusy = false
    }
  }
</script>

<div class="controls" data-testid="controls-panel">
  <div class="status-wrap">
    <span class:connected class="status-dot"></span>
    <span class="status-text">{connected ? 'Connected' : 'Disconnected'}</span>
    <span class="state-pill">{paused ? 'Paused' : 'Listening'}</span>
  </div>

  <button class="toggle-btn" type="button" onclick={handleToggle} disabled={busy}>
    {#if paused}
      Resume
    {:else}
      Pause
    {/if}
  </button>

  {#if activeSessionId}
    <button class="end-btn" type="button" onclick={handleEndSession} disabled={endBusy}>
      End Session
    </button>
  {/if}
</div>
