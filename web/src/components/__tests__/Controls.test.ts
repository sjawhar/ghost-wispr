import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import Controls from '../Controls.svelte'

function props(overrides: Record<string, unknown> = {}) {
  return {
    connected: true,
    paused: false,
    capturing: true,
    activeSessionId: '',
    onToggle: vi.fn(),
    onEndSession: vi.fn(),
    onStartSession: vi.fn(),
    onStopSession: vi.fn(),
    ...overrides,
  }
}

describe('Controls', () => {
  it('renders connection and listening status', () => {
    render(Controls, props())

    expect(screen.getByText('Connected')).toBeTruthy()
    expect(screen.getByText('Listening')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Pause' })).toBeTruthy()
  })

  it('calls toggle callback on click', async () => {
    const onToggle = vi.fn().mockResolvedValue(undefined)
    render(Controls, props({ paused: true, capturing: false, onToggle }))

    await fireEvent.click(screen.getByRole('button', { name: 'Resume' }))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('shows End Session button when session is active', () => {
    render(Controls, props({ activeSessionId: 'ses_123' }))

    expect(screen.getByRole('button', { name: 'End Session' })).toBeTruthy()
  })

  it('hides End Session button when no active session', () => {
    render(Controls, props())

    expect(screen.queryByRole('button', { name: 'End Session' })).toBeNull()
  })

  it('disables Record while capture is paused', async () => {
    const onStartSession = vi.fn().mockResolvedValue(undefined)
    render(Controls, props({ paused: true, capturing: false, onStartSession }))

    expect(screen.getByText('Paused')).toBeTruthy()
    const record = screen.getByTestId('record-toggle') as HTMLButtonElement
    expect(record.disabled).toBe(true)
    expect(record.title).toMatch(/Resume/)
    await fireEvent.keyDown(window, { key: 'R', ctrlKey: true, shiftKey: true })
    expect(onStartSession).not.toHaveBeenCalled()
  })

  // After a resume the switch is on before the microphone is back: the label
  // must not claim it is listening, and Record must wait for capture.
  it('shows Not capturing and disables Record until the microphone is capturing', async () => {
    const onStartSession = vi.fn().mockResolvedValue(undefined)
    render(Controls, props({ paused: false, capturing: false, onStartSession }))

    expect(screen.queryByText('Listening')).toBeNull()
    expect(screen.getByText('Not capturing')).toBeTruthy()
    const record = screen.getByTestId('record-toggle') as HTMLButtonElement
    expect(record.disabled).toBe(true)
    await fireEvent.keyDown(window, { key: 'R', ctrlKey: true, shiftKey: true })
    expect(onStartSession).not.toHaveBeenCalled()
  })

  it('shows why starting a session failed', async () => {
    const onStartSession = vi
      .fn()
      .mockRejectedValue(
        new Error('{"error":"capture is paused; resume before starting a session"}'),
      )
    render(Controls, props({ onStartSession }))

    await fireEvent.click(screen.getByTestId('record-toggle'))

    expect((await screen.findByRole('alert')).textContent).toContain(
      'capture is paused; resume before starting a session',
    )
  })
})
