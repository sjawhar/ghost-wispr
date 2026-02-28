import type {
  LiveTranscriptEvent,
  PresetMap,
  SessionDetailResponse,
  SessionSummary,
  SummaryReadyEvent,
  WebSocketEvent,
} from './types'

type AppState = {
  connected: boolean
  paused: boolean
  liveSegments: LiveTranscriptEvent[]
  sessionsByDate: Map<string, SessionSummary[]>
  sessionDetails: Map<string, SessionDetailResponse>
  dates: string[]
  warnings: string[]
  presets: PresetMap
  activeSessionId: string
  activeSessionStartedAt: number
  activeAudioSessionId: string
  interimText: string
  interimSpeaker: number
}

export const appState = $state<AppState>({
  connected: false,
  paused: false,
  liveSegments: [],
  sessionsByDate: new Map(),
  sessionDetails: new Map(),
  dates: [],
  warnings: [],
  presets: {},
  activeSessionId: '',
  activeSessionStartedAt: 0,
  activeAudioSessionId: '',
  interimText: '',
  interimSpeaker: -1,
})

export function getTodaysSessions(): SessionSummary[] {
  const today = new Date().toISOString().slice(0, 10)
  return appState.sessionsByDate.get(today) ?? []
}

export function setConnected(connected: boolean): void {
  appState.connected = connected
}

export function setPaused(paused: boolean): void {
  appState.paused = paused
}

export function setDates(dates: string[]): void {
  appState.dates = dates
}

export function setWarnings(warnings: string[]): void {
  appState.warnings = warnings
}

export function setPresets(presets: PresetMap): void {
  appState.presets = presets
}

export function setSessionsForDate(date: string, sessions: SessionSummary[]): void {
  const next = new Map(appState.sessionsByDate)
  next.set(date, sessions)
  appState.sessionsByDate = next
}

export function setSessionDetail(detail: SessionDetailResponse): void {
  const next = new Map(appState.sessionDetails)
  next.set(detail.session.id, detail)
  appState.sessionDetails = next
}

export function setActiveAudioSession(sessionId: string): void {
  appState.activeAudioSessionId = sessionId
}

export function appendLiveSegment(event: LiveTranscriptEvent): void {
  appState.liveSegments.push(event)
  if (appState.liveSegments.length > 400) {
    appState.liveSegments = appState.liveSegments.slice(-400)
  }
}

export function applySummaryUpdate(event: SummaryReadyEvent): void {
  const nextByDate = new Map(appState.sessionsByDate)

  for (const date of appState.dates) {
    const sessions = nextByDate.get(date)
    if (!sessions) {
      continue
    }

    nextByDate.set(
      date,
      sessions.map((session) =>
        session.id === event.session_id
          ? {
              ...session,
              summary: event.summary,
              summary_status: event.status,
              summary_preset: event.summary_preset ?? session.summary_preset,
            }
          : session,
      ),
    )
  }
  appState.sessionsByDate = nextByDate

  const detail = appState.sessionDetails.get(event.session_id)
  if (detail) {
    const nextDetails = new Map(appState.sessionDetails)
    nextDetails.set(event.session_id, {
      ...detail,
      session: {
        ...detail.session,
        summary: event.summary,
        summary_status: event.status,
        summary_preset: event.summary_preset ?? detail.session.summary_preset,
      },
    })
    appState.sessionDetails = nextDetails
  }
}

export function applyEvent(event: WebSocketEvent): void {
  switch (event.type) {
    case 'connection':
      setConnected(event.connected)
      return
    case 'status_changed':
      setPaused(event.paused)
      return
    case 'session_started':
      appState.activeSessionId = event.session_id
      appState.activeSessionStartedAt = Date.parse(event.timestamp)
      appState.liveSegments = []
      return
    case 'session_ended':
      appState.activeSessionId = ''
      appState.activeSessionStartedAt = 0
      return
    case 'summary_ready':
      applySummaryUpdate(event)
      return
    case 'live_transcript_interim':
      appState.interimText = event.text
      appState.interimSpeaker = event.speaker
      return
    case 'live_transcript':
      appState.interimText = ''
      appState.interimSpeaker = -1
      appendLiveSegment(event)
      return
    default:
      return
  }
}

export function resetState(): void {
  appState.connected = false
  appState.paused = false
  appState.liveSegments = []
  appState.sessionsByDate = new Map()
  appState.sessionDetails = new Map()
  appState.dates = []
  appState.activeSessionId = ''
  appState.activeSessionStartedAt = 0
  appState.activeAudioSessionId = ''
  appState.warnings = []
  appState.presets = {}
  appState.interimText = ''
  appState.interimSpeaker = -1
}
