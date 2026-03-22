<script lang="ts">
  import type { ConfigResponse } from '../lib/config-api'
  import { patchConfig } from '../lib/config-api'
  import { setConfig } from '../lib/config.svelte'

  let {
    config,
  }: {
    config: ConfigResponse
  } = $props()

  const providers = ['deepgram', 'openai', 'anthropic', 'gemini'] as const

  let keyInputs = $state<Record<string, string>>({})
  let keySaving = $state<Record<string, boolean>>({})
  let keyFeedback = $state<Record<string, string>>({})

  let gdriveFolder = $state(config.gdrive.folder_id)
  let gdriveSaving = $state(false)
  let gdriveFeedback = $state('')

  let syncEnabled = $state(config.gdrive.sync_enabled)
  let syncSaving = $state(false)
  let syncFeedback = $state('')

  let gcEnabled = $state(config.gc.enabled)
  let gcMaxAgeDays = $state(config.gc.max_age_days)
  let gcMaxAudioSizeMB = $state(config.gc.max_audio_size_mb)
  let gcSaving = $state(false)
  let gcFeedback = $state('')

  async function saveKey(provider: string): Promise<void> {
    const value = keyInputs[provider]?.trim()
    if (!value) return

    keySaving[provider] = true
    keyFeedback[provider] = ''
    try {
      const updated = await patchConfig({ api_keys: { [provider]: value } })
      setConfig(updated)
      keyInputs[provider] = ''
      keyFeedback[provider] = 'Saved!'
    } catch (error) {
      keyFeedback[provider] = error instanceof Error ? error.message : 'Save failed'
    } finally {
      keySaving[provider] = false
    }
  }

  async function saveGDrive(): Promise<void> {
    gdriveSaving = true
    gdriveFeedback = ''
    try {
      const updated = await patchConfig({
        gdrive: { folder_id: gdriveFolder.trim() },
      })
      setConfig(updated)
      gdriveFeedback = 'Saved!'
    } catch (error) {
      gdriveFeedback = error instanceof Error ? error.message : 'Save failed'
    } finally {
      gdriveSaving = false
    }
  }

  async function saveSyncEnabled(): Promise<void> {
    syncSaving = true
    syncFeedback = ''
    try {
      const updated = await patchConfig({
        gdrive: { sync_enabled: syncEnabled },
      })
      setConfig(updated)
      syncFeedback = 'Saved!'
    } catch (error) {
      syncFeedback = error instanceof Error ? error.message : 'Save failed'
    } finally {
      syncSaving = false
    }
  }

  async function saveGC(): Promise<void> {
    gcSaving = true
    gcFeedback = ''
    try {
      const updated = await patchConfig({
        gc: {
          enabled: gcEnabled,
          max_age_days: gcMaxAgeDays,
          max_audio_size_mb: gcMaxAudioSizeMB,
        },
      })
      setConfig(updated)
      gcFeedback = 'Saved!'
    } catch (error) {
      gcFeedback = error instanceof Error ? error.message : 'Save failed'
    } finally {
      gcSaving = false
    }
  }
</script>

