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
</style>
