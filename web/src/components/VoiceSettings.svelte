<script lang="ts">
  import type { ConfigResponse, TTSEngine, VoiceEngine } from '../lib/config-api'
  import { patchConfig } from '../lib/config-api'
  import { setConfig } from '../lib/config.svelte'

  let {
    config,
  }: {
    config: ConfigResponse
  } = $props()

  const engines: { value: VoiceEngine; label: string }[] = [
    { value: 'tts', label: 'Text-to-speech (tts)' },
    { value: 'gptlive', label: 'Speech-to-speech: OpenAI GPT-Live (gptlive)' },
    { value: 'dgagent', label: 'Voice agent: Deepgram (dgagent)' },
  ]
  const ttsEngines: { value: TTSEngine; label: string }[] = [
    { value: 'gemini', label: 'Gemini' },
    { value: 'aura', label: 'Aura' },
  ]

  let enabled = $state(config.voice.enabled)
  let engine = $state(config.voice.engine)
  let ttsEngine = $state(config.voice.tts_engine)
  let saving = $state(false)
  let feedback = $state('')

  async function save(): Promise<void> {
    const voice: Partial<ConfigResponse['voice']> = {}
    if (enabled !== config.voice.enabled) {
      voice.enabled = enabled
    }
    if (engine !== config.voice.engine) {
      voice.engine = engine
    }
    if (ttsEngine !== config.voice.tts_engine) {
      voice.tts_engine = ttsEngine
    }
    if (Object.keys(voice).length === 0) {
      feedback = 'No changes to save.'
      return
    }

    saving = true
    feedback = ''
    try {
      const updated = await patchConfig({ voice })
      setConfig(updated)
      feedback = 'Saved!'
    } catch (error) {
      feedback = error instanceof Error ? error.message : 'Save failed'
    } finally {
      saving = false
    }
  }
</script>

<div class="voice-settings" data-testid="voice-settings">
  <label class="toggle-label">
    <input type="checkbox" bind:checked={enabled} />
    Enable meeting voice
  </label>

  <fieldset class="choices">
    <legend>Voice</legend>
    {#each engines as option (option.value)}
      <label class="choice">
        <input type="radio" name="voice-engine" value={option.value} bind:group={engine} />
        {option.label}
      </label>
    {/each}
  </fieldset>

  {#if engine === 'tts'}
    <fieldset class="choices">
      <legend>Text-to-speech provider</legend>
      {#each ttsEngines as option (option.value)}
        <label class="choice">
          <input type="radio" name="voice-tts-engine" value={option.value} bind:group={ttsEngine} />
          {option.label}
        </label>
      {/each}
    </fieldset>
  {/if}

  <p class="hint">Changes apply from the next call; no restart.</p>

  <div class="form-actions">
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
  .voice-settings {
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

  .choices {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
    border: none;
    margin: 0;
    padding: 0;
  }

  .choices legend {
    font-weight: 600;
    font-size: 0.85rem;
    margin-bottom: 0.25rem;
    padding: 0;
  }

  .choice {
    display: flex;
    align-items: flex-start;
    gap: 0.5rem;
    font-size: 0.85rem;
    cursor: pointer;
  }

  /* Centres the radio, 13px tall, on the label's first line, so a label that
     wraps keeps its radio beside its first words. */
  .choice input {
    margin-block: calc((1lh - 13px) / 2) 0;
  }

  .hint {
    font-size: 0.8rem;
    color: var(--muted);
    margin: 0;
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
