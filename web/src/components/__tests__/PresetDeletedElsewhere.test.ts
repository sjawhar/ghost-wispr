import { fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import PresetEditor from '../PresetEditor.svelte'
import { resetConfigState } from '../../lib/config.svelte'

// The unit answers 404 to a change to a preset it does not hold that does not
// create it: another tab or the unit deleted the preset while it was open here.
describe('A save to a preset deleted elsewhere', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('says the preset was deleted elsewhere', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              error:
                'summarization preset "standup" does not exist; a new preset names its description, system_prompt, user_template and model, with a user_template',
            }),
            { status: 404 },
          ),
        ),
      ),
    )
    const { container } = render(PresetEditor, {
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

    const [, description] = container.querySelectorAll('.preset-form input')
    await fireEvent.input(description, { target: { value: 'Weekly notes' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const shown = await screen.findByText('This preset was deleted elsewhere.')
    expect(shown.classList.contains('error')).toBe(true)
  })

  // The control: a new preset's refusal shows the unit's own words.
  it("shows the unit's words when a new preset is refused", async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(new Response(JSON.stringify({ error: 'unit busy' }), { status: 404 })),
      ),
    )
    const { container } = render(PresetEditor, { presets: {} })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    await fireEvent.input(screen.getByPlaceholderText('my-preset'), { target: { value: 'retro' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    const [systemPrompt] = container.querySelectorAll('textarea')
    await fireEvent.input(systemPrompt, { target: { value: 'Summarize the retro.' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(await screen.findByText('unit busy')).toBeTruthy()
  })
})
