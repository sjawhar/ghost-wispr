import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import IntegrationsSettings from '../IntegrationsSettings.svelte'
import PresetEditor from '../PresetEditor.svelte'
import { resetConfigState } from '../../lib/config.svelte'
import { SessionStatus, SummaryStatus, type SessionSummary } from '../../lib/types'
import { stubFetch, unitConfig } from './configFixture'

// orphanLabels lists the text of each label under root that names no form
// control: one whose for names no element, or names one that is not a control,
// and that wraps none. A screen reader reads such a label as belonging to
// nothing, and a click on it moves focus nowhere.
function orphanLabels(root: HTMLElement): string[] {
  return [...root.querySelectorAll('label')]
    .filter((label) => label.control === null)
    .map((label) => label.textContent?.trim() ?? '')
}

const ended: SessionSummary = {
  id: 's1',
  title: '',
  started_at: '2026-11-11T10:00:00Z',
  ended_at: '2026-11-11T10:30:00Z',
  status: SessionStatus.Ended,
  summary: 'A stand-up.',
  summary_status: SummaryStatus.Completed,
  summary_preset: 'default',
  audio_path: '',
}

// stubSessions answers the Test dialog's reads: one date with the sessions
// given.
function stubSessions(sessions: SessionSummary[]): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const answer = String(input) === '/api/dates' ? ['2026-11-11'] : sessions
      return Promise.resolve(new Response(JSON.stringify(answer)))
    }),
  )
}

async function openTestDialog(): Promise<HTMLElement> {
  render(PresetEditor, {
    presets: {
      standup: {
        description: 'Stand-up',
        system_prompt: 'Summarize briefly',
        user_template: '{{transcript}}',
        model: '',
      },
    },
  })
  await fireEvent.click(screen.getByRole('button', { name: /standup/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Test' }))
  return screen.getByRole('dialog')
}

describe('Form labels', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('in Integrations each name a control, and the credentials status is not one', () => {
    stubFetch()
    const { container } = render(IntegrationsSettings, { config: unitConfig })

    expect(orphanLabels(container)).toEqual([])
    expect(screen.getByText('Google Credentials')).toBeTruthy()
  })

  it('in the Test dialog name the session picker, and nothing while there is none', async () => {
    stubSessions([])
    const dialog = await openTestDialog()

    expect(await within(dialog).findByText('No sessions found')).toBeTruthy()
    expect(within(dialog).getByText('Select Session')).toBeTruthy()
    expect(orphanLabels(dialog)).toEqual([])
  })

  it('in the Test dialog name the session picker once there are sessions', async () => {
    stubSessions([ended])
    const dialog = await openTestDialog()

    const picker = await within(dialog).findByLabelText('Select Session')
    expect(picker.tagName).toBe('SELECT')
    expect(orphanLabels(dialog)).toEqual([])
  })
})
