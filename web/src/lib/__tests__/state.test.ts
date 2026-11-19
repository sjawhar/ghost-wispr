import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  appState,
  applyEvent,
  applySegmentDeletion,
  applySummaryUpdate,
  beginSessionDetailRequest,
  removeSession,
  resetState,
  setDates,
  setSessionDetail,
  setSessionDetailIfCurrent,
  setSessionsForDate,
} from '../state.svelte'
import {
  ComponentStatus,
  RefinementStatus,
  SummaryStatus,
  type SessionDetailResponse,
  type SessionSummary,
} from '../types'

function makeSession(id: string): SessionSummary {
  return {
    id,
    title: 'Deleted words title',
    started_at: '2026-11-11T10:00:00Z',
    ended_at: '2026-11-11T10:10:00Z',
    status: 'ended',
    summary: 'Deleted words summary',
    summary_status: SummaryStatus.Completed,
    summary_preset: 'default',
    audio_path: 'recording.mp3',
    summary_error: 'previous failure',
    refinement_status: RefinementStatus.Completed,
    refined_transcript: 'Deleted words refined transcript',
    canonical_transcript: 'Deleted words canonical transcript',
    transcript_source: 'refined',
  }
}

afterEach(() => {
  vi.useRealTimers()
  resetState()
})

describe('applySegmentDeletion', () => {
  it('scrubs deleted text from the list and detail while retaining other segments and sessions', () => {
    const deleted = makeSession('s1')
    const untouched = makeSession('s2')
    setSessionsForDate('2026-11-11', [deleted, untouched])
    const detail: SessionDetailResponse = {
      session: deleted,
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Delete these words',
          start_time: 0,
          end_time: 2,
          timestamp: '2026-11-11T10:00:00Z',
        },
        {
          id: 43,
          speaker: 1,
          text: 'Keep these words',
          start_time: 2,
          end_time: 4,
          timestamp: '2026-11-11T10:00:02Z',
        },
      ],
    }
    setSessionDetail(detail)

    applySegmentDeletion('s1', 42)

    const listSession = appState.sessionsByDate.get('2026-11-11')?.[0]
    expect(listSession).toMatchObject({
      title: '',
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_error: '',
      refinement_status: RefinementStatus.Completed,
      refined_transcript: '',
      canonical_transcript: '',
      transcript_source: 'refined',
    })
    expect(appState.sessionsByDate.get('2026-11-11')?.[1]).toEqual(untouched)
    expect(appState.sessionDetails.get('s1')).toEqual({
      session: listSession,
      segments: [detail.segments[1]],
    })
  })
})

