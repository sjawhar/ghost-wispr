import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import AudioPlayer from '../AudioPlayer.svelte'
import { resetState } from '../../lib/state.svelte'
import { RefinementStatus, SummaryStatus, SyncState, SyncStatus } from '../../lib/types'

const segments = [
  {
    id: 42,
    speaker: 0,
    text: 'Orange tractor ninety',
    start_time: 0,
    end_time: 2,
    timestamp: '2026-11-12T10:00:00Z',
    transcript_revision: 1,
  },
  {
    id: 43,
    speaker: 1,
    text: 'Keep this segment',
    start_time: 2,
    end_time: 4,
    timestamp: '2026-11-12T10:00:02Z',
    transcript_revision: 1,
  },
]

function renderPlayer() {
  return render(AudioPlayer, {
    sessionId: 's1',
    segments,
    session: {
      id: 's1',
      title: '',
      started_at: '2026-11-12T10:00:00Z',
      status: 'ended',
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: '',
      audio_path: 'recording.mp3',
      refinement_status: RefinementStatus.Completed,
      refined_revision: 1,
    },
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(false),
  })
}

it('disables Delete until timed refined segments exist', () => {
  render(AudioPlayer, {
    sessionId: 's1',
    segments,
    session: {
      id: 's1',
      title: '',
      started_at: '2026-11-12T10:00:00Z',
      status: 'ended',
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: '',
      audio_path: 'recording.mp3',
      refinement_status: RefinementStatus.Pending,
    },
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(false),
  })
  for (const button of screen.getAllByRole('button', { name: 'Delete segment' })) {
    expect(button.hasAttribute('disabled')).toBe(true)
  }
  expect(
    screen.getAllByText('This conversation is still being refined; delete once it finishes.'),
  ).toHaveLength(2)
})

it('says a completed conversation without timed words needs refinement before deletion', () => {
  render(AudioPlayer, {
    sessionId: 's1',
    segments,
    session: {
      id: 's1',
      title: '',
      started_at: '2026-11-12T10:00:00Z',
      status: 'ended',
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: '',
      audio_path: 'recording.mp3',
      refinement_status: RefinementStatus.Completed,
      refined_revision: 0,
    },
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(false),
  })

  expect(
    screen.getAllByText(
      'This conversation completed without timed words. Use Retry refinement before deleting.',
    ),
  ).toHaveLength(2)
})

it('identifies a Drive-restored session as retryable before deletion', () => {
  render(AudioPlayer, {
    sessionId: 's1',
    segments,
    session: {
      id: 's1',
      title: '',
      started_at: '2026-11-12T10:00:00Z',
      status: 'ended',
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: '',
      audio_path: '',
      audio_removed_at: '2026-11-13T09:01:00Z',
      restored_at: '2026-11-13T09:01:00Z',
      refinement_status: RefinementStatus.NoAudio,
      gdrive_folder_id: 'folder-1',
      sync_status: SyncStatus.Synced,
      sync_state: SyncState.Synced,
    },
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(false),
  })

  expect(
    screen.getAllByText(
      'This conversation was restored from Drive; retry refinement to delete from it',
    ),
  ).toHaveLength(3)
})

it('does not offer a refinement retry when a restored session cannot restore audio', () => {
  render(AudioPlayer, {
    sessionId: 's1',
    segments,
    session: {
      id: 's1',
      title: '',
      started_at: '2026-11-12T10:00:00Z',
      status: 'ended',
      summary: '',
      summary_status: SummaryStatus.Pending,
      summary_preset: '',
      audio_path: '',
      audio_removed_at: '2026-11-13T09:01:00Z',
      restored_at: '2026-11-13T09:01:00Z',
      refinement_status: RefinementStatus.NoAudio,
    },
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(false),
  })

  expect(screen.queryByText(/retry refinement to delete from it/)).toBeNull()
})

describe('AudioPlayer segment deletion actions', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetState()
  })

  it('offers "Delete text" and "Delete text and audio", each with one sentence on what it does', async () => {
    renderPlayer()

    expect(screen.queryByRole('button', { name: 'Delete text' })).toBeNull()
    await fireEvent.click(screen.getAllByRole('button', { name: 'Delete segment' })[0])

    const actions = screen.getByRole('group', { name: 'Delete options' })
    const text = within(actions).getByRole('button', { name: 'Delete text' })
    const textAndAudio = within(actions).getByRole('button', { name: 'Delete text and audio' })
    expect(within(actions).getAllByRole('button')).toHaveLength(2)
    for (const [button, says] of [
      [text, /recording .*stay/i],
      [textAndAudio, /cuts .*recording/i],
    ] as const) {
      const note = document.getElementById(button.getAttribute('aria-describedby') ?? '')
      expect(note?.textContent).toMatch(says)
      expect(
        note?.textContent
          ?.trim()
          .split(/[.!?](\s|$)/)
          .filter((s) => s.trim()).length,
      ).toBe(1)
      expect(actions.contains(note)).toBe(true)
    }
  })

  it.each([
    ['Delete text', 'text'],
    ['Delete text and audio', 'text_and_audio'],
  ])('%s sends mode=%s for the chosen segment', async (action, mode) => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    renderPlayer()

    await fireEvent.click(screen.getAllByRole('button', { name: 'Delete segment' })[0])
    await fireEvent.click(screen.getByRole('button', { name: action }))

    await waitFor(() => {
      expect(fetch).toHaveBeenCalledExactlyOnceWith(
        `/api/sessions/s1/segments/42?mode=${mode}&revision=1`,
        {
          method: 'DELETE',
        },
      )
    })
    await waitFor(() => {
      expect(screen.queryByText('Orange tractor ninety')).toBeNull()
    })
    expect(screen.getByText('Keep this segment')).toBeTruthy()
  })

  it('asks before each action, naming what it removes', async () => {
    const fetch = vi.fn()
    const confirm = vi.fn().mockReturnValue(false)
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('confirm', confirm)
    renderPlayer()

    await fireEvent.click(screen.getAllByRole('button', { name: 'Delete segment' })[0])
    await fireEvent.click(screen.getByRole('button', { name: 'Delete text' }))
    await fireEvent.click(screen.getAllByRole('button', { name: 'Delete segment' })[0])
    await fireEvent.click(screen.getByRole('button', { name: 'Delete text and audio' }))

    expect(confirm.mock.calls.map(([question]) => question)).toEqual([
      "Delete this segment's text? This cannot be undone.",
      "Delete this segment's text and audio? This cannot be undone.",
    ])
    expect(fetch).not.toHaveBeenCalled()
  })

  it('closes its delete menu on Escape or a click outside it', async () => {
    renderPlayer()

    const opener = screen.getAllByRole('button', { name: 'Delete segment' })[0]
    await fireEvent.click(opener)
    expect(screen.getByRole('group', { name: 'Delete options' })).toBeTruthy()

    await fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.queryByRole('group', { name: 'Delete options' })).toBeNull()
    expect(screen.getByText('Orange tractor ninety')).toBeTruthy()

    await fireEvent.click(opener)
    expect(screen.getByRole('group', { name: 'Delete options' })).toBeTruthy()

    await fireEvent.click(document.body)
    expect(screen.queryByRole('group', { name: 'Delete options' })).toBeNull()
    expect(screen.getByText('Orange tractor ninety')).toBeTruthy()
  })
})
