import { fireEvent, render as renderSvelte, screen, waitFor } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SessionList from '../SessionList.svelte'
import { RequestError } from '../../lib/api'
import {
  SessionStatus,
  SummaryStatus,
  type SessionDetailResponse,
  type SessionSummary,
} from '../../lib/types'

type SessionListProps = ComponentProps<typeof SessionList>

function render(
  props: Omit<SessionListProps, 'onRefreshSession' | 'onSegmentDeleted'> &
    Partial<Pick<SessionListProps, 'onRefreshSession' | 'onSegmentDeleted'>>,
) {
  return renderSvelte(SessionList, {
    onRefreshSession: vi.fn().mockResolvedValue(undefined),
    onSegmentDeleted: vi.fn(),
    ...props,
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
})
describe('SessionList', () => {
  it('renders empty state', () => {
    render({
      dates: [],
      sessionsByDate: new Map(),
      sessionDetails: new Map(),
      presets: {},
      expandedSessionId: '',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
      onDelete: vi.fn().mockResolvedValue(undefined),
      onMerge: vi.fn().mockResolvedValue(undefined),
    })

    expect(screen.getByText('No sessions yet.')).toBeTruthy()
  })

  it('loads previous dates on button click', async () => {
    const dates = ['2026-02-26', '2026-02-25', '2026-02-24', '2026-02-23']
    render({
      dates,
      sessionsByDate: new Map([
        ['2026-02-26', []],
        ['2026-02-25', []],
        ['2026-02-24', []],
        ['2026-02-23', []],
      ]),
      sessionDetails: new Map(),
      presets: {},
      expandedSessionId: '',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
      onDelete: vi.fn().mockResolvedValue(undefined),
      onMerge: vi.fn().mockResolvedValue(undefined),
    })

    const button = screen.getByRole('button', { name: 'Load previous' })
    await fireEvent.click(button)
    expect(screen.queryByRole('button', { name: 'Load previous' })).toBeNull()
  })

  it.each([
    ['after a successful segment deletion', 204, 1],
    ['when the segment is already gone', 404, 0],
  ])('clears search results %s before refreshing', async (_, status, deletedCalls) => {
    const session: SessionSummary = {
      id: 's1',
      title: 'Fixture session',
      started_at: '2026-11-11T10:00:00Z',
      ended_at: '2026-11-11T10:10:00Z',
      status: SessionStatus.Ended,
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: 'default',
      audio_path: 'fixture.mp3',
    }
    const detail: SessionDetailResponse = {
      session,
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Deleted phrase',
          start_time: 0,
          end_time: 2,
          timestamp: '2026-11-11T10:00:00Z',
        },
      ],
    }
    const calls: string[] = []
    const onSegmentDeleted = vi.fn(() => calls.push('scrub'))
    const onRefreshSession = vi.fn(async () => {
      calls.push('refresh')
    })
    const fetch = vi.fn((input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/search?')) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { session_id: 's1', title: 'Fixture session', snippet: 'Deleted phrase' },
            ]),
            { status: 200 },
          ),
        )
      }
      return Promise.resolve(new Response(null, { status }))
    })
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    vi.stubGlobal('fetch', fetch)
    render({
      dates: ['2026-11-11'],
      sessionsByDate: new Map([['2026-11-11', [session]]]),
      sessionDetails: new Map([['s1', detail]]),
      presets: {},
      expandedSessionId: 's1',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
      onDelete: vi.fn().mockResolvedValue(undefined),
      onMerge: vi.fn().mockResolvedValue(undefined),
      onSegmentDeleted,
      onRefreshSession,
    })

    const search = screen.getByTestId('search-input')
    await fireEvent.input(search, { target: { value: 'Deleted phrase' } })
    expect(await screen.findByTestId('search-results')).toBeTruthy()

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Delete text and audio' }))

    await waitFor(() => {
      expect(onSegmentDeleted).toHaveBeenCalledTimes(deletedCalls)
      expect(onRefreshSession).toHaveBeenCalledExactlyOnceWith('s1')
      expect((search as HTMLInputElement).value).toBe('')
    })
    expect(calls).toEqual(deletedCalls === 1 ? ['scrub', 'refresh'] : ['refresh'])
    expect(screen.queryByTestId('search-results')).toBeNull()
  })

  it('clears the segment locally when uncertain deletion refresh fails', async () => {
    const session: SessionSummary = {
      id: 's1',
      title: 'Fixture session',
      started_at: '2026-11-11T10:00:00Z',
      ended_at: '2026-11-11T10:10:00Z',
      status: SessionStatus.Ended,
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: 'default',
      audio_path: 'fixture.mp3',
    }
    const onSegmentDeleted = vi.fn()
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 500 })))
    render({
      dates: ['2026-11-11'],
      sessionsByDate: new Map([['2026-11-11', [session]]]),
      sessionDetails: new Map([
        [
          's1',
          {
            session,
            segments: [
              {
                id: 42,
                speaker: 0,
                text: 'Possibly deleted phrase',
                start_time: 0,
                end_time: 2,
                timestamp: '2026-11-11T10:00:00Z',
              },
            ],
          },
        ],
      ]),
      presets: {},
      expandedSessionId: 's1',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
      onDelete: vi.fn().mockResolvedValue(undefined),
      onMerge: vi.fn().mockResolvedValue(undefined),
      onSegmentDeleted,
      onRefreshSession: vi.fn().mockRejectedValue(new Error('refresh unavailable')),
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Delete text and audio' }))

    await waitFor(() => {
      expect(onSegmentDeleted).toHaveBeenCalledWith('s1', 42)
      expect(screen.queryByText('Possibly deleted phrase')).toBeNull()
    })
  })

  const refusal =
    'a session to merge still has a deleted segment on Google Drive; merge it after its Drive files are replaced'

  function endedSession(id: string, title: string): SessionSummary {
    return {
      id,
      title,
      started_at: '2026-11-11T10:00:00Z',
      ended_at: '2026-11-11T10:10:00Z',
      status: SessionStatus.Ended,
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: 'default',
      audio_path: `${id}.mp3`,
    }
  }

  it('shows why a merge was refused and keeps the selection', async () => {
    const sessions = [endedSession('s1', 'First'), endedSession('s2', 'Second')]
    const onMerge = vi
      .fn()
      .mockRejectedValue(new RequestError(409, JSON.stringify({ error: refusal })))
    const { container } = render({
      dates: ['2026-11-11'],
      sessionsByDate: new Map([['2026-11-11', sessions]]),
      sessionDetails: new Map(),
      presets: {},
      expandedSessionId: '',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
      onDelete: vi.fn().mockResolvedValue(undefined),
      onMerge,
    })
    for (const checkbox of container.querySelectorAll<HTMLInputElement>('.session-checkbox')) {
      await fireEvent.click(checkbox)
    }
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }))

    expect(onMerge).toHaveBeenCalledWith(['s1', 's2'])
    expect((await screen.findByRole('alert')).textContent).toBe(`Merge refused: ${refusal}`)
    expect(screen.getByText('2 selected')).toBeTruthy()
  })

  it('shows why a session delete was refused', async () => {
    const reason =
      'this session still has a deleted segment on Google Drive; delete it after its Drive files are replaced'
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    render({
      dates: ['2026-11-11'],
      sessionsByDate: new Map([['2026-11-11', [endedSession('s1', 'First')]]]),
      sessionDetails: new Map(),
      presets: {},
      expandedSessionId: '',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
      onDelete: vi.fn().mockRejectedValue(new RequestError(409, JSON.stringify({ error: reason }))),
      onMerge: vi.fn().mockResolvedValue(undefined),
    })
    await fireEvent.click(screen.getAllByRole('button', { name: /from this unit/ })[0])

    expect((await screen.findByRole('alert')).textContent).toBe(`Not deleted: ${reason}`)
  })
})