describe('deletion state fences', () => {
  it('does not accept a detail response requested before the segment was deleted', () => {
    const session = makeSession('s1')
    const staleDetail: SessionDetailResponse = {
      session,
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Deleted words',
          start_time: 0,
          end_time: 2,
          timestamp: '2026-11-11T10:00:00Z',
        },
      ],
    }
    setSessionsForDate('2026-11-11', [session])
    setSessionDetail(staleDetail)
    const request = beginSessionDetailRequest('s1')

    applySegmentDeletion('s1', 42)

    expect(setSessionDetailIfCurrent(staleDetail, request)).toBe(false)
    expect(appState.sessionDetails.get('s1')?.segments).toEqual([])
    expect(appState.sessionDetails.get('s1')?.session.summary).toBe('')
  })

  it('ignores a summary event from before deletion and accepts a regenerated summary', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-11-11T10:00:00Z'))
    const session = makeSession('s1')
    setDates(['2026-11-11'])
    setSessionsForDate('2026-11-11', [session])

    applySegmentDeletion('s1', 42)
    applySummaryUpdate({
      type: 'summary_ready',
      version: 1,
      timestamp: '2026-11-11T09:59:59Z',
      session_id: 's1',
      title: 'Stale title',
      summary: 'Deleted words summary',
      status: SummaryStatus.Completed,
    })

    expect(appState.sessionsByDate.get('2026-11-11')?.[0]).toMatchObject({
      title: '',
      summary: '',
      summary_status: SummaryStatus.Pending,
    })

    applySummaryUpdate({
      type: 'summary_ready',
      version: 1,
      timestamp: '2026-11-11T10:00:01Z',
      session_id: 's1',
      title: 'Regenerated title',
      summary: 'Regenerated summary',
      status: SummaryStatus.Completed,
    })

    expect(appState.sessionsByDate.get('2026-11-11')?.[0]).toMatchObject({
      title: 'Regenerated title',
      summary: 'Regenerated summary',
      summary_status: SummaryStatus.Completed,
    })
  })

  it('keeps the reason a summary failed, and drops it once a new summary runs', () => {
    const session = makeSession('s1')
    setDates(['2026-11-11'])
    setSessionsForDate('2026-11-11', [session])
    setSessionDetail({ session, segments: [] })

    applySummaryUpdate({
      type: 'summary_ready',
      version: 1,
      timestamp: '2026-11-11T10:00:01Z',
      session_id: 's1',
      title: 'Kept title',
      summary: 'Kept summary',
      status: SummaryStatus.Failed,
      summary_error: 'summarization failed: the model answered with no summary text',
    })
    for (const shown of [
      appState.sessionsByDate.get('2026-11-11')?.[0],
      appState.sessionDetails.get('s1')?.session,
    ]) {
      expect(shown).toMatchObject({
        summary_status: SummaryStatus.Failed,
        summary_error: 'summarization failed: the model answered with no summary text',
      })
    }

    applySummaryUpdate({
      type: 'summary_ready',
      version: 1,
      timestamp: '2026-11-11T10:00:02Z',
      session_id: 's1',
      title: '',
      summary: '',
      status: SummaryStatus.Running,
    })
    for (const shown of [
      appState.sessionsByDate.get('2026-11-11')?.[0],
      appState.sessionDetails.get('s1')?.session,
    ]) {
      expect(shown).toMatchObject({ summary_status: SummaryStatus.Running, summary_error: '' })
    }
  })

  it('clears live segments only for the session whose segment was deleted', () => {
    applyEvent({
      type: 'session_started',
      version: 1,
      timestamp: '2026-11-11T10:00:00Z',
      session_id: 's1',
    })
    applyEvent({
      type: 'live_transcript',
      version: 1,
      timestamp: '2026-11-11T10:00:01Z',
      session_id: 's1',
      speaker: 0,
      text: 'Live words',
      start_time: 0,
      end_time: 2,
    })
    applyEvent({
      type: 'session_ended',
      version: 1,
      timestamp: '2026-11-11T10:00:02Z',
      session_id: 's1',
      duration: 2,
    })

    applySegmentDeletion('s2', 42)

    expect(appState.liveSegments).toHaveLength(1)
    expect(appState.liveSegmentsSessionId).toBe('s1')

    applySegmentDeletion('s1', 42)

    expect(appState.liveSegments).toEqual([])
    expect(appState.liveSegmentsSessionId).toBe('')
  })

  it('restores the live pane from a session-identified event after bootstrap misses its start', () => {
    applyEvent({
      type: 'live_transcript',
      version: 1,
      timestamp: '2026-11-11T10:00:01Z',
      session_id: 's1',
      speaker: 0,
      text: 'Recovered live words',
      start_time: 0,
      end_time: 2,
    })

    expect(appState.liveSegmentsSessionId).toBe('s1')
    expect(appState.liveSegments).toHaveLength(1)
  })

  it('drops a live event from another session while recording', () => {
    applyEvent({
      type: 'session_started',
      version: 1,
      timestamp: '2026-11-11T10:00:00Z',
      session_id: 's1',
    })

    applyEvent({
      type: 'live_transcript',
      version: 1,
      timestamp: '2026-11-11T10:00:01Z',
      session_id: 's2',
      speaker: 0,
      text: 'Other session words',
      start_time: 0,
      end_time: 2,
    })

    expect(appState.liveSegmentsSessionId).toBe('s1')
    expect(appState.liveSegments).toEqual([])
  })
})

