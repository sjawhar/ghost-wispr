import { render, screen } from '@testing-library/svelte'
import { describe, expect, it } from 'vitest'
import LivePanel from '../LivePanel.svelte'

describe('LivePanel', () => {
  it('shows listening state when empty', () => {
    render(LivePanel, {
      segments: [],
      connected: true,
      activeSessionStartedAt: 0,
      interimText: '',
      interimSpeaker: -1,
    })
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
      interimText: '',
      interimSpeaker: -1,
    })

    expect(screen.getByText('Speaker 2')).toBeTruthy()
    expect(screen.getByText('Ship it')).toBeTruthy()
  })

  it('shows interim text when provided', () => {
    render(LivePanel, {
      segments: [],
      connected: true,
      activeSessionStartedAt: Date.now(),
      interimText: 'being transcribed now',
      interimSpeaker: 0,
    })
    expect(screen.getByText('being transcribed now')).toBeTruthy()
    expect(screen.getByText('Speaker 0')).toBeTruthy()
  })

  it('shows ellipsis for unknown speaker in interim', () => {
    render(LivePanel, {
      segments: [],
      connected: true,
      activeSessionStartedAt: Date.now(),
      interimText: 'some speech',
      interimSpeaker: -1,
    })
    expect(screen.getByText('...')).toBeTruthy()
  })
})
