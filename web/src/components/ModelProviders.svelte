<script lang="ts">
  import { untrack } from 'svelte'
  import type {
    ConfigResponse,
    ProviderAPI,
    ProviderConfig,
    ProviderStatus,
  } from '../lib/config-api'
  import { fetchProviders, patchConfig } from '../lib/config-api'
  import { configState, setConfig } from '../lib/config.svelte'

  let {
    config,
  }: {
    config: ConfigResponse
  } = $props()

  // Each wire API by the name its vendor gives it.
  const apiNames: Record<ProviderAPI, string> = {
    openai: 'OpenAI API',
    anthropic: 'Anthropic API',
    vertex: 'Vertex AI',
    gemini: 'Gemini API',
    deepgram: 'Deepgram',
    elevenlabs: 'ElevenLabs',
    'google-tts': 'Google Cloud Text-to-Speech',
  }

  // isProviderAPI says whether api is one this build has a client for.
  function isProviderAPI(api: string): api is ProviderAPI {
    return Object.hasOwn(apiNames, api)
  }

  // apiName is api by its vendor's name, or as the config writes it when this
  // build has no client for it (the unit warned about it at start).
  function apiName(api: string): string {
    return isProviderAPI(api) ? apiNames[api] : api
  }

  // Each role, by the config key GET /api/providers names it with.
  const roleNames: Record<string, string> = {
    'transcription.model': 'Live transcription',
    'batch_transcription.model': 'Batch transcription',
    'summarization.model': 'Summaries and titles',
    'embedding.model': 'Search embeddings',
    'concierge.model': 'Concierge',
    'tts.model': 'Text-to-speech',
    'voice.gptlive.model': 'Speech-to-speech: OpenAI GPT-Live',
    'voice.dgagent.provider': 'Voice agent: Deepgram',
  }

  let statuses = $state<Record<string, ProviderStatus>>({})
  let checking = $state(false)
  let checkError = $state('')
  let checkVersion = 0
  // checkedFor is the config the page held when the last check began, as
  // JSON: the unit names each provider's roles from the config it holds, so
  // only a config that differs asks again. The same config loaded once more,
  // as the settings page loads it over the app's, does not.
  let checkedFor: string | undefined

  let keyInputs = $state<Record<string, string>>({})
  let keySaving = $state<Record<string, boolean>>({})
  // What the last key save said, by provider: the unit's refusal is an error.
  let keyFeedback = $state<Record<string, { text: string; error: boolean } | undefined>>({})

  const names = $derived(Object.keys(config.providers).sort())

  // check asks the unit how each provider signs in and whether it works;
  // refresh checks every one now rather than keep a check of the last 5
  // minutes.
  async function check(refresh: boolean): Promise<void> {
    const version = ++checkVersion
    checkedFor = JSON.stringify(configState.config)
    checking = true
    checkError = ''
    try {
      const checked = await fetchProviders(refresh)
      if (version !== checkVersion) return
      statuses = Object.fromEntries(checked.map((status) => [status.name, status]))
    } catch (error) {
      if (version === checkVersion) {
        checkError = error instanceof Error ? error.message : 'Check failed'
      }
    } finally {
      if (version === checkVersion) checking = false
    }
  }

  async function saveKey(name: string): Promise<void> {
    const key = keyInputs[name]?.trim()
    if (!key) return

    keySaving[name] = true
    keyFeedback[name] = undefined
    let saved = false
    try {
      setConfig(await patchConfig({ provider_keys: { [name]: key } }))
      keyInputs[name] = ''
      keyFeedback[name] = { text: 'Saved!', error: false }
      saved = true
    } catch (error) {
      keyFeedback[name] = {
        text: error instanceof Error ? error.message : 'Save failed',
        error: true,
      }
    } finally {
      keySaving[name] = false
    }
    // The provider signs in with the new key from now on: check it now.
    if (saved) await check(true)
  }

  // Health is what the Status line says about a provider, and how it shows:
  // why, when the state is Signed in, is the check's message under it, which
  // a phone, with no hover, shows as it shows a failure's.
  type Health = {
    state: 'checking' | 'unchecked' | 'ok' | 'signed-in' | 'fail'
    text: string
    why?: string
  }

  // works says whether a provider works, as its last check found: only when
  // its answers proved every model its roles call (asked). One whose check
  // only minted or exchanged a token, or whose roles call a model no cheap
  // call proves (a speech model), only signed in, and says why. Works says
  // nothing more.
  function works(status: ProviderStatus | undefined): Health {
    if (checking || (!status && !checkError)) return { state: 'checking', text: 'Checking…' }
    if (!status?.checked) return { state: 'unchecked', text: 'Not checked' }
    if (!status.ok) return { state: 'fail', text: `Fails: ${status.message}` }
    if (status.asked) return { state: 'ok', text: 'Works' }
    return {
      state: 'signed-in',
      text: 'Signed in',
      why: status.message.replace(/^Signed in: /, ''),
    }
  }

  // where tells providers of one API apart: a Vertex AI provider by its
  // project and location, any other by the base URL it calls (the config's
  // url, else its API's own, which the check reports).
  function where(provider: ProviderConfig, status: ProviderStatus | undefined): string {
    if (provider.api === 'vertex') {
      const place = `Project ${provider.project ?? ''}, location ${provider.location || 'global'}`
      return provider.url ? `${place}, ${provider.url}` : place
    }
    return provider.url || status?.url || ''
  }

  // Check at mount, and again, from the unit's cache, whenever the config the
  // page holds changes (another section saved), so "Used by" follows it. A
  // check that began for that config already, as a key save's, is enough.
  $effect(() => {
    const held = JSON.stringify(configState.config)
    untrack(() => {
      if (held !== checkedFor) void check(false)
    })
  })
