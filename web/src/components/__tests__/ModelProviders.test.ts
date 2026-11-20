import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import ModelProviders from '../ModelProviders.svelte'
import SettingsPage from '../SettingsPage.svelte'
import { configState, resetConfigState, setConfig } from '../../lib/config.svelte'
import type { ConfigResponse, ProviderStatus } from '../../lib/config-api'
import { checkedAt, providerStatuses, sent, stubFetch, unitConfig } from './configFixture'

// recheckedAt is the time of a check after the one at checkedAt.
const recheckedAt = '2026-11-20T19:41:00Z'

// row is the row of provider name, once the providers are checked.
async function row(name: string): Promise<HTMLElement> {
  const found = await screen.findByRole('listitem', { name })
  await within(found).findByText(/^(Works|Signed in|Fails: )/)
  return found
}

// withStatus is providerStatuses with the status of name changed by change.
function withStatus(name: string, change: Partial<ProviderStatus>): ProviderStatus[] {
  return providerStatuses.map((status) =>
    status.name === name ? { ...status, ...change } : status,
  )
}

// shownTime is a check's time as the row shows it.
function shownTime(at: string): string {
  return new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

describe('ModelProviders', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('shows how a provider signed in by workload identity signs in, with no key field', async () => {
    stubFetch()
    render(ModelProviders, { config: unitConfig })

    for (const [name, signIn] of [
      ['vertex', 'Google workload identity (google_credentials_file)'],
      [
        'openai',
        "OpenAI workload identity: an OpenAI access token in exchange for the unit's Google ID token",
      ],
      ['gateway', 'Google ID token for audience example-gateway'],
    ]) {
      const r = await row(name)
      expect(within(r).getByText(signIn)).toBeTruthy()
      expect(within(r).queryByText(/Not set/)).toBeNull()
      expect(within(r).queryByLabelText(/^Key for/)).toBeNull()
    }
  })

  it('shows a key field only for a provider that signs in with a key, and why it fails', async () => {
    stubFetch()
    render(ModelProviders, { config: unitConfig })

    const r = await row('deepgram')
    expect(within(r).getByText('API key from EXAMPLE_DEEPGRAM_KEY')).toBeTruthy()
    expect(within(r).getByText('Fails: its key is not set')).toBeTruthy()
    const key = within(r).getByLabelText('Key for deepgram') as HTMLInputElement
    expect(key.type).toBe('password')
    expect(key.placeholder).toBe('Not set')
    expect(screen.getAllByLabelText(/^Key for/)).toHaveLength(1)
  })

  it('says Works for a provider that answered the read its check made', async () => {
    stubFetch()
    render(ModelProviders, { config: unitConfig })

    const r = await row('gateway')
    expect(within(r).getByText('Works')).toBeTruthy()
    expect(within(r).queryByText('Signed in')).toBeNull()
  })

  it('says Signed in, not Works, for a provider whose check only signed in', async () => {
    stubFetch()
    render(ModelProviders, { config: unitConfig })

    for (const [name, how] of [
      ['vertex', 'Signed in: Google issued an access token.'],
      ['openai', 'Signed in: OpenAI exchanged the Google ID token for an access token.'],
    ]) {
      const r = await row(name)
      expect(within(r).getByText('Signed in')).toBeTruthy()
      expect(within(r).queryByText('Works')).toBeNull()
      expect(within(r).getByTitle(how)).toBeTruthy()
    }
  })

  // A phone has no hover: the reason a provider is only Signed in, not Works,
  // must be on the page, not only in the status's tooltip (re-proof of
  // 4e85fcf). Works stays one word.
  it('shows why a provider is only Signed in under its state, and keeps Works terse', async () => {
    const why = 'api.deepgram.com accepted the credential. The check makes no call of nova-3.'
    stubFetch({
      providers: withStatus('deepgram', {
        key_set: true,
        ok: true,
        asked: false,
        message: `Signed in: ${why}`,
      }),
    })
    render(ModelProviders, { config: unitConfig })

    const signedIn = await row('deepgram')
    const reason = within(signedIn).getByText(why)
    expect(reason.closest('dd')).toBe(within(signedIn).getByText('Signed in').closest('dd'))
    for (const [name, how] of [
      ['vertex', 'Google issued an access token.'],
      ['openai', 'OpenAI exchanged the Google ID token for an access token.'],
    ]) {
      expect(within(await row(name)).getByText(how)).toBeTruthy()
    }

    const works = await row('gateway')
    expect(within(works).getByText('Works')).toBeTruthy()
    expect(within(works).queryByText(/accepted the credential/)).toBeNull()
  })

  it('says Fails and why for a provider that answered the read its check made with a refusal', async () => {
    stubFetch({
      providers: withStatus('deepgram', {
        key_set: true,
        asked: true,
        message: 'api.deepgram.com refused the credential (HTTP 401).',
      }),
    })
    render(ModelProviders, { config: unitConfig })

    const r = await row('deepgram')
    expect(
      within(r).getByText('Fails: api.deepgram.com refused the credential (HTTP 401).'),
    ).toBeTruthy()
    expect(within(r).queryByText('Works')).toBeNull()
    expect(within(r).queryByText('Signed in')).toBeNull()
  })

  it('names each API and what uses each provider in standard words', async () => {
    stubFetch()
    render(ModelProviders, { config: unitConfig })

    const deepgram = await row('deepgram')
    expect(within(deepgram).getByText('Deepgram')).toBeTruthy()
    expect(within(deepgram).getByText('Live transcription, Batch transcription')).toBeTruthy()
    const gateway = await row('gateway')
    expect(within(gateway).getByText('OpenAI API')).toBeTruthy()
    expect(within(gateway).getByText('https://gateway.example.test/v1')).toBeTruthy()
    expect(within(gateway).getByText('Search embeddings')).toBeTruthy()
    const vertex = await row('vertex')
    expect(within(vertex).getByText('Vertex AI')).toBeTruthy()
    expect(within(vertex).getByText('Summaries and titles, Concierge, Text-to-speech')).toBeTruthy()
    const openai = await row('openai')
    expect(within(openai).getByText('OpenAI API')).toBeTruthy()
    expect(within(openai).getByText('Nothing uses it')).toBeTruthy()
  })

  it('says when each provider was checked', async () => {
    stubFetch()
    render(ModelProviders, { config: unitConfig })

    expect(within(await row('vertex')).getByText(`checked at ${shownTime(checkedAt)}`)).toBeTruthy()
  })

  it('shows Checking… on each provider while the providers are checked', async () => {
    stubFetch({ providers: new Promise<never>(() => {}) })
    render(ModelProviders, { config: unitConfig })

    for (const name of ['deepgram', 'gateway', 'openai', 'vertex']) {
      const r = await screen.findByRole('listitem', { name })
      expect(within(r).getByText('Checking…')).toBeTruthy()
    }
  })

  it('saves a key with provider_keys, then checks the providers again now', async () => {
    const fetch = stubFetch()
    render(ModelProviders, { config: unitConfig })

    const r = await row('deepgram')
    const key = within(r).getByLabelText('Key for deepgram') as HTMLInputElement
    await fireEvent.input(key, { target: { value: ' dg-2 ' } })
    await fireEvent.click(within(r).getByRole('button', { name: 'Save key' }))

    await waitFor(() =>
      expect(sent(fetch)).toEqual([
        { url: '/api/providers', method: 'GET', body: undefined },
        { url: '/api/config', method: 'PATCH', body: { provider_keys: { deepgram: 'dg-2' } } },
        { url: '/api/providers?refresh=1', method: 'GET', body: undefined },
      ]),
    )
    await waitFor(() => expect(configState.config).toEqual(unitConfig))
    expect(key.value).toBe('')
  })

  it('shows the check a key save asks for in place of the one before it', async () => {
    // Deepgram's check reads its projects, which prove the key and not the
    // model its roles call: a working key reads Signed in.
    const message =
      'Signed in: api.deepgram.com accepted the credential. The check makes no call of nova-3.'
    stubFetch({
      refreshed: withStatus('deepgram', {
        key_set: true,
        ok: true,
        asked: false,
        message,
        checked_at: recheckedAt,
      }),
    })
    render(ModelProviders, { config: unitConfig })
    const r = await row('deepgram')
    expect(within(r).getByText('Fails: its key is not set')).toBeTruthy()
    expect(within(r).getByText(`checked at ${shownTime(checkedAt)}`)).toBeTruthy()

    await fireEvent.input(within(r).getByLabelText('Key for deepgram'), {
      target: { value: 'dg-2' },
    })
    await fireEvent.click(within(r).getByRole('button', { name: 'Save key' }))

    expect(await within(r).findByText('Signed in')).toBeTruthy()
    expect(within(r).getByTitle(message)).toBeTruthy()
    expect(within(r).queryByText('Works')).toBeNull()
    expect(within(r).getByText(`checked at ${shownTime(recheckedAt)}`)).toBeTruthy()
    expect(within(r).queryByText(/^Fails: /)).toBeNull()
    expect(within(r).queryByText(`checked at ${shownTime(checkedAt)}`)).toBeNull()
    expect((within(r).getByLabelText('Key for deepgram') as HTMLInputElement).placeholder).toBe(
      '••••••••',
    )
  })

  it('keeps the new key check when an older check finishes later', async () => {
    // The app targets ES2022, which has no Promise.withResolvers.
    let finishOld!: (response: Response) => void
    const oldCheck = new Promise<Response>((resolve) => {
      finishOld = resolve
    })
    const fetch = vi.fn((input: RequestInfo | URL) => {
      if (String(input) === '/api/providers') return oldCheck
      if (String(input) === '/api/config') {
        return Promise.resolve(new Response(JSON.stringify(unitConfig)))
      }
      const fresh = providerStatuses.map((status) =>
        status.name === 'deepgram'
          ? { ...status, ok: true, asked: false, key_set: true, message: 'New key accepted.' }
          : status,
      )
      return Promise.resolve(new Response(JSON.stringify({ providers: fresh })))
    })
    vi.stubGlobal('fetch', fetch)
    render(ModelProviders, { config: unitConfig })
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))

    const r = screen.getByRole('listitem', { name: 'deepgram' })
    await fireEvent.input(within(r).getByLabelText('Key for deepgram'), {
      target: { value: 'dg-2' },
    })
    await fireEvent.click(within(r).getByRole('button', { name: 'Save key' }))
    await within(r).findByText('Signed in')

    const oldResponse = new Response(JSON.stringify({ providers: providerStatuses }))
    const decoded = vi.spyOn(oldResponse, 'json')
    finishOld(oldResponse)
    await waitFor(() => expect(decoded).toHaveResolved())
    expect(within(r).getByText('Signed in')).toBeTruthy()
    expect((within(r).getByLabelText('Key for deepgram') as HTMLInputElement).placeholder).toBe(
      '••••••••',
    )
  })

  it("shows the unit's refusal of a key", async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) =>
        Promise.resolve(
          String(input).startsWith('/api/providers')
            ? new Response(JSON.stringify({ providers: providerStatuses }))
            : new Response(JSON.stringify({ error: 'provider_keys: no provider "deepgram"' }), {
                status: 400,
              }),
        ),
      ),
    )
    render(ModelProviders, { config: unitConfig })

    const r = await row('deepgram')
    await fireEvent.input(within(r).getByLabelText('Key for deepgram'), {
      target: { value: 'dg-2' },
    })
    await fireEvent.click(within(r).getByRole('button', { name: 'Save key' }))

    const shown = await within(r).findByText('provider_keys: no provider "deepgram"')
    expect(shown.classList.contains('error')).toBe(true)
  })

  it('checks every provider now on Check again', async () => {
    const fetch = stubFetch()
    render(ModelProviders, { config: unitConfig })
    await row('vertex')

    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }))

    await waitFor(() =>
      expect(sent(fetch).map((r) => r.url)).toEqual(['/api/providers', '/api/providers?refresh=1']),
    )
  })

  it('shows the check Check again asks for in place of the one before it', async () => {
    stubFetch({
      providers: withStatus('gateway', {
        ok: false,
        message: 'Signed in, but gateway.example.test answered HTTP 503.',
      }),
      refreshed: withStatus('gateway', { checked_at: recheckedAt }),
    })
    render(ModelProviders, { config: unitConfig })
    const r = await row('gateway')
    expect(
      within(r).getByText('Fails: Signed in, but gateway.example.test answered HTTP 503.'),
    ).toBeTruthy()
    expect(within(r).getByText(`checked at ${shownTime(checkedAt)}`)).toBeTruthy()

    await fireEvent.click(screen.getByRole('button', { name: 'Check again' }))

    expect(await within(r).findByText('Works')).toBeTruthy()
    expect(within(r).getByText(`checked at ${shownTime(recheckedAt)}`)).toBeTruthy()
    expect(within(r).queryByText(/^Fails: /)).toBeNull()
    expect(within(r).queryByText(`checked at ${shownTime(checkedAt)}`)).toBeNull()
  })

  it('says why the providers could not be checked', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ error: 'provider checks are not configured' }), {
            status: 501,
          }),
        ),
      ),
    )
    render(ModelProviders, { config: unitConfig })

    expect(
      await screen.findByText('Could not check the providers: provider checks are not configured'),
    ).toBeTruthy()
    expect(
      within(screen.getByRole('listitem', { name: 'vertex' })).getByText('Not checked'),
    ).toBeTruthy()
  })

  it('asks again what uses each provider when the config the page holds changes', async () => {
    // The unit names the roles from the config it holds now: after another
    // section's save, openai serves the voice that is now selected.
    const gptlive: ConfigResponse = { ...unitConfig, voice: { enabled: true, engine: 'gptlive' } }
    let checks = 0
    const fetch = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(() => {
      checks++
      const statuses =
        checks === 1 ? providerStatuses : withStatus('openai', { roles: ['voice.gptlive.model'] })
      return Promise.resolve(new Response(JSON.stringify({ providers: statuses })))
    })
    vi.stubGlobal('fetch', fetch)
    render(ModelProviders, { config: unitConfig })
    expect(within(await row('openai')).getByText('Nothing uses it')).toBeTruthy()

    setConfig(gptlive)

    expect(
      await within(screen.getByRole('listitem', { name: 'openai' })).findByText(
        'Speech-to-speech: OpenAI GPT-Live',
      ),
    ).toBeTruthy()
    expect(fetch.mock.calls.map(([input]) => String(input))).toEqual([
      '/api/providers',
      '/api/providers',
    ])
  })

  it('asks for the providers once when the settings page loads, and once, refreshed, for a key save', async () => {
    // The app loads the config at start and the settings page loads it again:
    // the same config in a new object, which changes no provider's roles.
    const fetch = stubFetch()
    setConfig(unitConfig)
    render(SettingsPage, { onBack: () => {} })
    const r = await row('deepgram')
    await waitFor(() => expect(configState.config).not.toBe(unitConfig))
    // A check the reload starts asks fetch at once, in the effect tick runs.
    await tick()
    const providerRequests = () =>
      sent(fetch)
        .map((request) => request.url)
        .filter((url) => url.startsWith('/api/providers'))
    expect(providerRequests()).toEqual(['/api/providers'])

    await fireEvent.input(within(r).getByLabelText('Key for deepgram'), {
      target: { value: 'dg-2' },
    })
    await fireEvent.click(within(r).getByRole('button', { name: 'Save key' }))
    await within(r).findByText('Saved!')
    await waitFor(() =>
      expect(providerRequests()).toEqual(['/api/providers', '/api/providers?refresh=1']),
    )
    await tick()
    expect(providerRequests()).toEqual(['/api/providers', '/api/providers?refresh=1'])
  })

  it('tells providers of one API apart by project and location, or by the URL they call', async () => {
    const vertex = providerStatuses.find((status) => status.name === 'vertex')!
    const config: ConfigResponse = {
      ...unitConfig,
      providers: {
        ...unitConfig.providers,
        'vertex-us': {
          api: 'vertex',
          project: 'example-project',
          location: 'us',
          auth: { kind: 'google' },
        },
        'vertex-other': { api: 'vertex', project: 'other-project', auth: { kind: 'google' } },
      },
    }
    stubFetch({
      config,
      providers: [
        ...providerStatuses,
        { ...vertex, name: 'vertex-us', url: 'https://aiplatform.us.example.test', roles: [] },
        { ...vertex, name: 'vertex-other', roles: [] },
      ],
    })
    render(ModelProviders, { config })

    for (const [name, where] of [
      ['vertex', 'Project example-project, location global'],
      ['vertex-us', 'Project example-project, location us'],
      ['vertex-other', 'Project other-project, location global'],
      // No url in the config: the base URL the unit calls.
      ['deepgram', 'https://api.deepgram.com'],
      ['openai', 'https://api.openai.com/v1'],
      ['gateway', 'https://gateway.example.test/v1'],
    ]) {
      expect(within(await row(name)).getByText(where)).toBeTruthy()
    }
  })
})