<div class="integrations" data-testid="integrations-settings">
  <h4>API Keys</h4>
  <div class="key-list">
    {#each providers as provider (provider)}
      <div class="key-row">
        <span class="key-status" class:active={config.api_keys[provider]}>
          {config.api_keys[provider] ? '●' : '○'}
        </span>
        <span class="key-name">{provider}</span>
        <input
          type="password"
          placeholder={config.api_keys[provider] ? '••••••••' : 'Not set'}
          bind:value={keyInputs[provider]}
        />
        <button
          class="save-btn"
          type="button"
          onclick={() => saveKey(provider)}
          disabled={keySaving[provider] || !keyInputs[provider]?.trim()}
        >
          {keySaving[provider] ? '...' : 'Save'}
        </button>
        {#if keyFeedback[provider]}
          <span class="feedback" class:error={keyFeedback[provider] !== 'Saved!'}
            >{keyFeedback[provider]}</span
          >
        {/if}
      </div>
    {/each}
  </div>

  <h4>Google Drive Sync</h4>
  <div class="gdrive-section">
    <div class="field">
      <label class="toggle-label">
        <input type="checkbox" bind:checked={syncEnabled} onchange={saveSyncEnabled} />
        Enable automatic sync
        {#if syncSaving}
          <span class="saving">Saving...</span>
        {/if}
        {#if syncFeedback}
          <span class="feedback" class:error={syncFeedback !== 'Saved!'}>{syncFeedback}</span>
        {/if}
      </label>
    </div>
    <div class="field">
      <label for="gdrive-folder">Folder ID</label>
      <input
        id="gdrive-folder"
        type="text"
        bind:value={gdriveFolder}
        placeholder="Google Drive folder ID"
      />
    </div>

    <div class="field">
      <label for="gdrive-creds">Service Account Credentials</label>
      <p class="hint">
        {#if config.gdrive.has_credentials}
          Credentials uploaded ✓
        {:else}
          Credential upload coming soon — set <code>GOOGLE_CREDENTIALS_FILE</code> env var for now.
        {/if}
      </p>
    </div>

    <div class="form-actions">
      <button class="save-btn" type="button" onclick={saveGDrive} disabled={gdriveSaving}>
        {gdriveSaving ? 'Saving...' : 'Save'}
      </button>
      {#if gdriveFeedback}
        <span
          class="feedback"
          class:error={!gdriveFeedback.includes('Saved') && !gdriveFeedback.includes('uploaded')}
          >{gdriveFeedback}</span
        >
      {/if}
    </div>
  </div>

  <h4>Garbage Collection</h4>
  <div class="gc-section">
    {#if gcEnabled && !config.gdrive.sync_enabled}
      <div class="warning-banner">
        ⚠️ Garbage collection will delete files without a Google Drive backup.
      </div>
    {/if}

    <div class="field">
      <label class="toggle-label">
        <input type="checkbox" bind:checked={gcEnabled} />
        Enable garbage collection
      </label>
    </div>

    {#if gcEnabled}
      <div class="field">
        <label for="gc-max-age">Delete synced sessions older than (days)</label>
        <input id="gc-max-age" type="number" min="1" bind:value={gcMaxAgeDays} />
      </div>
      <div class="field">
        <label for="gc-max-size">Max audio storage (MB)</label>
        <input id="gc-max-size" type="number" min="100" bind:value={gcMaxAudioSizeMB} />
      </div>
    {/if}

    <div class="form-actions">
      <button class="save-btn" type="button" onclick={saveGC} disabled={gcSaving}>
        {gcSaving ? 'Saving...' : 'Save'}
      </button>
      {#if gcFeedback}
        <span class="feedback" class:error={gcFeedback !== 'Saved!'}>{gcFeedback}</span>
      {/if}
    </div>
  </div>
</div>

<style>
  .integrations h4 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
  }

  .key-list {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    margin-bottom: 1.5rem;
  }

  .key-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }

  .key-status {
    font-size: 0.8rem;
    color: var(--muted);
  }

  .key-status.active {
    color: var(--accent);
  }

  .key-name {
    font-weight: 600;
    font-size: 0.85rem;
    min-width: 6rem;
    text-transform: capitalize;
  }

  .key-row input {
    flex: 1;
    min-width: 12rem;
    padding: 0.4rem 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    background: var(--panel);
  }

  .gdrive-section {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .field label {
    font-weight: 600;
    font-size: 0.85rem;
  }

  .field input[type='text'] {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    background: var(--panel);
  }

  .cred-row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }

  .cred-status {
    font-size: 0.8rem;
    color: var(--muted);
  }

  .cred-status.active {
    color: var(--accent);
  }

  .form-actions {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .save-btn {
    background: var(--accent);
    color: white;
    border: none;
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
    font-weight: 600;
  }

  .save-btn:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  .feedback {
    font-size: 0.85rem;
    color: var(--accent);
  }

  .feedback.error {
    color: var(--danger);
  }
  .gc-section {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }

  .toggle-label {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 600;
    font-size: 0.85rem;
    cursor: pointer;
  }

  .toggle-label input[type='checkbox'] {
    width: 1rem;
    height: 1rem;
    cursor: pointer;
  }

  .saving {
    font-size: 0.8rem;
    color: var(--muted);
    font-weight: normal;
  }

  .warning-banner {
    padding: 0.55rem;
    background: rgba(201, 160, 78, 0.08);
    border: 1px solid rgba(201, 160, 78, 0.2);
    border-radius: 0.35rem;
    font-size: 0.8rem;
    color: #c9a04e;
  }

  .field input[type='number'] {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    background: var(--panel);
    max-width: 8rem;
  }

  .hint {
    font-size: 0.8rem;
    color: var(--muted);
    margin: 0;
  }
</style>
