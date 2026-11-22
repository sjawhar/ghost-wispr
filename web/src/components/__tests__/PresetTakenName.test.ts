import { fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import PresetEditor from '../PresetEditor.svelte'
import { resetConfigState } from '../../lib/config.svelte'
import { sent, stubFetch } from './configFixture'

// A new preset sends every field of its form, and the unit sets each field it
// is sent, "" included (a preset's model or description of "" clears it). A
// new preset under a name already taken would replace that preset: its
// description and model would become those of the blank form.
describe('A new preset under a name already taken', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('is refused, and nothing is sent', async () => {
    const fetch = stubFetch()
    const { container } = render(PresetEditor, {
      presets: {
        standup: {
          description: 'Daily standup notes',
          system_prompt: 'Summarize the standup.',
          user_template: '{{transcript}}',
          model: 'openai/gpt-4o-mini',
        },
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    await fireEvent.input(screen.getByPlaceholderText('my-preset'), {
      target: { value: 'standup' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    const [systemPrompt] = container.querySelectorAll('textarea')
    await fireEvent.input(systemPrompt, { target: { value: 'Something else.' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    const shown = await screen.findByText('A preset named "standup" already exists.')
    expect(shown.classList.contains('error')).toBe(true)
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })
})
