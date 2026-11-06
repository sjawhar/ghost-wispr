import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import SessionCard from '../SessionCard.svelte'
import sessionCardSource from '../SessionCard.svelte?raw'
import {
  RefinementStatus,
  SessionStatus,
  SummaryStatus,
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
}

describe('SessionCard', () => {
  it('shows summary status', () => {
    render(SessionCard, {
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

  it('loads details when opened', async () => {
    const onToggle = vi.fn()
    const onLoadDetail = vi.fn().mockResolvedValue(undefined)
    render(SessionCard, {
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
    render(SessionCard, {
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

  it('says a session with no saved recording cannot be refined, and offers no retry', () => {
    render(SessionCard, {
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

  it('marks a session whose audio was removed to free space, refined or not', () => {
    for (const refinementStatus of [RefinementStatus.Completed, RefinementStatus.NoAudio]) {
      const { unmount } = render(SessionCard, {
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
})
