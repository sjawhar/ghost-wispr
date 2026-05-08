import { fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AudioPlayer from '../AudioPlayer.svelte'
import { appState, resetState } from '../../lib/state.svelte'
import { RefinementStatus, SummaryStatus, type SessionSummary } from '../../lib/types'

const writeText = vi.fn().mockResolvedValue(undefined)

function makeSession(overrides: Partial<SessionSummary> = {}): SessionSummary {
  return {
    id: 's1',
    title: 'Test Session',
    started_at: '2026-04-25T00:00:00Z',
    status: 'ended',
    summary: '',
    summary_status: SummaryStatus.Completed,
    summary_preset: 'standup',
    audio_path: '/tmp/audio.mp3',
    ...overrides,
  }
}

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
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    writeText.mockClear()
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

  it('shows refined transcript and view toggle when refinement completed', () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'raw streaming',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      session: makeSession({
        refinement_status: RefinementStatus.Completed,
        refined_transcript: 'Polished refined transcript text.',
        canonical_transcript: 'Polished refined transcript text.',
        transcript_source: 'refined',
      }),
    })

    expect(screen.getByTestId('transcript-refined').textContent).toContain(
      'Polished refined transcript text.',
    )
    expect(screen.getByTestId('transcript-view-toggle')).toBeTruthy()
    // Streaming segment should NOT be visible by default
    expect(screen.queryByText('raw streaming')).toBeNull()
  })

  it('switches to segments view when toggle clicked', async () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'raw streaming',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      session: makeSession({
        refinement_status: RefinementStatus.Completed,
        refined_transcript: 'Polished refined transcript text.',
        transcript_source: 'refined',
      }),
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Segments' }))
    expect(screen.getByText('raw streaming')).toBeTruthy()
    expect(screen.queryByTestId('transcript-refined')).toBeNull()
  })

  it('falls back to segments view when refinement not completed', () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'raw streaming',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      session: makeSession({
        refinement_status: RefinementStatus.Pending,
      }),
    })

    expect(screen.getByText('raw streaming')).toBeTruthy()
    expect(screen.queryByTestId('transcript-view-toggle')).toBeNull()
    expect(screen.getByTestId('refining-indicator')).toBeTruthy()
  })

  it('copies refined transcript when refined view active', async () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'raw streaming',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      session: makeSession({
        refinement_status: RefinementStatus.Completed,
        refined_transcript: 'Polished refined transcript text.',
        transcript_source: 'refined',
      }),
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Copy transcript' }))
    expect(writeText).toHaveBeenCalledWith('Polished refined transcript text.')
  })

  it('copies segment-formatted transcript when in segments view', async () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'raw streaming',
          start_time: 5,
          end_time: 8,
          timestamp: new Date().toISOString(),
        },
      ],
      session: makeSession({
        refinement_status: RefinementStatus.Completed,
        refined_transcript: 'Polished refined transcript text.',
        transcript_source: 'refined',
      }),
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Segments' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Copy transcript' }))
    expect(writeText).toHaveBeenCalledWith('[00:05] Speaker 0: raw streaming')
  })

  it('uses canonical_transcript when source is refined and refined_transcript empty', () => {
    render(AudioPlayer, {
      sessionId: 's1',
      segments: [],
      session: makeSession({
        refinement_status: RefinementStatus.Completed,
        refined_transcript: '',
        canonical_transcript: 'Canonical refined output.',
        transcript_source: 'refined',
      }),
    })

    expect(screen.getByTestId('transcript-refined').textContent).toContain(
      'Canonical refined output.',
    )
  })
})
