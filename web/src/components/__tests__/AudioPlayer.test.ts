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

  it('seeks to the clicked transcript line and applies seeking class during seek', async () => {
    const segments = [
      {
        speaker: 0,
        text: 'Hello',
        start_time: 0,
        end_time: 30,
        timestamp: '2026-01-01T00:00:00Z',
      },
      {
        speaker: 0,
        text: 'Far segment',
        start_time: 300,
        end_time: 330,
        timestamp: '2026-01-01T00:05:00Z',
      },
    ]

    const { container } = render(AudioPlayer, { sessionId: 's1', segments })
    const audio = container.querySelector('audio') as HTMLAudioElement
    Object.defineProperty(audio, 'duration', { value: 600, writable: true })
    await audio.dispatchEvent(new Event('loadedmetadata'))

    const lines = container.querySelectorAll('.line')
    await fireEvent.click(lines[1])

    expect(audio.currentTime).toBe(300)
    expect(container.querySelector('.audio-player')?.classList.contains('seeking')).toBe(true)

    await audio.dispatchEvent(new Event('seeked'))

    expect(container.querySelector('.audio-player')?.classList.contains('seeking')).toBe(false)
  })

  it('does not let timeupdate overwrite currentTime while seeking', async () => {
    const segments = [
      {
        speaker: 0,
        text: 'Hello',
        start_time: 0,
        end_time: 30,
        timestamp: '2026-01-01T00:00:00Z',
      },
      {
        speaker: 0,
        text: 'Far segment',
        start_time: 300,
        end_time: 330,
        timestamp: '2026-01-01T00:05:00Z',
      },
    ]

    const { container } = render(AudioPlayer, { sessionId: 's1', segments })
    const audio = container.querySelector('audio') as HTMLAudioElement
    Object.defineProperty(audio, 'duration', { value: 600, writable: true })
    Object.defineProperty(audio, 'currentTime', { value: 120, writable: true, configurable: true })

    await audio.dispatchEvent(new Event('loadedmetadata'))
    await audio.dispatchEvent(new Event('timeupdate'))
    expect(container.querySelector('.audio-time')?.textContent).toContain('02:00 / 10:00')

    const lines = container.querySelectorAll('.line')
    await fireEvent.click(lines[1])

    audio.currentTime = 0
    await audio.dispatchEvent(new Event('timeupdate'))
    expect(container.querySelector('.audio-time')?.textContent).toContain('02:00 / 10:00')

    audio.currentTime = 300
    await audio.dispatchEvent(new Event('seeked'))
    expect(container.querySelector('.audio-time')?.textContent).toContain('05:00 / 10:00')
  })
})
