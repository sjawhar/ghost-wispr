import { vi, type Mock } from 'vitest'
import type { ConfigResponse, ProviderStatus } from '../../lib/config-api'

// unitConfig is GET /api/config of a unit with a provider of each sign-in
// kind: Deepgram by key, the model gateway by a Google ID token, OpenAI by
// workload identity and Vertex AI by a Google access token.
export const unitConfig: ConfigResponse = {
  silence_timeout: '30s',
  min_session_segments: 0,
  summarization: { model: 'vertex/gemini-4-carbon', presets: {} },
  transcription: { endpointing: '400', utterance_end_ms: '1000' },
  gdrive: { folder_id: '', has_credentials: true, sync_enabled: true },
  gc: { enabled: true, max_age_days: 30, max_audio_size_mb: 1024 },
  tts: { model: 'vertex/gemini-3.8-flash-tts', voice: 'Kore' },
  models: {
    live_transcription: 'deepgram/nova-3',
    batch_transcription: 'deepgram/nova-3',
    summarization: 'vertex/gemini-4-carbon',
    embedding: 'gateway/gemini-embedding-2',
    concierge: 'vertex/gemini-3.5-flash-lite',
    tts: 'vertex/gemini-3.8-flash-tts',
    tts_voice: 'Kore',
    gptlive: 'openai/gpt-live-1',
    dgagent_provider: 'deepgram',
  },
  providers: {
    deepgram: { api: 'deepgram', auth: { kind: 'api_key', env: 'EXAMPLE_DEEPGRAM_KEY' } },
    gateway: {
      api: 'openai',
      url: 'https://gateway.example.test/v1',
      auth: { kind: 'google_id_token', audience: 'example-gateway' },
    },
    openai: {
      api: 'openai',
      auth: {
        kind: 'openai_wif',
        identity_provider_id: 'idp-example',
        service_account_id: 'sa-example',
      },
    },
    vertex: {
      api: 'vertex',
      project: 'example-project',
      location: 'global',
      auth: { kind: 'google' },
    },
  },
  recording_rules: [],
  voice: { enabled: true, engine: 'tts' },
}

export const checkedAt = '2026-11-20T19:04:00Z'

// providerStatuses is GET /api/providers for unitConfig: Deepgram's key is
// not set, so its check asked Deepgram nothing; the others sign in, and the
// gateway's check also read its model list. A Google token minted, or an
// OpenAI token exchanged, is the whole check of vertex and openai.
export const providerStatuses: ProviderStatus[] = [
  {
    name: 'deepgram',
    api: 'deepgram',
    url: 'https://api.deepgram.com',
    auth: 'api_key',
    sign_in: 'API key from EXAMPLE_DEEPGRAM_KEY',
    key_env: 'EXAMPLE_DEEPGRAM_KEY',
    key_set: false,
    checked: true,
    ok: false,
    asked: false,
    message: 'its key is not set',
    checked_at: checkedAt,
    roles: ['transcription.model', 'batch_transcription.model'],
  },
  {
    name: 'gateway',
    api: 'openai',
    url: 'https://gateway.example.test/v1',
    auth: 'google_id_token',
    sign_in: 'Google ID token for audience example-gateway',
    key_set: false,
    checked: true,
    ok: true,
    asked: true,
    message: 'Signed in: gateway.example.test accepted the credential.',
    checked_at: checkedAt,
    roles: ['embedding.model'],
  },
  {
    name: 'openai',
    api: 'openai',
    url: 'https://api.openai.com/v1',
    auth: 'openai_wif',
    sign_in:
      "OpenAI workload identity: an OpenAI access token in exchange for the unit's Google ID token",
    key_set: false,
    checked: true,
    ok: true,
    asked: false,
    message: 'Signed in: OpenAI exchanged the Google ID token for an access token.',
    checked_at: checkedAt,
    roles: [],
  },
  {
    name: 'vertex',
    api: 'vertex',
    url: 'https://aiplatform.googleapis.com',
    auth: 'google',
    sign_in: 'Google workload identity (google_credentials_file)',
    key_set: false,
    checked: true,
    ok: true,
    asked: false,
    message: 'Signed in: Google issued an access token.',
    checked_at: checkedAt,
    roles: ['summarization.model', 'concierge.model', 'tts.model'],
  },
]

// FetchMock is the stubbed fetch; its calls are the requests sent.
export type FetchMock = Mock<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>

// stubFetch stubs fetch as the unit answers: GET /api/providers with
// providers (a promise that never settles keeps every check running),
// GET /api/providers?refresh=1 with refreshed (providers unless given), and
// every other request with config.
export function stubFetch(
  options: {
    config?: ConfigResponse
    providers?: ProviderStatus[] | Promise<never>
    refreshed?: ProviderStatus[]
  } = {},
): FetchMock {
  const { config = unitConfig, providers = providerStatuses, refreshed = providers } = options
  const fetch: FetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = String(input)
    if (url.startsWith('/api/providers')) {
      const answer = url === '/api/providers?refresh=1' ? refreshed : providers
      if (answer instanceof Promise) return answer
      return Promise.resolve(new Response(JSON.stringify({ providers: answer })))
    }
    return Promise.resolve(new Response(JSON.stringify(config)))
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

// deferredFetch stubs fetch so that a request to the unit waits until the
// test passes the answer to the function it returns: a save stays on its way
// until then. GET /api/providers answers at once, with no provider. (The
// app's TypeScript library, ES2022, has no Promise.withResolvers.)
export function deferredFetch(): (answer: Response) => void {
  let give: (answer: Response) => void = () => {}
  const pending = new Promise<Response>((resolve) => {
    give = resolve
  })
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) =>
      String(input).startsWith('/api/providers')
        ? Promise.resolve(new Response(JSON.stringify({ providers: [] })))
        : pending.then((answer) => answer.clone()),
    ),
  )
  return give
}

// sent lists the requests a stubbed fetch was sent, each as its URL, method
// and JSON body.
export function sent(fetch: FetchMock): { url: string; method: string; body: unknown }[] {
  return fetch.mock.calls.map(([input, init]) => ({
    url: String(input),
    method: init?.method ?? 'GET',
    body: init?.body ? JSON.parse(init.body as string) : undefined,
  }))
}
