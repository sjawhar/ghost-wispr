import { render, screen } from '@testing-library/svelte'
import { describe, expect, it } from 'vitest'
import LivePanel from '../LivePanel.svelte'

describe('LivePanel', () => {
  it('shows listening state when empty', () => {
    render(LivePanel, { segments: [], connected: true, activeSessionStartedAt: 0 })
    expect(screen.getByText('Listening...')).toBeTruthy()
  })

  it('renders transcript segments', () => {
    render(LivePanel, {
      segments: [
        {
          type: 'live_transcript',
          version: 1,
          timestamp: new Date().toISOString(),
          speaker: 2,
          text: 'Ship it',
          start_time: 0,
          end_time: 1,
        },
      ],
      connected: true,
      activeSessionStartedAt: Date.now(),
    })

    expect(screen.getByText('Speaker 2')).toBeTruthy()
    expect(screen.getByText('Ship it')).toBeTruthy()
  })
})
