import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import Controls from '../Controls.svelte'

describe('Controls', () => {
  it('renders connection and listening status', () => {
    render(Controls, {
      connected: true,
      paused: false,
      activeSessionId: '',
      onToggle: vi.fn(),
      onEndSession: vi.fn(),
    })

    expect(screen.getByText('Connected')).toBeTruthy()
    expect(screen.getByText('Listening')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Pause' })).toBeTruthy()
  })

  it('calls toggle callback on click', async () => {
    const onToggle = vi.fn().mockResolvedValue(undefined)
    render(Controls, {
      connected: true,
      paused: true,
      activeSessionId: '',
      onToggle,
      onEndSession: vi.fn(),
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Resume' }))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('shows End Session button when session is active', () => {
    render(Controls, {
      connected: true,
      paused: false,
      activeSessionId: 'ses_123',
      onToggle: vi.fn(),
      onEndSession: vi.fn(),
    })

    expect(screen.getByRole('button', { name: 'End Session' })).toBeTruthy()
  })

  it('hides End Session button when no active session', () => {
    render(Controls, {
      connected: true,
      paused: false,
      activeSessionId: '',
      onToggle: vi.fn(),
      onEndSession: vi.fn(),
    })

    expect(screen.queryByRole('button', { name: 'End Session' })).toBeNull()
  })
})
