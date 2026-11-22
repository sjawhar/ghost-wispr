<script lang="ts">
  import type { ConfigPatch, ConfigResponse } from '../lib/config-api'
  import { patchConfig } from '../lib/config-api'
  import { setConfig, setError } from '../lib/config.svelte'
  import { unitField, type UnitField } from '../lib/unit-field.svelte'
  import ReplacedNote from './ReplacedNote.svelte'

  let {
    config,
  }: {
    config: ConfigResponse
  } = $props()

  const taken = () => config
  const silenceTimeout = unitField(() => config.silence_timeout, taken)
  const model = unitField(() => config.summarization.model, taken)
  const endpointing = unitField(() => config.transcription.endpointing, taken)
  const utteranceEndMs = unitField(() => config.transcription.utterance_end_ms, taken)

  let saving = $state(false)
  let feedback = $state('')

  async function save(): Promise<void> {
    saving = true
    feedback = ''
    try {
      const patch: ConfigPatch = {}
      // Each field the save carries, acknowledged once the unit takes it.
      const carried: Pick<UnitField<unknown>, 'acknowledge'>[] = []

      if (silenceTimeout.edited) {
        patch.silence_timeout = silenceTimeout.value
        carried.push(silenceTimeout)
      }
      if (model.edited) {
        patch.summarization = { model: model.value }
        carried.push(model)
      }
      const transcription: NonNullable<ConfigPatch['transcription']> = {}
      if (endpointing.edited) {
        transcription.endpointing = endpointing.value
        carried.push(endpointing)
      }
      if (utteranceEndMs.edited) {
        transcription.utterance_end_ms = utteranceEndMs.value
        carried.push(utteranceEndMs)
      }
      if (Object.keys(transcription).length > 0) {
        patch.transcription = transcription
      }
      if (carried.length === 0) {
        feedback = 'No changes to save.'
        return
      }

      const updated = await patchConfig(patch)
      const take = setConfig(updated)
      for (const field of carried) field.acknowledge(take)
      feedback = 'Saved!'
    } catch (error) {
      feedback = error instanceof Error ? error.message : 'Save failed'
      setError(feedback)
    } finally {
      saving = false
    }
  }
</script>

<div class="general-settings" data-testid="general-settings">
  <div class="field">
    <label for="silence-timeout">Silence Timeout</label>
    <input
      id="silence-timeout"
      type="text"
      bind:value={silenceTimeout.value}
      disabled={saving}
      placeholder="30s"
    />
    <ReplacedNote shown={silenceTimeout.replaced} />
    <span class="hint">Go duration format (e.g. 30s, 1m, 1m30s)</span>
  </div>

  <div class="field">
    <label for="default-model">Default Summarization Model</label>
    <input
      id="default-model"
      type="text"
      bind:value={model.value}
      disabled={saving}
      placeholder="openai/gpt-4o-mini"
    />
    <ReplacedNote shown={model.replaced} />
    <span class="hint">Format: provider/model_name</span>
  </div>

  <div class="field">
    <label for="endpointing">Transcription Endpointing (ms)</label>
    <input
      id="endpointing"
      type="text"
      bind:value={endpointing.value}
      disabled={saving}
      placeholder="400"
    />
    <ReplacedNote shown={endpointing.replaced} />
  </div>

  <div class="field">
    <label for="utterance-end">Utterance End (ms)</label>
    <input
      id="utterance-end"
      type="text"
      bind:value={utteranceEndMs.value}
      disabled={saving}
      placeholder="1000"
    />
    <ReplacedNote shown={utteranceEndMs.replaced} />
    <span class="hint">Changes to transcription settings will reconnect Deepgram</span>
  </div>

  <div class="actions">
    <button class="save-btn" type="button" onclick={save} disabled={saving}>
      {saving ? 'Saving...' : 'Save'}
    </button>
    {#if feedback}
      <span
        class="feedback"
        class:error={feedback !== 'Saved!' && feedback !== 'No changes to save.'}>{feedback}</span
      >
    {/if}
  </div>
</div>

<style>
  .general-settings {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .field label {
    font-weight: 600;
    font-size: 0.85rem;
    color: var(--ink);
  }

  .field input {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.9rem;
    background: var(--panel);
  }

  .hint {
    font-size: 0.75rem;
    color: var(--muted);
  }

  .actions {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }

  .save-btn {
    background: var(--accent);
    color: white;
    border: none;
    border-radius: 6px;
    padding: 0.5rem 1.2rem;
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
