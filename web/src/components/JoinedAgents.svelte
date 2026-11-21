<script lang="ts">
  import { appState } from '../lib/state.svelte'

  // The recording session whose joined agents this lists (R17).
  let { sessionId }: { sessionId: string } = $props()

  const joined = $derived(appState.joinedAgents.get(sessionId))

  function clock(at: string): string {
    return new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }
</script>

{#if joined && (joined.agents.length > 0 || joined.error)}
  <section class="joined-agents" aria-label="Agents in the meeting">
    <div class="section-header">
      <span class="section-label">Agents in the meeting</span>
    </div>
    {#if joined.error}
      <p class="joined-agents-error" role="alert">The agents could not be read: {joined.error}</p>
    {/if}
    <ul class="joined-agent-list">
      {#each joined.agents as agent (agent.agent)}
        <li class="joined-agent">
          {#if agent.issue_key}
            <span class="joined-agent-issue">{agent.issue_key}</span>
          {/if}
          <span class="joined-agent-title">{agent.title}</span>
          <span class="joined-agent-time">
            joined <time datetime={agent.joined_at}>{clock(agent.joined_at)}</time>
          </span>
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .joined-agents {
    margin-top: 0.55rem;
  }

  .joined-agents-error {
    margin: 0.25rem 0;
    font-size: 0.8rem;
    color: var(--danger);
  }

  .joined-agent-list {
    list-style: none;
    margin: 0.25rem 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }

  .joined-agent {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0.5rem;
    font-size: 0.85rem;
  }

  .joined-agent-issue {
    font-weight: 600;
  }

  .joined-agent-title {
    overflow-wrap: anywhere;
  }

  .joined-agent-time {
    font-size: 0.75rem;
    color: var(--muted);
  }
</style>
