import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  appState,
  applyEvent,
  applySegmentDeletion,
  applySummaryUpdate,
  beginSessionDetailRequest,
  resetState,
  setDates,
  setSessionDetail,
  setSessionDetailIfCurrent,
  setSessionsForDate,
} from '../state.svelte'
import {
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
      refinement_status: RefinementStatus.Pending,
      refined_transcript: '',
      canonical_transcript: '',
      transcript_source: 'streaming',
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
})
