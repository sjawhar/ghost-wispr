<script lang="ts">
  let {
    connected,
    paused,
    onToggle,
  }: {
    connected: boolean
    paused: boolean
    onToggle: () => Promise<void>
  } = $props()

  let busy = $state(false)

  async function handleToggle() {
    if (busy) {
      return
    }

    busy = true
    try {
      await onToggle()
    } finally {
      busy = false
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
</div>
