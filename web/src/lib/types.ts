export interface BaseEvent {
  type: string
  version: number
  timestamp: string
}

export interface LiveTranscriptEvent extends BaseEvent {
  type: 'live_transcript'
  speaker: number
  text: string
  start_time: number
  end_time: number
}

export interface LiveTranscriptInterimEvent extends BaseEvent {
  type: 'live_transcript_interim'
  speaker: number
  text: string
  start_time: number
}

export interface SessionStartedEvent extends BaseEvent {
  type: 'session_started'
  session_id: string
}

export interface SessionEndedEvent extends BaseEvent {
  type: 'session_ended'
  session_id: string
  duration: number
}

export interface SummaryReadyEvent extends BaseEvent {
  type: 'summary_ready'
  session_id: string
  summary: string
  status: 'pending' | 'running' | 'completed' | 'failed'
  summary_preset?: string
}

export interface StatusChangedEvent extends BaseEvent {
  type: 'status_changed'
  paused: boolean
}

export interface ConnectionEvent extends BaseEvent {
  type: 'connection'
  connected: boolean
}

export type WebSocketEvent =
  | LiveTranscriptEvent
  | LiveTranscriptInterimEvent
  | SessionStartedEvent
  | SessionEndedEvent
  | SummaryReadyEvent
  | StatusChangedEvent
  | ConnectionEvent

export interface Segment {
  speaker: number
  text: string
  start_time: number
  end_time: number
  timestamp: string
}

export interface SessionSummary {
  id: string
  started_at: string
  ended_at?: string
  status: string
  summary: string
  summary_status: 'pending' | 'running' | 'completed' | 'failed'
  summary_preset: string
  audio_path: string
}

export interface SessionDetailResponse {
  session: SessionSummary
  segments: Segment[]
}

export interface StatusResponse {
  paused: boolean
  warnings: string[]
}

export type PresetMap = Record<string, string>
