import { ComponentStatus as ComponentStatusValue, RefinementStatus, SummaryStatus } from './types'
import type {
  ComponentStatusEvent,
  LiveTranscriptEvent,
  PresetMap,
  SessionDetailResponse,
  SessionSummary,
  SummaryReadyEvent,
  WebSocketEvent,
} from './types'

export type ComponentStatus = {
  status: ComponentStatusValue
  message: string
  timestamp: string
}

type AppState = {
  connected: boolean
  paused: boolean
  // Whether the microphone is capturing, from GET /api/status and mic
  // component_status events. After a resume it stays false until the
  // microphone is back.
  capturing: boolean
  liveSegments: LiveTranscriptEvent[]
  liveSegmentsSessionId: string
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
  componentStatuses: Record<string, ComponentStatus>
}

export const appState = $state<AppState>({
  connected: false,
  paused: false,
  capturing: false,
  liveSegments: [],
  liveSegmentsSessionId: '',
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
  componentStatuses: {},
})

const detailRequestVersions = new Map<string, number>()
const summaryDeletionTimes = new Map<string, number>()

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

export function setCapturing(capturing: boolean): void {
  appState.capturing = capturing
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

export function beginSessionDetailRequest(sessionID: string): number {
  return detailRequestVersions.get(sessionID) ?? 0
}

export function setSessionDetailIfCurrent(
  detail: SessionDetailResponse,
  requestVersion: number,
): boolean {
  if ((detailRequestVersions.get(detail.session.id) ?? 0) !== requestVersion) {
    return false
  }
  setSessionDetail(detail)
  return true
}

function invalidateSessionDetailRequest(sessionID: string): void {
  detailRequestVersions.set(sessionID, (detailRequestVersions.get(sessionID) ?? 0) + 1)
}

export function setLiveSegmentsForSession(
  sessionID: string,
  segments: LiveTranscriptEvent[],
): void {
  appState.liveSegmentsSessionId = sessionID
  appState.liveSegments = segments
}

// The server has removed the selected timed occurrence and queued a fresh
// summary. Keep the completed-refinement state so the remaining timed
// segments remain the default view until the detail refresh arrives.
function withoutDeletedText(session: SessionSummary): SessionSummary {
  return {
    ...session,
    title: '',
    summary: '',
    summary_status: SummaryStatus.Pending,
    summary_error: '',
    refined_transcript: '',
    canonical_transcript: '',
    transcript_source: 'refined',
  }
}

export function applySegmentDeletion(sessionID: string, segmentID: number): void {
  invalidateSessionDetailRequest(sessionID)
  summaryDeletionTimes.set(sessionID, Date.now())

  if (appState.liveSegmentsSessionId === sessionID) {
    appState.liveSegmentsSessionId = ''
    appState.liveSegments = []
  }

  const nextByDate = new Map(appState.sessionsByDate)
  for (const [date, sessions] of nextByDate) {
    if (sessions.some((session) => session.id === sessionID)) {
      nextByDate.set(
        date,
        sessions.map((session) =>
          session.id === sessionID ? withoutDeletedText(session) : session,
        ),
      )
    }
  }
  appState.sessionsByDate = nextByDate

  const detail = appState.sessionDetails.get(sessionID)
  if (!detail) {
    return
  }
  const next = new Map(appState.sessionDetails)
  next.set(sessionID, {
    session: withoutDeletedText(detail.session),
    segments: detail.segments.filter((segment) => segment.id !== segmentID),
  })
  appState.sessionDetails = next
}

export function setActiveAudioSession(sessionId: string): void {
  appState.activeAudioSessionId = sessionId
}

export function appendLiveSegment(event: LiveTranscriptEvent): void {
  if (appState.liveSegmentsSessionId !== event.session_id) {
    if (appState.activeSessionId && appState.activeSessionId !== event.session_id) {
      return
    }
    setLiveSegmentsForSession(event.session_id, [])
  }
  appState.liveSegments.push(event)
  if (appState.liveSegments.length > 400) {
    appState.liveSegments = appState.liveSegments.slice(-400)
  }
}

export function applySummaryUpdate(event: SummaryReadyEvent): void {
  const deletedAt = summaryDeletionTimes.get(event.session_id)
  if (deletedAt !== undefined && Date.parse(event.timestamp) <= deletedAt) {
    return
  }

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
              title: event.title || session.title,
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
        title: event.title || detail.session.title,
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
      setLiveSegmentsForSession(event.session_id, [])
      appState.interimText = ''
      appState.interimSpeaker = -1
      return
    case 'session_ended':
      if (appState.activeSessionId === event.session_id) {
        appState.activeSessionId = ''
        appState.activeSessionStartedAt = 0
        appState.interimText = ''
        appState.interimSpeaker = -1
      }
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
    case 'component_status':
      applyComponentStatus(event)
      return
    default:
      return
  }
}

export function applyComponentStatus(event: ComponentStatusEvent): void {
  appState.componentStatuses = {
    ...appState.componentStatuses,
    [event.component]: {
      status: event.status,
      message: event.message,
      timestamp: event.timestamp,
    },
  }
  if (event.component === 'mic') {
    setCapturing(
      event.status === ComponentStatusValue.Connected || event.status === ComponentStatusValue.Open,
    )
  }
}

export function resetState(): void {
  appState.connected = false
  appState.paused = false
  appState.capturing = false
  appState.liveSegments = []
  appState.liveSegmentsSessionId = ''
  detailRequestVersions.clear()
  summaryDeletionTimes.clear()
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
  appState.componentStatuses = {}
}

export function removeSession(sessionId: string): void {
  invalidateSessionDetailRequest(sessionId)
  if (appState.liveSegmentsSessionId === sessionId) {
    appState.liveSegments = []
    appState.liveSegmentsSessionId = ''
  }
  const nextByDate = new Map(appState.sessionsByDate)
  for (const [date, sessions] of nextByDate) {
    const filtered = sessions.filter((s) => s.id !== sessionId)
    if (filtered.length !== sessions.length) {
      nextByDate.set(date, filtered)
    }
  }
  appState.sessionsByDate = nextByDate

  if (appState.sessionDetails.has(sessionId)) {
    const nextDetails = new Map(appState.sessionDetails)
    nextDetails.delete(sessionId)
    appState.sessionDetails = nextDetails
  }
}
