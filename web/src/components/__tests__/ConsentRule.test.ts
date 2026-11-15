import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import AudioPlayer from '../AudioPlayer.svelte'
import ConsentRule from '../ConsentRule.svelte'
import { RefinementStatus, SummaryStatus } from '../../lib/types'

const consentRule =
  'Record only while the light is on. Anyone may pause it at any time without a reason. Transcripts are readable by Theorem staff only. A person can ask for a segment to be deleted.'

const howTo =
  'Tap the button on the recorder to pause or resume. To have a segment’s text and audio removed, open the conversation once it has been refined, choose Delete on that segment, then choose Delete text and audio.'

async function openRules(): Promise<HTMLElement> {
  render(ConsentRule)
  await fireEvent.click(screen.getByRole('button', { name: 'Recording rules' }))
  return screen.getByRole('dialog', { name: 'Recording rules' })
}

describe('ConsentRule', () => {
  afterEach(() => {
    cleanup()
  })

  it('opens the recording rules and closes them with Escape', async () => {
    render(ConsentRule)

    expect(screen.getByRole('button', { name: 'Recording rules' })).toBeTruthy()
    expect(screen.queryByRole('dialog', { name: 'Recording rules' })).toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'Recording rules' }))

    const dialog = screen.getByRole('dialog', { name: 'Recording rules' })
    expect(within(dialog).getByText(consentRule)).toBeTruthy()
    expect(within(dialog).getByText(howTo)).toBeTruthy()

    await fireEvent.keyDown(window, { key: 'Escape' })

    expect(screen.queryByRole('dialog', { name: 'Recording rules' })).toBeNull()
  })

  // The how-to must name controls the refined conversation view has: a Delete
  // button on each segment, then the "Delete text and audio" action. It failed
  // when it still said to switch to segments, a toggle the page no longer has.
  it('names the controls a refined conversation offers for deleting a segment', async () => {
    const dialog = await openRules()
    const text = (within(dialog).getByText(/segment’s text and audio removed/).textContent ?? '')
      .replace(/\s+/g, ' ')
      .trim()
    cleanup()

    render(AudioPlayer, {
      sessionId: 's1',
      onDeleted: vi.fn(),
      onNotFound: vi.fn().mockResolvedValue(false),
      session: {
        id: 's1',
        title: 'Refined conversation',
        started_at: '2026-11-15T00:00:00Z',
        status: 'ended',
        summary: '',
        summary_status: SummaryStatus.Completed,
        summary_preset: 'standup',
        audio_path: '/tmp/audio.mp3',
        refinement_status: RefinementStatus.Completed,
        refined_revision: 1,
      },
      segments: [
        {
          id: 7,
          speaker: 0,
          text: 'A refined segment.',
          start_time: 0,
          end_time: 2,
          timestamp: '2026-11-15T00:00:01Z',
          transcript_revision: 1,
        },
      ],
    })
    expect(screen.queryByTestId('transcript-view-toggle')).toBeNull()
    expect(text).not.toMatch(/switch to segments/i)

    const deleteButton = screen.getByRole('button', { name: 'Delete segment' })
    expect(deleteButton.textContent?.trim()).toBe('Delete')
    expect(text).toContain('choose Delete on that segment')

    await fireEvent.click(deleteButton)
    expect(screen.getByRole('button', { name: 'Delete text and audio' })).toBeTruthy()
    expect(text).toContain('choose Delete text and audio')
  })
})
