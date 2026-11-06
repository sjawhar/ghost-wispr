<script lang="ts">
  import { onMount } from 'svelte'

  let {
    connected,
    paused,
    capturing,
    activeSessionId,
    onToggle,
    onEndSession,
    onStartSession,
    onStopSession,
  }: {
    connected: boolean
    paused: boolean
    capturing: boolean
    activeSessionId: string
    onToggle: () => Promise<void>
    onEndSession: () => Promise<void>
    onStartSession: () => Promise<void>
    onStopSession: () => Promise<void>
  } = $props()

  let busy = $state(false)
  let endBusy = $state(false)
  let recordBusy = $state(false)
  let recordError = $state('')

  const isRecording = $derived(!!activeSessionId)
  // A session can only start while the microphone is capturing: not while
  // paused, and not after a resume until the microphone is back.
  const recordBlocked = $derived(!isRecording && (paused || !capturing))
  const captureLabel = $derived(paused ? 'Paused' : capturing ? 'Listening' : 'Not capturing')

  // The API answers errors as {"error": "..."}; show that message.
  function errorText(err: unknown): string {
    const text = err instanceof Error ? err.message : String(err)
    try {
      const body: unknown = JSON.parse(text)
      if (body && typeof body === 'object' && 'error' in body && typeof body.error === 'string') {
        return body.error
      }
    } catch {
      // not JSON: show the text as it is
    }
    return text
  }

  async function handleToggle() {
    if (busy) return
    busy = true
    try {
      await onToggle()
    } catch (err) {
      console.error('Failed to toggle recording:', err)
    } finally {
      busy = false
    }
  }

  async function handleEndSession() {
    if (endBusy) return
    endBusy = true
    try {
      await onEndSession()
    } catch (err) {
      console.error('Failed to end session:', err)
    } finally {
      endBusy = false
    }
  }

  async function handleRecordToggle() {
    if (recordBusy || recordBlocked) return
    recordBusy = true
    recordError = ''
    try {
      if (isRecording) {
        await onStopSession()
      } else {
        await onStartSession()
      }
    } catch (err) {
      recordError = errorText(err)
    } finally {
      recordBusy = false
    }
  }

  onMount(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.ctrlKey && e.shiftKey && e.key === 'R') {
        e.preventDefault()
        void handleRecordToggle()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  })
</script>

<div class="controls" data-testid="controls-panel">
  <div class="status-wrap">
    <span class:connected class="status-dot"></span>
    <span class="status-text">{connected ? 'Connected' : 'Disconnected'}</span>
    <span class="state-pill" class:not-capturing={!paused && !capturing}>{captureLabel}</span>
  </div>

  <button
    class="record-btn"
    class:recording={isRecording}
    type="button"
    onclick={handleRecordToggle}
    disabled={recordBusy || recordBlocked}
    title={recordBlocked
      ? paused
        ? 'Resume capture to record'
        : 'The microphone is not capturing yet'
      : isRecording
        ? 'Stop Recording (Ctrl+Shift+R)'
        : 'Start Recording (Ctrl+Shift+R)'}
    data-testid="record-toggle"
  >
    <span class="record-indicator" class:pulsing={isRecording}></span>
    {isRecording ? 'Stop' : 'Record'}
  </button>

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

  {#if recordError}
    <span class="record-error" role="alert">{recordError}</span>
  {/if}
</div>

<style>
  .state-pill.not-capturing {
    border-color: var(--warning);
    color: var(--warning);
  }

  .record-error {
    color: var(--danger);
    font-size: 0.8rem;
  }

  .record-btn {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    padding: 0.35rem 0.7rem;
    border-radius: 0.4rem;
    font-size: 0.85rem;
    font-weight: 600;
    cursor: pointer;
    border: 1px solid var(--line);
    background: var(--bg);
    color: var(--text);
    transition:
      background 0.15s,
      border-color 0.15s;
  }

  .record-btn:hover {
    background: var(--accent-soft);
    border-color: var(--accent);
  }

  .record-btn.recording {
    background: #fee2e2;
    border-color: #ef4444;
    color: #dc2626;
  }

  .record-btn.recording:hover {
    background: #fecaca;
  }

  .record-indicator {
    display: inline-block;
    width: 0.6rem;
    height: 0.6rem;
    border-radius: 50%;
    background: #9ca3af;
    flex-shrink: 0;
  }

  .record-indicator.pulsing {
    background: #ef4444;
    animation: pulse 1.5s ease-in-out infinite;
  }

  @keyframes pulse {
    0%,
    100% {
      opacity: 1;
      transform: scale(1);
    }
    50% {
      opacity: 0.5;
      transform: scale(1.2);
    }
  }
</style>