</script>

<div class="providers" data-testid="model-providers">
  <div class="providers-head">
    <h4>Model providers</h4>
    <button class="check-btn" type="button" onclick={() => check(true)} disabled={checking}>
      Check again
    </button>
  </div>
  {#if checkError}
    <p class="feedback error">Could not check the providers: {checkError}</p>
  {/if}

  <ul class="provider-list" aria-label="Model providers">
    {#each names as name (name)}
      {@const provider = config.providers[name]}
      {@const status = statuses[name]}
      {@const health = works(status)}
      {@const feedback = keyFeedback[name]}
      <li class="provider-row" aria-label={name}>
        <div class="provider-title">
          <span class="provider-name">{name}</span>
          <span class="provider-api">{apiName(provider.api)}</span>
        </div>
        {#if where(provider, status)}
          <div class="provider-where">{where(provider, status)}</div>
        {/if}
        <dl class="provider-facts">
          <dt>Signs in</dt>
          <dd>{status?.sign_in ?? '…'}</dd>
          <dt>Status</dt>
          <dd>
            <span
              class="works"
              class:ok={health.state === 'ok'}
              class:signed-in={health.state === 'signed-in'}
              class:fail={health.state === 'fail'}
              title={status?.ok ? status.message : undefined}>{health.text}</span
            >
            {#if status?.checked && !checking}
              <span class="checked-at"
                >checked at {new Date(status.checked_at).toLocaleTimeString([], {
                  hour: '2-digit',
                  minute: '2-digit',
                })}</span
              >
            {/if}
            {#if health.why}
              <span class="works-why">{health.why}</span>
            {/if}
          </dd>
          <dt>Used by</dt>
          <dd>
            {#if !status}
              …
            {:else if status.roles.length === 0}
              Nothing uses it
            {:else}
              {status.roles.map((role) => roleNames[role] ?? role).join(', ')}
            {/if}
          </dd>
        </dl>
        {#if provider.auth.kind === 'api_key'}
          {#if provider.auth.read_only}
            <p class="key-read-only">
              This unit reads its key from its deployment. It cannot be changed here.
            </p>
          {:else}
            <div class="key-row">
              <input
                type="password"
                autocomplete="off"
                aria-label="Key for {name}"
                placeholder={status ? (status.key_set ? '••••••••' : 'Not set') : ''}
                bind:value={keyInputs[name]}
              />
              <button
                class="save-btn"
                type="button"
                onclick={() => saveKey(name)}
                disabled={keySaving[name] || !keyInputs[name]?.trim()}
              >
                {keySaving[name] ? '...' : 'Save key'}
              </button>
              {#if feedback}
                <span class="feedback" class:error={feedback.error}>{feedback.text}</span>
              {/if}
            </div>
          {/if}
        {/if}
      </li>
    {/each}
  </ul>
</div>

<style>
  .providers {
    margin-bottom: 1.5rem;
  }

  .providers-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    margin-bottom: 0.75rem;
  }

  .providers-head h4 {
    margin: 0;
    font-size: 1rem;
  }

  .check-btn {
    background: none;
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 0.25rem 0.65rem;
    cursor: pointer;
    font-family: inherit;
    color: var(--ink);
  }

  .check-btn:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  .provider-list {
    display: flex;
    flex-direction: column;
    gap: 0.65rem;
    list-style: none;
    margin: 0;
    padding: 0;
  }

  .provider-row {
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 0.55rem 0.65rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }

  .provider-title {
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
    flex-wrap: wrap;
  }

  .provider-name {
    font-weight: 600;
    font-size: 0.9rem;
  }

  .provider-api {
    font-size: 0.85rem;
  }

  .provider-where {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--muted);
    overflow-wrap: anywhere;
  }

  .provider-facts {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.2rem 0.75rem;
    margin: 0;
    font-size: 0.82rem;
  }

  .provider-facts dt {
    color: var(--muted);
  }

  .provider-facts dd {
    margin: 0;
    overflow-wrap: anywhere;
  }

  .works.ok {
    color: var(--accent);
    font-weight: 600;
  }

  /* Signed in is less than Works: the check proved the sign-in, not every model the provider's roles call. */
  .works.signed-in {
    color: var(--accent);
  }

  /* Why a provider is only Signed in, on a line of its own under the state. */
  .works-why {
    display: block;
    color: var(--muted);
  }

  .works.fail {
    color: var(--danger);
  }

  .checked-at {
    color: var(--muted);
    margin-left: 0.35rem;
  }

  .key-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }

  .key-read-only {
    margin: 0;
    color: var(--muted);
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
