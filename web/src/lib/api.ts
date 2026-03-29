import type {
  PresetMap,
  SearchResult,
  SessionDetailResponse,
  SessionSummary,
  StatusResponse,
} from './types'

async function request<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init)
  if (!response.ok) {
    const text = await response.text()
    throw new Error(text || `request failed: ${response.status}`)
  }

  if (response.status === 204) {
    return undefined as T
  }

  return (await response.json()) as T
}

export function fetchDates(): Promise<string[]> {
  return request<string[]>('/api/dates')
}

export function fetchSessions(date: string): Promise<SessionSummary[]> {
  return request<SessionSummary[]>(`/api/sessions?date=${encodeURIComponent(date)}`)
}

export function fetchSession(id: string): Promise<SessionDetailResponse> {
  return request<SessionDetailResponse>(`/api/sessions/${encodeURIComponent(id)}`)
}

export function fetchStatus(): Promise<StatusResponse> {
  return request<StatusResponse>('/api/status')
}

export function fetchPresets(): Promise<PresetMap> {
  return request<PresetMap>('/api/presets')
}

export function fetchSearch(query: string): Promise<SearchResult[]> {
  return request<SearchResult[]>(`/api/search?q=${encodeURIComponent(query)}`)
}

export function pauseRecording(): Promise<void> {
  return request<void>('/api/pause', { method: 'POST' })
}

export function resumeRecording(): Promise<void> {
  return request<void>('/api/resume', { method: 'POST' })
}

export async function resummarize(sessionId: string, preset?: string): Promise<void> {
  const response = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}/resummarize`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(preset ? { preset } : {}),
  })
  if (!response.ok) {
    throw new Error(`resummarize failed: ${response.status}`)
  }
}

export function endSession(): Promise<void> {
  return request<void>('/api/session/end', { method: 'POST' })
}

export function updateSessionTitle(sessionId: string, title: string): Promise<SessionSummary> {
  return request<SessionSummary>(`/api/sessions/${encodeURIComponent(sessionId)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title }),
  })
}

export function deleteSession(id: string): Promise<void> {
  return request<void>(`/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export function mergeSessions(sessionIds: string[]): Promise<SessionSummary> {
  return request<SessionSummary>('/api/sessions/merge', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session_ids: sessionIds }),
  })
}

export function startSession(titleHint?: string): Promise<{ session_id: string }> {
  return request<{ session_id: string }>('/api/sessions/start', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(titleHint ? { title_hint: titleHint } : {}),
  })
}

export function stopCurrentSession(): Promise<{ status: string }> {
  return request<{ status: string }>('/api/sessions/current/stop', { method: 'POST' })
}

export function stopSession(sessionId: string): Promise<{ status: string }> {
  return request<{ status: string }>(`/api/sessions/${encodeURIComponent(sessionId)}/stop`, {
    method: 'POST',
  })
}

export function retrySummary(sessionId: string): Promise<void> {
  return request<void>(`/api/sessions/${encodeURIComponent(sessionId)}/retry-summary`, {
    method: 'POST',
  })
}

export function retrySync(sessionId: string): Promise<void> {
  return request<void>(`/api/sessions/${encodeURIComponent(sessionId)}/retry-sync`, {
    method: 'POST',
  })
}

export function retryRefinement(sessionId: string): Promise<void> {
  return request<void>(`/api/sessions/${encodeURIComponent(sessionId)}/retry-refinement`, {
    method: 'POST',
  })
}

export interface LogEntry {
  timestamp: string
  level: string
  module?: string
  message: string
  raw: string
}

export function fetchLogs(level?: string, limit?: number, since?: string): Promise<LogEntry[]> {
  const params = new URLSearchParams()
  if (level && level !== 'ALL') params.set('level', level)
  if (limit) params.set('limit', limit.toString())
  if (since) params.set('since', since)
  const qs = params.toString()
  return request<LogEntry[]>(`/api/logs${qs ? '?' + qs : ''}`)
}

export interface HealthResponse {
  deepgram: string
  db: string
  mic: string
  llm: string
}

export function fetchHealth(): Promise<HealthResponse> {
  return request<HealthResponse>('/healthz/ready')
}
