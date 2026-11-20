import { render, screen, within } from '@testing-library/svelte'
import { describe, expect, it } from 'vitest'
import VoiceTasks from '../VoiceTasks.svelte'
import { VoiceTaskState, type VoiceTask } from '../../lib/types'

const answered: VoiceTask = {
  id: 'evt-1',
  issue_key: 'ABC-12',
  holder: 'session:agent-7',
  state: VoiceTaskState.Done,
  asked_at: '2026-11-19T09:30:00Z',
  spoken_at: '2026-11-19T09:32:00Z',
  answer_written: 'It merged an hour ago.\nThe deploy runs tonight.',
}

const waiting: VoiceTask = {
  id: 'evt-2',
  issue_key: 'ABC-13',
  holder: 'session:agent-8',
  state: VoiceTaskState.Working,
  asked_at: '2026-11-19T09:31:00Z',
  answer_written: '',
}

const section = () => screen.queryByRole('region', { name: 'Questions to agents' })

describe('VoiceTasks', () => {
  // Each question shows its issue, the agent it went to, where it stands,
  // when it was asked and spoken, and the agent's whole answer: it failed
  // while the session's page showed none of them.
  it('shows each question with its issue, holder, state, times and the full answer', () => {
    render(VoiceTasks, { voiceTasks: { tasks: [answered, waiting], error: '' } })

    const [first, second] = within(section()!).getAllByRole('listitem')
    expect(within(first).getByText('ABC-12')).toBeTruthy()
    expect(within(first).getByText('session:agent-7')).toBeTruthy()
    expect(within(first).getByText('answered')).toBeTruthy()
    expect(first.querySelector('time[datetime="2026-11-19T09:30:00Z"]')?.textContent).toBeTruthy()
    expect(first.querySelector('time[datetime="2026-11-19T09:32:00Z"]')?.textContent).toBeTruthy()
    expect(first.textContent).toMatch(/Asked .+, spoken .+/)
    expect(within(first).getByTestId('voice-task-answer').textContent).toBe(answered.answer_written)

    expect(within(second).getByText('ABC-13')).toBeTruthy()
    expect(within(second).getByText('waiting')).toBeTruthy()
    expect(second.textContent).toMatch(/Asked .+, not spoken/)
    expect(within(second).queryByTestId('voice-task-answer')).toBeNull()
    expect(within(second).getByText('No answer yet')).toBeTruthy()
  })

  it.each([
    [VoiceTaskState.Working, 'waiting'],
    [VoiceTaskState.Done, 'answered'],
    [VoiceTaskState.Cancelled, 'cancelled'],
    [VoiceTaskState.Replaced, 'replaced'],
  ])('says a %s question is %s', (state, words) => {
    render(VoiceTasks, { voiceTasks: { tasks: [{ ...answered, state }], error: '' } })

    expect(within(section()!).getByText(words)).toBeTruthy()
  })

  // A session the voice asked nothing in, or whose questions are not read
  // yet, shows no section.
  it.each([
    ['no questions', { tasks: [], error: '' }],
    ['questions not read yet', undefined],
  ])('is hidden for a session with %s', (_, voiceTasks) => {
    render(VoiceTasks, { voiceTasks })

    expect(section()).toBeNull()
    expect(screen.queryByText('Questions to agents')).toBeNull()
  })

  // The answer is an agent's text: markup in it is shown as the characters
  // it is, never made into elements.
  it('shows an answer that holds markup as text', () => {
    const markup = '<b>x</b> & <img src="x" alt="y">'
    const { container } = render(VoiceTasks, {
      voiceTasks: { tasks: [{ ...answered, answer_written: markup }], error: '' },
    })

    expect(screen.getByTestId('voice-task-answer').textContent).toBe(markup)
    expect(container.querySelector('b')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
  })

  // A read that failed says so, and does not pass for a session with no
  // questions: it keeps the questions an earlier read showed.
  it('says why the questions could not be read, and keeps those read before', () => {
    render(VoiceTasks, {
      voiceTasks: { tasks: [answered], error: 'list voice tasks: database is locked' },
    })

    expect(within(section()!).getByRole('alert').textContent).toContain(
      'list voice tasks: database is locked',
    )
    expect(within(section()!).getAllByRole('listitem')).toHaveLength(1)
  })

  it('shows a failed first read, though no question was read', () => {
    render(VoiceTasks, { voiceTasks: { tasks: [], error: 'request failed: 500' } })

    expect(within(section()!).getByRole('alert').textContent).toContain('request failed: 500')
  })
})
