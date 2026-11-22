<script lang="ts">
  import type { ConfigPatch, ConfigResponse, VoiceEngine } from '../lib/config-api'
  import { patchConfig } from '../lib/config-api'
  import { setConfig } from '../lib/config.svelte'
  import { unitField } from '../lib/unit-field.svelte'
  import ReplacedNote from './ReplacedNote.svelte'

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

  // After a save the fields show what the unit answered, as the settings page
  // passes it in (a new model's voice is its default).
  const enabled = unitField(() => config.voice.enabled)
  const engine = unitField(() => config.voice.engine)
  const ttsModel = unitField(() => config.tts.model)
  const ttsVoice = unitField(() => config.tts.voice)
  let saving = $state(false)
  let feedback = $state('')

  async function save(): Promise<void> {
    // Each field the save carries, acknowledged with the value sent once the
    // unit takes it.
    const carried: (() => void)[] = []
    const voice: NonNullable<ConfigPatch['voice']> = {}
    if (enabled.edited) {
      const sent = enabled.value
      voice.enabled = sent
      carried.push(() => enabled.acknowledge(sent))
    }
    if (engine.edited) {
      const sent = engine.value
      voice.engine = sent
      carried.push(() => engine.acknowledge(sent))
    }
    const tts: NonNullable<ConfigPatch['tts']> = {}
    if (ttsModel.edited) {
      const sent = ttsModel.value.trim()
      tts.model = sent
      carried.push(() => ttsModel.acknowledge(sent))
    }
    if (ttsVoice.edited) {
      const sent = ttsVoice.value.trim()
      tts.voice = sent
      carried.push(() => ttsVoice.acknowledge(sent))
    }
    const patch: ConfigPatch = {}
    if (Object.keys(voice).length > 0) {
      patch.voice = voice
    }
    if (Object.keys(tts).length > 0) {
      patch.tts = tts
    }
    if (carried.length === 0) {
      feedback = 'No changes to save.'
      return
    }

    saving = true
    feedback = ''
    try {
      const updated = await patchConfig(patch)
      for (const acknowledge of carried) acknowledge()
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
    <input type="checkbox" bind:checked={enabled.value} disabled={saving} />
    Enable meeting voice
  </label>
  <ReplacedNote shown={enabled.replaced} />

  <fieldset class="choices">
    <legend>Voice</legend>
    {#each engines as option (option.value)}
      <label class="choice">
        <input
          type="radio"
          name="voice-engine"
          value={option.value}
          bind:group={engine.value}
          disabled={saving}
        />
        {option.label}
      </label>
    {/each}
    <ReplacedNote shown={engine.replaced} />
  </fieldset>

  {#if engine.value === 'tts'}
    <div class="field">
      <label for="tts-model">Text-to-speech model</label>
      <input
        id="tts-model"
        type="text"
        bind:value={ttsModel.value}
        disabled={saving}
        placeholder="provider/model, such as vertex/gemini-3.8-flash-tts"
      />
      <p class="hint">
        As provider/model: the provider is one of the Model providers under Integrations. Empty
        turns text-to-speech off.
      </p>
      <ReplacedNote shown={ttsModel.replaced} />
    </div>
    <div class="field">
      <label for="tts-voice">Text-to-speech voice</label>
      <input
        id="tts-voice"
        type="text"
        bind:value={ttsVoice.value}
        disabled={saving}
        placeholder="The model's default voice"
      />
      <ReplacedNote shown={ttsVoice.replaced} />
    </div>
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

  .field {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .field label {
    font-weight: 600;
    font-size: 0.85rem;
  }

  .field input {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    background: var(--panel);
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
