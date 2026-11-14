import { fireEvent, render as renderSvelte, screen, waitFor } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SessionCard from '../SessionCard.svelte'
import sessionCardSource from '../SessionCard.svelte?raw'
import {
  RefinementStatus,
  SessionStatus,
  SummaryStatus,
  SyncState,
  SyncStatus,
  type SessionSummary,
} from '../../lib/types'

const baseSession: SessionSummary = {
  id: 's1',
  title: '',
  started_at: new Date('2026-02-26T10:00:00Z').toISOString(),
  ended_at: new Date('2026-02-26T10:10:00Z').toISOString(),
  status: SessionStatus.Ended,
  summary: '',
  summary_status: SummaryStatus.Pending,
  summary_preset: 'default',
  audio_path: 'data/audio/s1.mp3',
  refinement_status: RefinementStatus.Completed,
  refined_revision: 1,
}

type SessionCardProps = ComponentProps<typeof SessionCard>

function render(
  props: Omit<SessionCardProps, 'onRefreshSession' | 'onSegmentDeleted'> &
    Partial<Pick<SessionCardProps, 'onRefreshSession' | 'onSegmentDeleted'>>,
) {
  return renderSvelte(SessionCard, {
    onRefreshSession: vi.fn().mockResolvedValue(undefined),
    onSegmentDeleted: vi.fn(),
    ...props,
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('SessionCard', () => {
  it('shows summary status', () => {
    render({
      session: baseSession,
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByText(`Summary: ${SummaryStatus.Pending}`)).toBeTruthy()
    expect(screen.getByText('Summarizing...')).toBeTruthy()
  })

  it('labels whole-conversation removal as local-only once Drive has the session', () => {
    const synced = {
      ...baseSession,
      sync_status: SyncStatus.Synced,
      sync_state: SyncState.Synced,
    }
    render({
      session: synced,
      detail: { session: synced, segments: [] },
      expanded: true,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(
      screen.getAllByRole('button', {
        name: 'Remove from this unit; the shared drive keeps its copy',
      }),
    ).toHaveLength(2)
  })

  it.each([
    ['never synced', {}],
    ['still pending', { sync_status: SyncStatus.Pending, sync_state: SyncState.Pending }],
    ['failed', { sync_status: SyncStatus.Failed, sync_state: SyncState.Failed }],
    ['deleted on Drive', { sync_status: SyncStatus.Failed, sync_state: SyncState.RemoteDeleted }],
  ])('says this unit holds the only copy when the session is %s', (_, sync) => {
    const session = { ...baseSession, ...sync }
    render({
      session,
      detail: { session, segments: [] },
      expanded: true,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(
      screen.getAllByRole('button', {
        name: 'Delete from this unit; this unit holds the only copy',
      }),
    ).toHaveLength(2)
    expect(
      screen.queryByRole('button', {
        name: 'Remove from this unit; the shared drive keeps its copy',
      }),
    ).toBeNull()
  })

  it('loads details when opened', async () => {
    const onToggle = vi.fn()
    const onLoadDetail = vi.fn().mockResolvedValue(undefined)
    render({
      session: baseSession,
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle,
      onLoadDetail,
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    await fireEvent.click(screen.getByRole('button', { name: /10:00/ }))
    expect(onToggle).toHaveBeenCalledTimes(1)
    expect(onLoadDetail).toHaveBeenCalledWith('s1')
  })

  it('shows resummarize trigger when completed and presets exist', () => {
    render({
      session: {
        ...baseSession,
        summary_status: SummaryStatus.Completed,
        summary: 'Done summary',
      },
      detail: undefined,
      expanded: false,
      presets: { default: 'General', detailed: 'Detailed' },
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByRole('button', { name: 'Resummarize ▾' })).toBeTruthy()
  })

  it('shows the summary a session keeps when making it again failed', () => {
    render({
      session: {
        ...baseSession,
        summary_status: SummaryStatus.Failed,
        summary: 'The last good summary.',
      },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByText('The last good summary.')).toBeTruthy()
    expect(screen.getByText('The last attempt failed; this is the previous summary.')).toBeTruthy()
    expect(screen.queryByText('Summary unavailable')).toBeNull()
  })

  it('says the summary is unavailable when one failed and there is none from before', () => {
    render({
      session: { ...baseSession, summary_status: SummaryStatus.Failed, summary: '' },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByText('Summary unavailable')).toBeTruthy()
  })

  it('says a session with no saved recording cannot be refined, and offers no retry', () => {
    render({
      session: { ...baseSession, audio_path: '', refinement_status: RefinementStatus.NoAudio },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    const badge = screen.getByTestId('refinement-badge')
    expect(badge.textContent?.trim()).toBe('Refine: no recording')
    expect(badge.getAttribute('title')).toBe(
      'No recording was saved for this session, so it cannot be refined',
    )
    expect(screen.queryByTestId('retry-refinement-btn')).toBeNull()
    expect(screen.queryByTestId('audio-removed-badge')).toBeNull()
  })

  it.each([
    [
      'a listed Drive recording',
      {
        ...baseSession,
        audio_path: '',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.NoAudio,
        sync_status: SyncStatus.Synced,
        sync_state: SyncState.Synced,
        gdrive_folder_id: 'folder-1',
      } as SessionSummary & { gdrive_folder_id: string },
      true,
    ],
    [
      'a session without a listed Drive folder',
      {
        ...baseSession,
        audio_path: '',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.NoAudio,
        sync_status: SyncStatus.Synced,
        sync_state: SyncState.Synced,
      },
      false,
    ],
    [
      'a never-recorded session with a Drive folder',
      {
        ...baseSession,
        audio_path: '',
        refinement_status: RefinementStatus.NoAudio,
        sync_status: SyncStatus.Synced,
        sync_state: SyncState.Synced,
        gdrive_folder_id: 'folder-1',
      },
      false,
    ],
  ])('offers Retry refinement only for %s', (_, session, wantsRetry) => {
    render({
      session,
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(Boolean(screen.queryByTestId('retry-refinement-btn'))).toBe(wantsRetry)
  })

  it('identifies a retryable session restored from Drive', () => {
    render({
      session: {
        ...baseSession,
        audio_path: '',
        audio_removed_at: '2026-11-13T09:01:00Z',
        restored_at: '2026-11-13T09:01:00Z',
        refinement_status: RefinementStatus.NoAudio,
        sync_status: SyncStatus.Synced,
        sync_state: SyncState.Synced,
        gdrive_folder_id: 'folder-1',
      } as SessionSummary & { restored_at: string },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByTestId('refinement-badge').getAttribute('title')).toBe(
      'This conversation was restored from Drive; retry refinement to delete from it',
    )
    expect(screen.getByTestId('retry-refinement-btn')).toBeTruthy()
    expect(screen.queryByTestId('audio-removed-badge')).toBeNull()
  })

  it('keeps the removal badge after a restored refinement fails', () => {
    render({
      session: {
        ...baseSession,
        audio_path: '',
        audio_removed_at: '2026-11-13T09:01:00Z',
        restored_at: '2026-11-13T09:01:00Z',
        refinement_status: RefinementStatus.Failed,
      } as SessionSummary & { restored_at: string },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByTestId('audio-removed-badge')).toBeTruthy()
  })

  it('marks a session whose audio was removed to free space, refined or not', () => {
    for (const refinementStatus of [RefinementStatus.Completed, RefinementStatus.NoAudio]) {
      const { unmount } = render({
        session: {
          ...baseSession,
          audio_path: '',
          audio_removed_at: '2026-11-07T09:30:00Z',
          refinement_status: refinementStatus,
        },
        detail: undefined,
        expanded: false,
        presets: {},
        onToggle: vi.fn(),
        onLoadDetail: vi.fn(),
        onResummarize: vi.fn(),
        onDelete: vi.fn(),
      })

      const marker = screen.getByTestId('audio-removed-badge')
      expect(marker.textContent?.trim()).toBe('Audio removed to free space')
      expect(marker.getAttribute('title')).toBe(
        'The audio was removed to keep the audio directory under its size limit. The transcript and summary are kept.',
      )
      if (refinementStatus === RefinementStatus.NoAudio) {
        expect(screen.getByTestId('refinement-badge').getAttribute('title')).toBe(
          'The audio was removed to free space, so it cannot be refined',
        )
      }
      expect(screen.queryByTestId('retry-refinement-btn')).toBeNull()
      unmount()
    }
  })

  it('says only part of the audio was removed, and how much is left', () => {
    render({
      session: {
        ...baseSession,
        audio_path: 'data/audio/b.mp3, data/audio/c.mp3',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.Pending,
      },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    const marker = screen.getByTestId('audio-removed-badge')
    expect(marker.textContent?.trim()).toBe('Some audio removed to free space')
    expect(marker.getAttribute('title')).toBe(
      'Part of the audio was removed to keep the audio directory under its size limit; 2 recordings are left. The transcript and summary are kept.',
    )
  })

  it('says the recording left was not on disk when GC found none, in the marker and the refinement badge', () => {
    render({
      session: {
        ...baseSession,
        audio_path: 'mnt/b.mp3',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.NoAudio,
      },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    expect(screen.getByTestId('audio-removed-badge').getAttribute('title')).toBe(
      'Part of the audio was removed to keep the audio directory under its size limit; 1 recording is left, but it was not on disk when the rest was removed. The transcript and summary are kept.',
    )
    expect(screen.getByTestId('refinement-badge').getAttribute('title')).toBe(
      'Part of the audio was removed to free space, and the recording left was not on disk, so it cannot be refined',
    )
  })

  it('shows why a sync retry was refused', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            error:
              'cannot sync session s1 now: invalid sync state transition: REMOTE_DELETED -> SYNCING',
          }),
          { status: 409 },
        ),
      ),
    )
    try {
      render({
        session: { ...baseSession, sync_status: SyncStatus.Failed },
        detail: undefined,
        expanded: false,
        presets: {},
        onToggle: vi.fn(),
        onLoadDetail: vi.fn(),
        onResummarize: vi.fn(),
        onDelete: vi.fn(),
      })

      await fireEvent.click(screen.getByTestId('retry-sync-btn'))
      const reason = await screen.findByTestId('retry-sync-error')
      expect(reason.textContent).toBe(
        'cannot sync session s1 now: invalid sync state transition: REMOTE_DELETED -> SYNCING',
      )
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('shows why a refinement retry was refused', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            error: 'the recording was removed from this unit and its Drive folder is deleted',
          }),
          {
            status: 409,
          },
        ),
      ),
    )
    try {
      render({
        session: {
          ...baseSession,
          audio_path: '',
          audio_removed_at: '2026-11-07T09:30:00Z',
          refinement_status: RefinementStatus.NoAudio,
          sync_status: SyncStatus.Synced,
          sync_state: SyncState.Synced,
          gdrive_folder_id: 'folder-1',
        },
        detail: undefined,
        expanded: false,
        presets: {},
        onToggle: vi.fn(),
        onLoadDetail: vi.fn(),
        onResummarize: vi.fn(),
        onDelete: vi.fn(),
      })

      await fireEvent.click(screen.getByTestId('retry-refinement-btn'))
      expect(
        await screen.findByText(
          'the recording was removed from this unit and its Drive folder is deleted',
        ),
      ).toBeTruthy()
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('refreshes Retry refinement and re-enables it when the status changes', async () => {
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValue(
          new Response('{}', { status: 202, headers: { 'Content-Type': 'application/json' } }),
        ),
    )
    const session = {
      ...baseSession,
      audio_path: '',
      audio_removed_at: '2026-11-07T09:30:00Z',
      refinement_status: RefinementStatus.NoAudio,
      sync_status: SyncStatus.Synced,
      sync_state: SyncState.Synced,
      gdrive_folder_id: 'folder-1',
    }
    const onRefreshSession = vi.fn().mockResolvedValue(undefined)
    const props = {
      session,
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
      onRefreshSession,
    }
    const view = render(props)

    await fireEvent.click(screen.getByTestId('retry-refinement-btn'))
    await waitFor(() => {
      expect(onRefreshSession).toHaveBeenCalledExactlyOnceWith('s1')
    })
    await view.rerender({
      ...props,
      session: { ...session, refinement_status: RefinementStatus.Failed },
    })
    await waitFor(() => {
      expect(screen.getByTestId('retry-refinement-btn').hasAttribute('disabled')).toBe(false)
    })
  })

  it('styles the audio-removed marker', () => {
    expect(sessionCardSource).toMatch(/\.status-badge\.audio-removed\b/)
  })

  it('gives every refinement status its own badge style', () => {
    for (const status of Object.values(RefinementStatus)) {
      expect(sessionCardSource, `no badge style for refinement status "${status}"`).toMatch(
        new RegExp(`\\.status-badge\\.${status}\\b`),
      )
    }
  })

  it('says a session whose Drive folder was deleted is kept on the device, and offers a retry for a restored folder', () => {
    render({
      session: {
        ...baseSession,
        sync_status: SyncStatus.Failed,
        sync_state: SyncState.RemoteDeleted,
      },
      detail: undefined,
      expanded: false,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
    })

    const badge = screen.getByTestId('sync-badge')
    expect(badge.textContent).toContain('Sync: deleted on Drive')
    expect(badge.getAttribute('title')).toBe(
      'Its Drive folder is in the trash or gone, so this device keeps the only copy. Restore the folder on Drive, then retry to sync it again.',
    )
    expect(screen.getByTestId('retry-sync-btn')).toBeTruthy()
  })

  it.each([
    ['updates state and refreshes once after a successful deletion', 204, 1],
    ['refreshes first when a segment is already gone', 404, 0],
  ])('%s', async (_, status, deletedCalls) => {
    const onSegmentDeleted = vi.fn()
    const onRefreshSession = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status })))
    render({
      session: baseSession,
      detail: {
        session: baseSession,
        segments: [
          {
            id: 42,
            speaker: 0,
            text: 'A stored segment',
            start_time: 0,
            end_time: 2,
            timestamp: '2026-11-11T10:00:00Z',
            transcript_revision: 1,
          },
        ],
      },
      expanded: true,
      presets: {},
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
      onResummarize: vi.fn(),
      onDelete: vi.fn(),
      onSegmentDeleted,
      onRefreshSession,
      onSegmentNotFound: async (sessionID) => {
        await onRefreshSession(sessionID)
        return false
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Delete text and audio' }))

    await waitFor(() => {
      expect(onRefreshSession).toHaveBeenCalledExactlyOnceWith('s1')
    })
    expect(onSegmentDeleted).toHaveBeenCalledTimes(deletedCalls)
    if (deletedCalls === 1) {
      expect(onSegmentDeleted).toHaveBeenCalledWith('s1', 42)
    }
  })
})
