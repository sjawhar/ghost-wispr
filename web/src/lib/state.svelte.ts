import { fetchVoiceTasks, requestErrorMessage } from './api'
import { ComponentStatus as ComponentStatusValue, SummaryStatus } from './types'
import type {
  ComponentStatusEvent,
  LiveTranscriptEvent,
  PresetMap,
  SessionDetailResponse,
  SessionSummary,
  SummaryReadyEvent,
  VoiceTask,
  WebSocketEvent,
} from './types'

export type ComponentStatus = {
  status: ComponentStatusValue
  message: string
  timestamp: string
  // The session the status is about, from the event's session_id.
  sessionId?: string
  // The number of the change, for a component whose changes are numbered
  // (Deepgram's): a status with a lower one is older and is not applied.
  seq?: number
}

// A session's questions to agents as last read, and why the last read
// failed ('' when it did not), so a failed read never shows as no questions.
export type SessionVoiceTasks = {
  tasks: VoiceTask[]
  error: string
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
  // By session, its questions to agents, once the tab has read them.
  voiceTasks: Map<string, SessionVoiceTasks>
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
  voiceTasks: new Map(),
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
// By session, the number of the latest read of its questions to agents: only
// that read's answer is shown.
const voiceTaskReads = new Map<string, number>()

export function getTodaysSessions(): SessionSummary[] {
  const today = new Date().toISOString().slice(0, 10)
  return appState.sessionsByDate.get(today) ?? []
}

export function setConnected(connected: boolean): void {
  appState.connected = connected
  if (connected) {
    // A new connection can be to a restarted unit, which numbers its changes
    // from 1 again: the statuses it sends replace the ones this page holds.
    appState.componentStatuses = Object.fromEntries(
      Object.entries(appState.componentStatuses).map(([component, held]) => [
        component,
        { ...held, seq: undefined },
      ]),
    )
  }
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

// clearLiveSegmentsOf empties the live pane when it shows session sessionID.
function clearLiveSegmentsOf(sessionID: string): void {
  if (appState.liveSegmentsSessionId === sessionID) {
    setLiveSegmentsForSession('', [])
  }
}

// loadVoiceTasks reads the session's questions to agents. Only the latest
// read is shown, and none of a session this tab removed. A failed read keeps
// the questions shown and says why.
export async function loadVoiceTasks(sessionID: string): Promise<void> {
  if (removedSessions.has(sessionID)) {
    return
  }
  const read = (voiceTaskReads.get(sessionID) ?? 0) + 1
  voiceTaskReads.set(sessionID, read)
  let next: SessionVoiceTasks
  try {
    next = { tasks: await fetchVoiceTasks(sessionID), error: '' }
  } catch (error) {
    next = {
      tasks: appState.voiceTasks.get(sessionID)?.tasks ?? [],
      error: requestErrorMessage(error),
    }
  }
  if (voiceTaskReads.get(sessionID) !== read || removedSessions.has(sessionID)) {
    return
  }
  const voiceTasks = new Map(appState.voiceTasks)
  voiceTasks.set(sessionID, next)
  appState.voiceTasks = voiceTasks
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

  clearLiveSegmentsOf(sessionID)

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

// applySummaryUpdate applies a summary event to the session's list entry and
// detail. A failed summary carries why it failed; any other status clears the
// reason, because the attempt it described is over.
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
              summary_error: event.summary_error ?? '',
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
        summary_error: event.summary_error ?? '',
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
      // Interim text from before a pause, which no session took, must not
      // show after it as words of the meeting that follows.
      appState.interimText = ''
      appState.interimSpeaker = -1
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
    case 'session_deleted':
      for (const id of [event.session_id, ...(event.source_session_ids ?? [])]) {
        removeSession(id)
      }
      return
    // An answer landed or was spoken: a session whose questions this tab
    // read reads them again.
    case 'voice_answer_ready':
    case 'voice_spoken':
    case 'voice_tasks_changed':
      if (voiceTaskReads.has(event.session_id)) {
        void loadVoiceTasks(event.session_id)
      }
      return
    default:
      return
  }
}

// The sessions this tab removed: a status about one of them is never shown
// again, also one the server sent before it learnt of the delete.
const removedSessions = new Set<string>()

export function applyComponentStatus(event: ComponentStatusEvent): void {
  if (event.session_id && removedSessions.has(event.session_id)) {
    return
  }
  // A page can get a component's status twice, older after newer: from the
  // snapshot it is sent on connecting and from the events queued meanwhile.
  const held = appState.componentStatuses[event.component]
  if (event.seq && held?.seq && event.seq < held.seq) {
    return
  }
  appState.componentStatuses = {
    ...appState.componentStatuses,
    [event.component]: {
      status: event.status,
      message: event.message,
      timestamp: event.timestamp,
      sessionId: event.session_id,
      seq: event.seq,
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
  voiceTaskReads.clear()
  removedSessions.clear()
  appState.sessionsByDate = new Map()
  appState.sessionDetails = new Map()
  appState.voiceTasks = new Map()
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

// removeSession drops a deleted session from this tab: its list entry, its
// detail (title, summary, segments), its questions to agents, its live
// segments and its statuses, and keeps any later status about it out.
export function removeSession(sessionId: string): void {
  removedSessions.add(sessionId)
  invalidateSessionDetailRequest(sessionId)
  clearLiveSegmentsOf(sessionId)
  if (appState.activeAudioSessionId === sessionId) {
    appState.activeAudioSessionId = ''
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
  if (appState.voiceTasks.has(sessionId)) {
    const nextVoiceTasks = new Map(appState.voiceTasks)
    nextVoiceTasks.delete(sessionId)
    appState.voiceTasks = nextVoiceTasks
  }
  appState.componentStatuses = Object.fromEntries(
    Object.entries(appState.componentStatuses).filter(([, s]) => s.sessionId !== sessionId),
  )
}
