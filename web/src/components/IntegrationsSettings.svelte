<script lang="ts">
  import type { ConfigPatch, ConfigResponse } from '../lib/config-api'
  import { patchConfig } from '../lib/config-api'
  import { setConfig } from '../lib/config.svelte'
  import { unitField } from '../lib/unit-field.svelte'
  import ModelProviders from './ModelProviders.svelte'

  let {
    config,
  }: {
    config: ConfigResponse
  } = $props()

  const gdriveFolder = unitField(() => config.gdrive.folder_id)
  let gdriveSaving = $state(false)
  let gdriveFeedback = $state('')

  const syncEnabled = unitField(() => config.gdrive.sync_enabled)
  let syncSaving = $state(false)
  let syncFeedback = $state('')

  const gcEnabled = unitField(() => config.gc.enabled)
  const gcMaxAgeDays = unitField(() => config.gc.max_age_days)
  const gcMaxAudioSizeMB = unitField(() => config.gc.max_audio_size_mb)
  let gcSaving = $state(false)
  let gcFeedback = $state('')

  async function saveGDrive(): Promise<void> {
    const folder = gdriveFolder.value.trim()
    if (folder === config.gdrive.folder_id) {
      gdriveFeedback = 'No changes to save.'
      return
    }
    gdriveSaving = true
    gdriveFeedback = ''
    try {
      const updated = await patchConfig({
        gdrive: { folder_id: folder },
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
        gdrive: { sync_enabled: syncEnabled.value },
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
    // The unit takes a whole number of 0 or more for each limit. A cleared
    // field holds null, which it reads as no value, so a save would change
    // nothing while the page said "Saved!"; a fraction it refuses with a
    // parser error. While GC is on the limits show, so the page says what each
    // needs; while it is off they are hidden, and one that is not a limit is
    // not sent.
    const ageSet = Number.isSafeInteger(gcMaxAgeDays.value) && gcMaxAgeDays.value >= 0
    const sizeSet = Number.isSafeInteger(gcMaxAudioSizeMB.value) && gcMaxAudioSizeMB.value >= 0
    if (gcEnabled.value && (!ageSet || !sizeSet)) {
      gcFeedback = 'Each GC limit needs a whole number, 0 or more; 0 means no limit.'
      return
    }
    const gc: NonNullable<ConfigPatch['gc']> = {}
    if (gcEnabled.value !== config.gc.enabled) {
      gc.enabled = gcEnabled.value
    }
    if (ageSet && gcMaxAgeDays.value !== config.gc.max_age_days) {
      gc.max_age_days = gcMaxAgeDays.value
    }
    if (sizeSet && gcMaxAudioSizeMB.value !== config.gc.max_audio_size_mb) {
      gc.max_audio_size_mb = gcMaxAudioSizeMB.value
    }
    if (Object.keys(gc).length === 0) {
      gcFeedback = 'No changes to save.'
      return
    }
    gcSaving = true
    gcFeedback = ''
    try {
      const updated = await patchConfig({ gc })
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
  <ModelProviders {config} />

  <h4>Google Drive Sync</h4>
  <div class="gdrive-section">
    <div class="field">
      <label class="toggle-label">
        <input
          type="checkbox"
          bind:checked={syncEnabled.value}
          onchange={saveSyncEnabled}
          disabled={syncSaving}
        />
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
        bind:value={gdriveFolder.value}
        disabled={gdriveSaving}
        placeholder="Google Drive folder ID"
      />
    </div>

    <div class="field">
      <span class="field-label">Google Credentials</span>
      <p class="hint">
        {#if config.gdrive.has_credentials}
          Credentials configured ✓
        {:else}
          Set <code>google_credentials_file</code> in <code>ghost-wispr.yaml</code>.
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
          class:error={!gdriveFeedback.includes('Saved') &&
            gdriveFeedback !== 'No changes to save.' &&
            !gdriveFeedback.includes('uploaded')}>{gdriveFeedback}</span
        >
      {/if}
    </div>
  </div>

  <h4>Garbage Collection</h4>
  <div class="gc-section">
    {#if gcEnabled.value && !config.gdrive.sync_enabled}
      <div class="warning-banner">
        Garbage collection is on hold until Google Drive sync is enabled.
      </div>
    {/if}

    <div class="field">
      <label class="toggle-label">
        <input type="checkbox" bind:checked={gcEnabled.value} disabled={gcSaving} />
        Enable garbage collection
      </label>
    </div>

    {#if gcEnabled.value}
      <div class="field">
        <label for="gc-max-age">Delete sessions older than (days)</label>
        <input
          id="gc-max-age"
          type="number"
          min="0"
          bind:value={gcMaxAgeDays.value}
          disabled={gcSaving}
        />
        <p class="hint">
          0 = never. Deletes only ended sessions that have a verified copy in Google Drive. A
          session restored from Drive counts from its restore.
        </p>
      </div>
      <div class="field">
        <label for="gc-max-size">Audio budget (MB)</label>
        <input
          id="gc-max-size"
          type="number"
          min="0"
          bind:value={gcMaxAudioSizeMB.value}
          disabled={gcSaving}
        />
        <p class="hint">
          0 = no budget: GC removes no audio to free space. Past the budget, the oldest verified
          Drive copies' local audio is removed; sessions, transcripts and summaries are kept.
          Recordings without a verified Drive copy are not counted or removed.
        </p>
      </div>
    {/if}

    <div class="form-actions">
      <button class="save-btn" type="button" onclick={saveGC} disabled={gcSaving}>
        {gcSaving ? 'Saving...' : 'Save'}
      </button>
      {#if gcFeedback}
        <span
          class="feedback"
          class:error={gcFeedback !== 'Saved!' && gcFeedback !== 'No changes to save.'}
          >{gcFeedback}</span
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

  .field label,
  .field-label {
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
