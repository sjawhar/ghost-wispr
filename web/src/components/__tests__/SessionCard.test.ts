import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import SessionCard from '../SessionCard.svelte'

const baseSession = {
  id: 's1',
  started_at: new Date('2026-02-26T10:00:00Z').toISOString(),
  ended_at: new Date('2026-02-26T10:10:00Z').toISOString(),
  status: 'ended',
  summary: '',
  summary_status: 'pending' as const,
  audio_path: 'data/audio/s1.mp3',
}

describe('SessionCard', () => {
  it('shows summary status', () => {
    render(SessionCard, {
      session: baseSession,
      detail: undefined,
      expanded: false,
      onToggle: vi.fn(),
      onLoadDetail: vi.fn(),
    })

    expect(screen.getByText('pending')).toBeTruthy()
    expect(screen.getByText('Summarizing...')).toBeTruthy()
  })

  it('loads details when opened', async () => {
    const onToggle = vi.fn()
    const onLoadDetail = vi.fn().mockResolvedValue(undefined)
    render(SessionCard, {
      session: baseSession,
      detail: undefined,
      expanded: false,
      onToggle,
      onLoadDetail,
    })

    await fireEvent.click(screen.getByRole('button'))
    expect(onToggle).toHaveBeenCalledTimes(1)
    expect(onLoadDetail).toHaveBeenCalledWith('s1')
  })
})
