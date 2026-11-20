<script lang="ts">
  import type { SessionVoiceTasks } from '../lib/state.svelte'
  import { VoiceTaskState } from '../lib/types'

  // The session's questions to agents, undefined until they are read.
  let { voiceTasks }: { voiceTasks: SessionVoiceTasks | undefined } = $props()

  const stateWords: Record<string, string> = {
    [VoiceTaskState.Working]: 'waiting',
    [VoiceTaskState.Done]: 'answered',
    [VoiceTaskState.Cancelled]: 'cancelled',
    [VoiceTaskState.Replaced]: 'replaced',
  }

  function clock(at: string): string {
    return new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }
</script>

{#if voiceTasks && (voiceTasks.tasks.length > 0 || voiceTasks.error)}
  <section class="voice-tasks" aria-label="Questions to agents">
    <div class="section-header">
      <span class="section-label">Questions to agents</span>
    </div>
    {#if voiceTasks.error}
      <p class="voice-tasks-error" role="alert">
        The questions could not be read: {voiceTasks.error}
      </p>
    {/if}
    <ol class="voice-task-list">
      {#each voiceTasks.tasks as task (task.id)}
        <li class="voice-task" data-testid="voice-task">
          <div class="voice-task-head">
            <span class="voice-task-issue">{task.issue_key}</span>
            <span class="voice-task-holder">{task.holder}</span>
            <span class={`voice-task-state ${task.state}`}>
              {stateWords[task.state] ?? task.state}
            </span>
          </div>
          <p class="voice-task-times">
            Asked <time datetime={task.asked_at}>{clock(task.asked_at)}</time>,
            {#if task.spoken_at}
              spoken <time datetime={task.spoken_at}>{clock(task.spoken_at)}</time>
            {:else}
              not spoken
            {/if}
          </p>
          {#if task.answer_written}
            <p class="voice-task-answer" data-testid="voice-task-answer">{task.answer_written}</p>
          {:else}
            <p class="voice-task-no-answer">
              {task.state === VoiceTaskState.Working ? 'No answer yet' : 'No answer'}
            </p>
          {/if}
        </li>
      {/each}
    </ol>
  </section>
{/if}

<style>
  .voice-tasks {
    margin-top: 0.55rem;
  }

  .voice-tasks-error {
    margin: 0.25rem 0;
    font-size: 0.8rem;
    color: var(--danger);
  }

  .voice-task-list {
    list-style: none;
    margin: 0.25rem 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }

  .voice-task {
    border: 1px solid var(--line);
    border-radius: 0.4rem;
    padding: 0.5rem 0.6rem;
  }

  .voice-task-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.8rem;
  }

  .voice-task-issue {
    font-weight: 600;
  }

  .voice-task-holder {
    color: var(--muted);
    overflow-wrap: anywhere;
  }

  .voice-task-state {
    font-size: 0.65rem;
    padding: 0.15rem 0.4rem;
    border-radius: 1rem;
    text-transform: uppercase;
    font-weight: 600;
    letter-spacing: 0.02em;
    background: var(--surface-hover);
    color: var(--muted);
  }

  .voice-task-state.working {
    background: var(--warning-soft);
    color: var(--warning);
  }

  .voice-task-state.done {
    background: var(--success-soft);
    color: var(--success);
  }

  .voice-task-times {
    margin: 0.25rem 0 0;
    font-size: 0.75rem;
    color: var(--muted);
  }

  .voice-task-answer,
  .voice-task-no-answer {
    margin: 0.35rem 0 0;
    font-size: 0.85rem;
  }

  /* The agent's text as it wrote it, line breaks kept. */
  .voice-task-answer {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .voice-task-no-answer {
    color: var(--muted);
  }
</style>