describe('removeSession', () => {
  it('drops the component statuses about the removed session, and keeps the others', () => {
    for (const [component, sessionId, message] of [
      ['refinement', 's1', 'Batch refinement failed for session s1'],
      ['sync', 's2', 'Google Drive sync failed for session s2'],
      ['mic', undefined, 'Microphone reconnected'],
    ] as const) {
      applyEvent({
        type: 'component_status',
        version: 1,
        timestamp: '2026-11-11T10:00:00Z',
        component,
        status: ComponentStatus.Error,
        message,
        ...(sessionId ? { session_id: sessionId } : {}),
      })
    }
    expect(Object.keys(appState.componentStatuses).sort()).toEqual(['mic', 'refinement', 'sync'])

    removeSession('s1')

    expect(Object.keys(appState.componentStatuses).sort()).toEqual(['mic', 'sync'])
  })
})

describe('a deleted session', () => {
  const status = (component: 'refinement' | 'sync' | 'summary', sessionId: string) =>
    applyEvent({
      type: 'component_status',
      version: 1,
      timestamp: '2026-11-11T10:00:00Z',
      component,
      status: ComponentStatus.Error,
      message: `${component} failed for session ${sessionId}`,
      session_id: sessionId,
    })

  it('never shows a status of a removed session again', () => {
    removeSession('s1')
    status('refinement', 's1')
    status('sync', 's2')

    expect(Object.keys(appState.componentStatuses).sort()).toEqual(['sync'])
  })

  it('is dropped from every tab, with the sessions merged into it, when the server says so', () => {
    const merged = makeSession('m')
    const kept = makeSession('k')
    setSessionsForDate('2026-11-11', [merged, kept])
    setSessionDetail({ session: merged, segments: [] })
    status('refinement', 'm')
    status('sync', 'a')
    status('summary', 'k')

    applyEvent({
      type: 'session_deleted',
      version: 1,
      timestamp: '2026-11-11T10:01:00Z',
      session_id: 'm',
      source_session_ids: ['a', 'b'],
    } as never)

    expect(appState.sessionsByDate.get('2026-11-11')?.map((s) => s.id)).toEqual(['k'])
    expect(appState.sessionDetails.has('m')).toBe(false)
    expect(Object.keys(appState.componentStatuses).sort()).toEqual(['summary'])

    status('sync', 'b')
    status('refinement', 'm')
    expect(Object.keys(appState.componentStatuses).sort()).toEqual(['summary'])
  })
})

describe('a capture pause', () => {
  const interim = (text: string) =>
    applyEvent({
      type: 'live_transcript_interim',
      version: 1,
      timestamp: '2026-11-19T09:00:00Z',
      session_id: '',
      speaker: 0,
      text,
      start_time: 0,
    })
  const paused = (value: boolean) =>
    applyEvent({
      type: 'status_changed',
      version: 1,
      timestamp: '2026-11-19T09:00:01Z',
      paused: value,
    })

  // Words from before a pause must not show after it: an interim that no
  // session took (the pause came before Deepgram finalized it) would stay on
  // the page through the pause and the resume, as words of the meeting after.
  it('clears the interim text from before the pause, and shows none after the resume', () => {
    interim('Orange tractors climb')
    paused(true)
    expect(appState.interimText).toBe('')
    expect(appState.interimSpeaker).toBe(-1)

    paused(false)
    expect(appState.interimText).toBe('')
  })

  it('still shows the interim text that comes after the resume', () => {
    paused(true)
    paused(false)
    interim('Seven quiet violins')
    expect(appState.interimText).toBe('Seven quiet violins')
  })
})
