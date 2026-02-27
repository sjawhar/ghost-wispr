import { fireEvent, render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import Controls from '../Controls.svelte'

describe('Controls', () => {
  it('renders connection and listening status', () => {
    render(Controls, { connected: true, paused: false, onToggle: vi.fn() })

    expect(screen.getByText('Connected')).toBeTruthy()
    expect(screen.getByText('Listening')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Pause' })).toBeTruthy()
  })

  it('calls toggle callback on click', async () => {
    const onToggle = vi.fn().mockResolvedValue(undefined)
    render(Controls, { connected: true, paused: true, onToggle })

    await fireEvent.click(screen.getByRole('button', { name: 'Resume' }))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })
})
