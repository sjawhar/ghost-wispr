import type { PresetMap, SessionDetailResponse, SessionSummary, StatusResponse } from './types'

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
