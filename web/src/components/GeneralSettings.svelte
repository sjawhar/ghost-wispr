<script lang="ts">
  import { patchConfig } from '../lib/config-api'
  import { configState, setConfig, setError } from '../lib/config.svelte'

  let config = $derived(configState.config!)

  let silenceTimeout = $state(config.silence_timeout)
  let model = $state(config.summarization.model)
  let endpointing = $state(config.transcription.endpointing)
  let utteranceEndMs = $state(config.transcription.utterance_end_ms)
  let keywords = $state((config.transcription.keywords ?? []).join(', '))

  let saving = $state(false)
  let feedback = $state('')

  $effect(() => {
    if (configState.config) {
      silenceTimeout = configState.config.silence_timeout
      model = configState.config.summarization.model
      endpointing = configState.config.transcription.endpointing
      utteranceEndMs = configState.config.transcription.utterance_end_ms
      keywords = (configState.config.transcription.keywords ?? []).join(', ')
    }
  })

  function parseKeywords(raw: string): string[] {
    return raw
      .split(',')
      .map((keyword) => keyword.trim())
      .filter((keyword) => keyword.length > 0)
  }

  async function save(): Promise<void> {
    saving = true
    feedback = ''
    try {
      const patch: Record<string, unknown> = {}

      if (silenceTimeout !== config.silence_timeout) {
        patch.silence_timeout = silenceTimeout
      }
      if (model !== config.summarization.model) {
        patch.summarization = { model }
      }
      if (
        endpointing !== config.transcription.endpointing ||
        utteranceEndMs !== config.transcription.utterance_end_ms
      ) {
        patch.transcription = { endpointing, utterance_end_ms: utteranceEndMs }
      }

      const parsedKeywords = parseKeywords(keywords)
      const currentKeywords = config.transcription.keywords ?? []
      if (JSON.stringify(parsedKeywords) !== JSON.stringify(currentKeywords)) {
        const transcriptionPatch = (patch.transcription as Record<string, unknown>) ?? {}
        transcriptionPatch.keywords = parsedKeywords
        patch.transcription = transcriptionPatch
      }

      if (Object.keys(patch).length === 0) {
        feedback = 'No changes to save.'
        return
      }

      const updated = await patchConfig(patch)
      setConfig(updated)
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
    <input id="silence-timeout" type="text" bind:value={silenceTimeout} placeholder="30s" />
    <span class="hint">Go duration format (e.g. 30s, 1m, 1m30s)</span>
  </div>

  <div class="field">
    <label for="default-model">Default Summarization Model</label>
    <input id="default-model" type="text" bind:value={model} placeholder="openai/gpt-4o-mini" />
    <span class="hint">Format: provider/model_name</span>
  </div>

  <div class="field">
    <label for="endpointing">Transcription Endpointing (ms)</label>
    <input id="endpointing" type="text" bind:value={endpointing} placeholder="400" />
  </div>

  <div class="field">
    <label for="utterance-end">Utterance End (ms)</label>
    <input id="utterance-end" type="text" bind:value={utteranceEndMs} placeholder="1000" />
    <span class="hint">Changes to transcription settings will reconnect Deepgram</span>
  </div>

  <div class="field">
    <label for="keywords">Custom Dictionary (Keywords)</label>
    <textarea id="keywords" bind:value={keywords} placeholder="Taiga, Anthropic, Lyon" rows="3"
    ></textarea>
    <span class="hint"
      >Comma-separated terms to boost in transcription. Takes effect on next recording.</span
    >
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

  .field textarea {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.9rem;
    background: var(--panel);
    resize: vertical;
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
