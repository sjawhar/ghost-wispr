import { fireEvent, render as renderSvelte, screen, waitFor } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
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

type AudioPlayerProps = ComponentProps<typeof AudioPlayer>

function render(
  props: Omit<AudioPlayerProps, 'onDeleted' | 'onNotFound'> &
    Partial<Pick<AudioPlayerProps, 'onDeleted' | 'onNotFound'>>,
) {
  return renderSvelte(AudioPlayer, {
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(undefined),
    ...props,
  })
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
    vi.unstubAllGlobals()
    resetState()
  })

  it('renders audio controls', () => {
    render({ sessionId: 's1', segments: [] })
    expect(screen.getByRole('button', { name: 'Play Audio' })).toBeTruthy()
  })

  it('sets active audio session when transcript line is clicked', async () => {
    render({
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
    render({
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
    render({
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
    render({
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
    render({
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
    render({
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
    render({
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

  it('says no recording was saved instead of loading audio or showing refinement', () => {
    render({
      sessionId: 's1',
      segments: [],
      session: makeSession({ audio_path: '', refinement_status: RefinementStatus.NoAudio }),
    })

    expect(
      screen.getByText(
        'No recording was saved for this session, so it cannot be played or refined.',
      ),
    ).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Play Audio' })).toBeNull()
    expect(screen.queryByText('Loading audio...')).toBeNull()
    expect(screen.queryByTestId('refining-indicator')).toBeNull()
  })

  it('says the audio was removed to free space instead of loading it, and keeps the transcript', () => {
    render({
      sessionId: 's1',
      segments: [],
      session: makeSession({
        audio_path: '',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.Completed,
        refined_transcript: 'The refined words.',
        transcript_source: 'refined',
      }),
    })

    expect(
      screen.getByText('The audio was removed to free space. The transcript and summary are kept.'),
    ).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Play Audio' })).toBeNull()
    expect(screen.queryByText('Loading audio...')).toBeNull()
    expect(screen.getByTestId('transcript-refined').textContent).toContain('The refined words.')
  })

  it('says what is left when only part of the audio was removed, and still offers to play it', () => {
    render({
      sessionId: 's1',
      segments: [],
      session: makeSession({
        audio_path: 'data/audio/b.mp3',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.Pending,
      }),
    })

    expect(
      screen.getByText('Part of the audio was removed to free space; 1 recording is left.'),
    ).toBeTruthy()
    expect(
      screen.queryByText(
        'The audio was removed to free space. The transcript and summary are kept.',
      ),
    ).toBeNull()
    expect(screen.getByRole('button', { name: 'Play Audio' })).toBeTruthy()
  })

  it('says the recording left was not found on disk when GC found none, as the card does', () => {
    render({
      sessionId: 's1',
      segments: [],
      session: makeSession({
        audio_path: 'mnt/b.mp3',
        audio_removed_at: '2026-11-07T09:30:00Z',
        refinement_status: RefinementStatus.NoAudio,
      }),
    })

    expect(
      screen.getByText(
        'Part of the audio was removed to free space; 1 recording is left, but it was not on disk when the rest was removed.',
      ),
    ).toBeTruthy()
  })

  it('deletes a confirmed stored segment and removes it from the transcript', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 204 }))
    const onDeleted = vi.fn()
    const confirm = vi.fn().mockReturnValue(true)
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('confirm', confirm)
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Delete this segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
        {
          id: 43,
          speaker: 1,
          text: 'Keep this segment',
          start_time: 2,
          end_time: 4,
          timestamp: new Date().toISOString(),
        },
      ],
      onDeleted,
    })

    await fireEvent.click(screen.getAllByRole('button', { name: 'Delete segment' })[0])

    await waitFor(() => {
      expect(screen.queryByText('Delete this segment')).toBeNull()
    })
    expect(confirm).toHaveBeenCalledTimes(1)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch).toHaveBeenCalledWith('/api/sessions/s1/segments/42', { method: 'DELETE' })
    expect(onDeleted).toHaveBeenCalledTimes(1)
    expect(onDeleted).toHaveBeenCalledWith(42)
    expect(screen.getByText('Keep this segment')).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('reloads audio from a cache-busting URL after a successful deletion', async () => {
    const pause = vi.fn()
    const load = vi.fn()
    Object.defineProperty(HTMLMediaElement.prototype, 'pause', {
      configurable: true,
      value: pause,
    })
    Object.defineProperty(HTMLMediaElement.prototype, 'load', {
      configurable: true,
      value: load,
    })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 204 })))
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))

    const { container } = render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Spliced segment',
          start_time: 0,
          end_time: 2,
          timestamp: '2026-11-11T10:00:00Z',
        },
      ],
    })
    const audio = container.querySelector('audio')

    expect(audio?.getAttribute('src')).toBe('/api/sessions/s1/audio?v=42')

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))

    await waitFor(() => {
      expect(audio?.getAttribute('src')).toBe('/api/sessions/s1/audio?v=')
    })
    expect(pause).toHaveBeenCalledExactlyOnceWith()
    expect(load).toHaveBeenCalledExactlyOnceWith()
  })

  it('disables the control and shows progress while the deletion is in flight', async () => {
    let finishRequest!: (response: Response) => void
    vi.stubGlobal(
      'fetch',
      vi.fn().mockReturnValue(new Promise<Response>((resolve) => (finishRequest = resolve))),
    )
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Slow segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
        {
          id: 43,
          speaker: 1,
          text: 'Other segment',
          start_time: 2,
          end_time: 4,
          timestamp: new Date().toISOString(),
        },
      ],
    })
    const deleteButtons = screen.getAllByRole('button', { name: 'Delete segment' })
    const deleteButton = deleteButtons[0]

    await fireEvent.click(deleteButton)

    await waitFor(() => {
      expect(deleteButton.textContent).toContain('Deleting…')
    })
    expect(deleteButtons.every((button) => button.hasAttribute('disabled'))).toBe(true)
    expect(screen.getByText('Slow segment')).toBeTruthy()
    expect(screen.getByText('Other segment')).toBeTruthy()

    finishRequest(new Response(null, { status: 204 }))

    await waitFor(() => {
      expect(screen.queryByText('Slow segment')).toBeNull()
    })
    expect(screen.getByText('Other segment')).toBeTruthy()
  })

  it('does not request deletion when the confirmation is cancelled', async () => {
    const fetch = vi.fn()
    const onDeleted = vi.fn()
    const confirm = vi.fn().mockReturnValue(false)
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('confirm', confirm)
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Do not delete this segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      onDeleted,
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))

    expect(confirm).toHaveBeenCalledTimes(1)
    expect(fetch).not.toHaveBeenCalled()
    expect(onDeleted).not.toHaveBeenCalled()
    expect(screen.getByText('Do not delete this segment')).toBeTruthy()
  })

  it.each([
    [
      'explains that an active conversation cannot delete a segment',
      409,
      'This conversation is still being recorded. Delete the segment after it ends.',
      'Active segment',
    ],
    [
      'shows the server message when a segment cannot be removed cleanly',
      422,
      'The segment could not be removed cleanly; nothing was deleted',
      'Unchanged segment',
    ],
  ])('%s', async (_, status, message, text) => {
    const onDeleted = vi.fn()
    const onNotFound = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: message }), { status })),
    )
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text,
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      onDeleted,
      onNotFound,
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))

    expect((await screen.findByRole('alert')).textContent).toBe(message)
    expect(screen.getByText(text)).toBeTruthy()
    expect(onDeleted).not.toHaveBeenCalled()
    expect(onNotFound).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Delete segment' }).hasAttribute('disabled')).toBe(
      false,
    )
  })

  it('keeps the not-found message and refreshes stale session state', async () => {
    const onDeleted = vi.fn()
    const onNotFound = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 404 })))
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Stale segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      onDeleted,
      onNotFound,
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))

    await waitFor(() => {
      expect(onNotFound).toHaveBeenCalledTimes(1)
    })
    expect(onDeleted).not.toHaveBeenCalled()
    expect((await screen.findByRole('alert')).textContent).toBe('request failed: 404')
    expect(screen.getByText('Stale segment')).toBeTruthy()
  })

  it.each([
    [
      'a 500 response',
      () => new Response(JSON.stringify({ error: 'rename failed' }), { status: 500 }),
      'rename failed',
    ],
    [
      'a network error',
      () => Promise.reject(new Error('connection dropped')),
      'connection dropped',
    ],
  ])('keeps the message and refreshes after %s', async (_, response, message) => {
    const onDeleted = vi.fn()
    const onNotFound = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('fetch', vi.fn().mockImplementation(response))
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Possibly deleted segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      onDeleted,
      onNotFound,
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))

    await waitFor(() => {
      expect(onNotFound).toHaveBeenCalledTimes(1)
    })
    expect(onDeleted).not.toHaveBeenCalled()
    expect((await screen.findByRole('alert')).textContent).toBe(message)
    expect(screen.getByText('Possibly deleted segment')).toBeTruthy()
  })

  it('keeps segments open without reloading audio after an uncertain deletion refresh', async () => {
    const pause = vi.fn()
    const load = vi.fn()
    Object.defineProperty(HTMLMediaElement.prototype, 'pause', {
      configurable: true,
      value: pause,
    })
    Object.defineProperty(HTMLMediaElement.prototype, 'load', {
      configurable: true,
      value: load,
    })
    const session = makeSession({
      refinement_status: RefinementStatus.Completed,
      refined_transcript: 'Refined transcript with the stored segment.',
      transcript_source: 'refined',
    })
    const props = {
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Possibly deleted segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      session,
    }
    let rerender: (props: AudioPlayerProps) => void = () => {}
    const onDeleted = vi.fn()
    const onNotFound = vi.fn(async () => {
      rerender({ ...props, session: { ...session }, onDeleted, onNotFound })
    })
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValue(
          new Response(JSON.stringify({ error: 'rename failed' }), { status: 500 }),
        ),
    )
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true))
    const result = render({ ...props, onDeleted, onNotFound })
    rerender = result.rerender

    await fireEvent.click(screen.getByRole('button', { name: 'Segments' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Delete segment' }))

    await waitFor(() => {
      expect(onNotFound).toHaveBeenCalledTimes(1)
    })
    expect(screen.getByRole('button', { name: 'Delete segment' })).toBeTruthy()
    expect(pause).not.toHaveBeenCalled()
    expect(load).not.toHaveBeenCalled()
  })
  it('keeps the refined transcript as the default and exposes deletion in segments', async () => {
    render({
      sessionId: 's1',
      segments: [
        {
          id: 42,
          speaker: 0,
          text: 'Stored segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
      session: makeSession({
        refinement_status: RefinementStatus.Completed,
        refined_transcript: 'Refined transcript with the stored segment.',
        transcript_source: 'refined',
      }),
    })

    expect(screen.getByTestId('transcript-refined')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Delete segment' })).toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'Segments' }))

    expect(screen.getByRole('button', { name: 'Delete segment' })).toBeTruthy()
  })
  it('does not offer deletion for a segment that is not stored yet', () => {
    render({
      sessionId: 's1',
      segments: [
        {
          speaker: 0,
          text: 'Live segment',
          start_time: 0,
          end_time: 2,
          timestamp: new Date().toISOString(),
        },
      ],
    })

    expect(screen.queryByRole('button', { name: 'Delete segment' })).toBeNull()
  })
})
