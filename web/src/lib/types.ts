export enum ComponentStatus {
  Connected = 'connected',
  Disconnected = 'disconnected',
  Reconnecting = 'reconnecting',
  Error = 'error',
  Unavailable = 'unavailable',
  Disabled = 'disabled',
  Open = 'open',
  Closed = 'closed',
  Synced = 'synced',
  Ok = 'ok',
  Draining = 'draining',
}

export enum SessionStatus {
  Active = 'active',
  Ended = 'ended',
  Discarded = 'discarded',
}

export enum SummaryStatus {
  Pending = 'pending',
  Running = 'running',
  Completed = 'completed',
  Failed = 'failed',
}

export enum SyncState {
  Pending = 'PENDING',
  Syncing = 'SYNCING',
  Synced = 'SYNCED',
  Failed = 'FAILED',
  RetryScheduled = 'RETRY_SCHEDULED',
}

export enum SyncStatus {
  Pending = 'pending',
  Syncing = 'syncing',
  Synced = 'synced',
  Failed = 'failed',
}

export enum RefinementStatus {
  Pending = 'pending',
  Running = 'running',
  Completed = 'completed',
  Failed = 'failed',
}

export enum HealthStatus {
  Healthy = 'healthy',
  Degraded = 'degraded',
  Error = 'error',
  Loading = 'loading',
}

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
  title: string
  summary: string
  status: SummaryStatus
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

export interface ComponentStatusEvent extends BaseEvent {
  type: 'component_status'
  component: 'deepgram' | 'summary' | 'sync' | 'mic'
  status: ComponentStatus
  message: string
}

export type WebSocketEvent =
  | LiveTranscriptEvent
  | LiveTranscriptInterimEvent
  | SessionStartedEvent
  | SessionEndedEvent
  | SummaryReadyEvent
  | StatusChangedEvent
  | ConnectionEvent
  | ComponentStatusEvent

export interface Segment {
  speaker: number
  text: string
  start_time: number
  end_time: number
  timestamp: string
}

export interface SessionSummary {
  id: string
  title: string
  started_at: string
  ended_at?: string
  status: SessionStatus | string
  summary: string
  summary_status: SummaryStatus
  summary_preset: string
  audio_path: string
  sync_status?: SyncStatus
  refinement_status?: RefinementStatus
  transcript_source?: string
}

export interface SessionDetailResponse {
  session: SessionSummary
  segments: Segment[]
}

export interface StatusResponse {
  paused: boolean
  warnings: string[]
  active_session_id: string
  active_session_started_at: string
}

export interface SearchResult {
  session_id: string
  title: string
  snippet: string
  rank: number
}

export type PresetMap = Record<string, string>
