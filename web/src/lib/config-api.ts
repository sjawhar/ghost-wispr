export interface PresetDetail {
  description: string
  system_prompt: string
  user_template: string
  model: string
}

// The voice that answers in a meeting: voice A (text-to-speech), voice B
// (speech-to-speech: OpenAI GPT-Live) or voice C (voice agent: Deepgram).
export type VoiceEngine = 'tts' | 'gptlive' | 'dgagent'

// The wire APIs this build has a client for (providers.<name>.api). A unit's
// config can name another, which the unit warns about at start and still
// reports, so a provider's api on the wire is any string.
export type ProviderAPI =
  | 'openai'
  | 'anthropic'
  | 'vertex'
  | 'gemini'
  | 'deepgram'
  | 'elevenlabs'
  | 'google-tts'

// A model provider as the unit's config names it: an endpoint, the wire API
// it speaks and how the unit signs in. It names no secret.
export interface ProviderConfig {
  // One of ProviderAPI in a config the unit takes without a warning.
  api: string
  url?: string
  project?: string
  location?: string
  auth: {
    // api_key, google, google_id_token or openai_wif in a config the unit
    // takes without a warning.
    kind: string
    // api_key: the variable that holds the key.
    env?: string
    audience?: string
    identity_provider_id?: string
    service_account_id?: string
  }
}

// A provider of GET /api/providers: how it signs in, whether that worked
// when last checked, and the roles in use that call it, by config key.
export interface ProviderStatus {
  name: string
  api: string // as ProviderConfig's
  url: string
  auth: string // the sign-in kind, as ProviderConfig's auth.kind
  sign_in: string
  key_env?: string
  key_set: boolean
  checked: boolean
  ok: boolean
  // asked says the provider's answers proved every model its roles call, or
  // refused one. It is false when a minted Google token or an exchanged
  // OpenAI one is the check's only proof, or when a role's model has no cheap
  // call (a speech model), even after the provider answered other reads: ok
  // then means it signed in.
  asked: boolean
  message: string
  checked_at: string
  roles: string[]
}

export interface ConfigResponse {
  silence_timeout: string
  min_session_segments: number
  summarization: {
    model: string
    presets: Record<string, PresetDetail>
  }
  transcription: {
    endpointing: string
    utterance_end_ms: string
  }
  gdrive: {
    folder_id: string
    has_credentials: boolean
    sync_enabled: boolean
  }
  gc: {
    enabled: boolean
    max_age_days: number
    max_audio_size_mb: number
  }
  // Text-to-speech: the provider/model voice A and agents' speech speak
  // with, and its voice ("" is the model's default).
  tts: {
    model: string
    voice: string
  }
  // What each role calls as the unit runs, environment overrides included.
  // Read-only.
  models: Record<string, string>
  // The model providers the unit's config names, by name. Read-only.
  providers: Record<string, ProviderConfig>
  // The rules the page shows under Recording rules, each as written in the
  // unit's recording_rules; empty when it has none. Read-only.
  recording_rules: string[]
  // The meeting voice: whether it is on, and which voice answers. A change
  // applies from the next exchange, with no restart.
  voice: {
    enabled: boolean
    engine: VoiceEngine
  }
}

// The body of PATCH /api/config: each field it changes, and nothing else.
// The unit refuses a body with a field it does not know, and saves nothing.
export interface ConfigPatch {
  silence_timeout?: string
  min_session_segments?: number
  summarization?: {
    model?: string
    // A preset's fields to change; null deletes the preset. A model or
    // description of '' clears it: a preset with no model uses
    // summarization.model.
    presets?: Record<string, Partial<PresetDetail> | null>
  }
  transcription?: {
    endpointing?: string
    utterance_end_ms?: string
  }
  // The key of each named provider that signs in with one.
  provider_keys?: Record<string, string>
  tts?: Partial<ConfigResponse['tts']>
  gdrive?: {
    folder_id?: string
    credentials_base64?: string
    sync_enabled?: boolean
  }
  gc?: Partial<ConfigResponse['gc']>
  voice?: Partial<ConfigResponse['voice']>
}

export interface TestPresetResponse {
  summary: string
  error: string
}

// ConfigRequestError is a refusal from a settings route: the unit's own words
// and the status it answered with.
export class ConfigRequestError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

// configRequest fetches a settings route; a refusal throws the unit's own
// words, the error of its {"error": ...} answer.
async function configRequest<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init)
  if (!response.ok) {
    const text = await response.text()
    let message = text
    try {
      const refusal: unknown = JSON.parse(text)
      if (
        refusal &&
        typeof refusal === 'object' &&
        'error' in refusal &&
        typeof refusal.error === 'string'
      ) {
        message = refusal.error
      }
    } catch {
      // Not JSON: the text is the refusal.
    }
    throw new ConfigRequestError(message || `request failed: ${response.status}`, response.status)
  }
  return (await response.json()) as T
}

// Each read and save of the config is numbered as it is sent. Two can be on
// their way at once, and the answer to the earlier one can land later; its
// config is then the older, and setConfig keeps the newer.
let configRequests = 0
const sentAs = new WeakMap<ConfigResponse, number>()

// sentNumber is the number of the request a config answered, if it answered
// one.
export function sentNumber(config: ConfigResponse): number | undefined {
  return sentAs.get(config)
}

async function numbered(request: () => Promise<ConfigResponse>): Promise<ConfigResponse> {
  const number = ++configRequests
  const config = await request()
  sentAs.set(config, number)
  return config
}

export function fetchConfig(): Promise<ConfigResponse> {
  return numbered(() => configRequest<ConfigResponse>('/api/config'))
}

// fetchProviders returns each configured provider's sign-in and last check;
// refresh checks every one now instead of keeping a recent check.
export async function fetchProviders(refresh = false): Promise<ProviderStatus[]> {
  const answer = await configRequest<{ providers: ProviderStatus[] }>(
    refresh ? '/api/providers?refresh=1' : '/api/providers',
  )
  return answer.providers
}

export function patchConfig(patch: ConfigPatch): Promise<ConfigResponse> {
  return numbered(() =>
    configRequest<ConfigResponse>('/api/config', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch),
    }),
  )
}

export function testPreset(name: string, sessionId: string): Promise<TestPresetResponse> {
  return configRequest<TestPresetResponse>(`/api/config/presets/${encodeURIComponent(name)}/test`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session_id: sessionId }),
  })
}

export interface GeneratePresetResponse {
  description: string
  system_prompt: string
  user_template: string
  model: string
}

export function generatePreset(description: string): Promise<GeneratePresetResponse> {
  return configRequest<GeneratePresetResponse>('/api/config/presets/generate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ description }),
  })
}

export function refinePreset(name: string, feedback: string): Promise<GeneratePresetResponse> {
  return configRequest<GeneratePresetResponse>('/api/config/presets/refine', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, feedback }),
  })
}
