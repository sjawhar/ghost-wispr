export interface PresetDetail {
  description: string
  system_prompt: string
  user_template: string
  model: string
}

export interface ConfigResponse {
  silence_timeout: string
  summarization: {
    model: string
    base_url: string
    presets: Record<string, PresetDetail>
  }
  transcription: {
    endpointing: string
    utterance_end_ms: string
  }
  gdrive: {
    folder_id: string
    has_credentials: boolean
  }
  api_keys: Record<string, boolean>
}

export interface TestPresetResponse {
  summary: string
  error: string
}

async function configRequest<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init)
  if (!response.ok) {
    const text = await response.text()
    throw new Error(text || `request failed: ${response.status}`)
  }
  return (await response.json()) as T
}

export function fetchConfig(): Promise<ConfigResponse> {
  return configRequest<ConfigResponse>('/api/config')
}

export function patchConfig(patch: Record<string, unknown>): Promise<ConfigResponse> {
  return configRequest<ConfigResponse>('/api/config', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
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
