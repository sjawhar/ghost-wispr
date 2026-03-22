<script lang="ts">
  import { onMount } from 'svelte'
  import { fetchConfig } from '../lib/config-api'
  import { configState, setConfig, setError } from '../lib/config.svelte'
  import GeneralSettings from './GeneralSettings.svelte'
  import PresetEditor from './PresetEditor.svelte'
  import IntegrationsSettings from './IntegrationsSettings.svelte'

  let { onBack }: { onBack: () => void } = $props()

  onMount(() => {
    const load = async () => {
      try {
        const config = await fetchConfig()
        setConfig(config)
      } catch (error) {
        setError(error instanceof Error ? error.message : 'Failed to load config')
      }
    }
    void load()
  })
</script>

<div class="settings-page" data-testid="settings-page">
  <header class="settings-header">
    <button class="back-btn" type="button" onclick={onBack}> ← Back </button>
    <h2>Settings</h2>
  </header>

  {#if configState.error}
    <p class="settings-error">{configState.error}</p>
  {/if}

  {#if !configState.loaded}
    <p class="settings-loading">Loading configuration...</p>
  {:else if configState.config}
    <section class="settings-section">
      <h3 class="section-heading">Summarization Presets</h3>
      <PresetEditor presets={configState.config.summarization.presets} />
    </section>

    <section class="settings-section">
      <h3 class="section-heading">General Settings</h3>
      <GeneralSettings />
    </section>

    <section class="settings-section">
      <h3 class="section-heading">Integrations</h3>
      <IntegrationsSettings config={configState.config} />
    </section>
  {/if}
</div>

<style>
  .settings-page {
    padding: 0.75rem;
  }

  .settings-header {
    display: flex;
    align-items: center;
    gap: 1rem;
    margin-bottom: 1rem;
  }

  .settings-header h2 {
    margin: 0;
    font-size: 1.15rem;
  }

  .back-btn {
    background: none;
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 0.25rem 0.65rem;
    cursor: pointer;
    font-family: inherit;
    color: var(--ink);
  }

  .back-btn:hover {
    background: var(--accent-soft);
  }

  .settings-error {
    color: var(--danger);
    padding: 0.5rem;
  }

  .settings-loading {
    color: var(--muted);
    font-style: italic;
  }

  .settings-section {
    background: var(--card);
    border: 1px solid var(--line);
    border-radius: 0.5rem;
    padding: 0.85rem;
    margin-bottom: 0.65rem;
  }

  .section-heading {
    font-size: 0.95rem;
    margin: 0 0 0.55rem;
    color: var(--ink);
  }

  .section-placeholder {
    color: var(--muted);
    font-style: italic;
    margin: 0;
  }
</style>
