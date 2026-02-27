import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import SessionList from '../SessionList.svelte'

describe('SessionList', () => {
  it('renders empty state', () => {
    render(SessionList, {
      dates: [],
      sessionsByDate: new Map(),
      sessionDetails: new Map(),
      presets: {},
      expandedSessionId: '',
      onToggleSession: vi.fn(),
      onLoadDate: vi.fn().mockResolvedValue(undefined),
      onLoadDetail: vi.fn().mockResolvedValue(undefined),
      onResummarize: vi.fn().mockResolvedValue(undefined),
    })

    expect(screen.getByText('No sessions yet.')).toBeTruthy()
  })

  it('loads previous dates on button click', async () => {
    const dates = ['2026-02-26', '2026-02-25', '2026-02-24', '2026-02-23']
    render(SessionList, {
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
    })

    const button = screen.getByRole('button', { name: 'Load previous' })
    await fireEvent.click(button)
    expect(screen.queryByRole('button', { name: 'Load previous' })).toBeNull()
  })
})
