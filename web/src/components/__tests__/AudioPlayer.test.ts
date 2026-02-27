import { fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AudioPlayer from '../AudioPlayer.svelte'
import { appState, resetState } from '../../lib/state.svelte'

describe('AudioPlayer', () => {
  beforeEach(() => {
    Object.defineProperty(HTMLMediaElement.prototype, 'play', {
      configurable: true,
      value: vi.fn().mockResolvedValue(undefined),
    })
    Object.defineProperty(HTMLMediaElement.prototype, 'pause', {
      configurable: true,
      value: vi.fn(),
    })
  })

  afterEach(() => {
    resetState()
  })

  it('renders audio controls', () => {
    render(AudioPlayer, { sessionId: 's1', segments: [] })
    expect(screen.getByRole('button', { name: 'Play Audio' })).toBeTruthy()
  })

  it('sets active audio session when transcript line is clicked', async () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'Hello',
          start_time: 5,
          end_time: 8,
          timestamp: new Date().toISOString(),
        },
      ],
    })

    await fireEvent.click(screen.getByRole('button', { name: /Hello/i }))
    expect(appState.activeAudioSessionId).toBe('s1')
  })
})
