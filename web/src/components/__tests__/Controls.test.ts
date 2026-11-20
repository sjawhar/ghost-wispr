import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import Controls from '../Controls.svelte'

function props(overrides: Record<string, unknown> = {}) {
  return {
    connected: true,
    paused: false,
    capturing: true,
    captureState: 'capturing',
    activeSessionId: '',
    onToggle: vi.fn(),
    onClose: vi.fn(),
    onEndSession: vi.fn(),
    onStartSession: vi.fn(),
    onStopSession: vi.fn(),
    ...overrides,
  }
}

describe('Controls', () => {
  it('renders connection and recording status', () => {
    render(Controls, props())

    expect(screen.getByText('Connected')).toBeTruthy()
    expect(screen.getByText('Recording')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Pause' })).toBeTruthy()
  })

  // The three states (INFRA-4592 R1): each shows what the microphone does, and
  // Close microphone closes it from recording and from listening.
  it('shows each of the three states, and closes the microphone from recording and listening', async () => {
    for (const [state, label, closable] of [
      [{ paused: false, capturing: true, captureState: 'capturing' }, 'Recording', true],
      [
        { paused: true, capturing: false, captureState: 'listening' },
        'Paused: hears only “Wispr, start recording”',
        true,
      ],
      [{ paused: true, capturing: false, captureState: 'paused' }, 'Microphone closed', false],
    ] as const) {
      const onClose = vi.fn().mockResolvedValue(undefined)
      const { unmount } = render(Controls, props({ ...state, onClose }))
      expect(screen.getByText(label)).toBeTruthy()
      const close = screen.queryByRole('button', { name: 'Close microphone' })
      expect(close !== null).toBe(closable)
      if (close) {
        await fireEvent.click(close)
        expect(onClose).toHaveBeenCalledTimes(1)
      }
      unmount()
    }
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
    render(
      Controls,
      props({ paused: true, capturing: false, captureState: 'paused', onStartSession }),
    )

    expect(screen.getByText('Microphone closed')).toBeTruthy()
    const record = screen.getByTestId('record-toggle') as HTMLButtonElement
    expect(record.disabled).toBe(true)
    expect(record.title).toMatch(/Resume/)
    await fireEvent.keyDown(window, { key: 'R', ctrlKey: true, shiftKey: true })
    expect(onStartSession).not.toHaveBeenCalled()
  })

  // After a resume the switch is on before the microphone is back: the label
  // must not claim it records, and Record must wait for capture.
  it('shows Not capturing and disables Record until the microphone is capturing', async () => {
    const onStartSession = vi.fn().mockResolvedValue(undefined)
    render(Controls, props({ paused: false, capturing: false, onStartSession }))

    expect(screen.queryByText('Recording')).toBeNull()
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
