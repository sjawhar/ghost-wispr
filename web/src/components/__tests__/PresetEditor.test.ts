import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { PresetDetail } from '../../lib/config-api'
import PresetEditor from '../PresetEditor.svelte'
import { configState, resetConfigState } from '../../lib/config.svelte'
import { deferredFetch, sent, stubFetch, unitConfig } from './configFixture'

const presets: Record<string, PresetDetail> = {
  standup: {
    description: 'Daily standup notes',
    system_prompt: 'Summarize the standup.',
    user_template: '{{transcript}}',
    model: 'openai/gpt-4o-mini',
  },
}

function field(label: string): HTMLInputElement | HTMLTextAreaElement {
  return screen.getByLabelText(label) as HTMLInputElement | HTMLTextAreaElement
}

async function expandStandup() {
  const view = render(PresetEditor, { presets })
  await fireEvent.click(screen.getByRole('button', { name: /standup/ }))
  return view
}

function overlay(container: HTMLElement): HTMLElement {
  const element = container.querySelector('.test-modal-overlay')
  if (!(element instanceof HTMLElement)) throw new Error('No dialog overlay is open')
  return element
}

// The Test dialog loads the unit's recent sessions; the unit has none.
function stubNoSessions(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response('[]'))),
  )
}

const dialogs = [
  { button: 'Test', heading: 'Test Preset: standup', closer: 'Close' },
  { button: 'Refine', heading: 'Refine Preset: standup', closer: 'Cancel' },
]

describe('PresetEditor', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it("names each field of a preset's form by its label", async () => {
    await expandStandup()

    expect(field('Name').value).toBe('standup')
    expect(field('Description').value).toBe('Daily standup notes')
    expect(field('System Prompt').value).toBe('Summarize the standup.')
    expect(field('User Template').value).toBe('{{transcript}}')
    expect(field('Model Override (optional)').value).toBe('openai/gpt-4o-mini')
  })

  it("names each field of a new preset's form by its label", async () => {
    render(PresetEditor, { presets })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    expect(field('Name').placeholder).toBe('my-preset')
    expect(field('Description').placeholder).toBe('What kind of summary do you want?')

    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    expect(field('System Prompt').value).toBe('')
    expect(field('User Template').value).toBe('{{transcript}}')
    expect(field('Model Override (optional)').placeholder).toBe('provider/model_name')
  })

  // The unit merges a preset's fields: a field sent replaces the unit's. The
  // model changed on the unit after the form opened, and the form was not
  // told: a save of the description alone must not send the old model.
  it('sends only the fields that were changed, so a save reverts no change made elsewhere', async () => {
    const fetch = stubFetch()
    await expandStandup()

    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        {
          url: '/api/config',
          method: 'PATCH',
          body: { summarization: { presets: { standup: { description: 'Weekly notes' } } } },
        },
      ]),
    )
  })

  // The model changed on the unit while the form was open; the first save's
  // answer carries it. The field must take it, so the next save does not send
  // the old model back.
  it("takes a change its save's answer carries, so the next save does not undo it", async () => {
    const answered = {
      ...unitConfig,
      summarization: {
        ...unitConfig.summarization,
        presets: {
          standup: {
            ...presets.standup,
            description: 'Weekly notes',
            model: 'vertex/gemini-4-carbon',
          },
        },
      },
    }
    const fetch = stubFetch({ config: answered })
    const view = await expandStandup()

    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('Saved!')).toBeTruthy()
    // The settings page passes in the presets of the config the save stored.
    await view.rerender({ presets: configState.config!.summarization.presets })
    expect(field('Model Override (optional)').value).toBe('vertex/gemini-4-carbon')
    // Neither the person's own save nor a change to a field nobody typed in
    // replaced an edit.
    expect(
      screen.queryByText('Changed on the unit or in another tab; your edit was replaced.'),
    ).toBeNull()
    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes, short' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(
        sent(fetch)
          .filter((r) => r.method === 'PATCH')
          .map((r) => r.body),
      ).toEqual([
        { summarization: { presets: { standup: { description: 'Weekly notes' } } } },
        { summarization: { presets: { standup: { description: 'Weekly notes, short' } } } },
      ]),
    )
  })

  // Another tab deleted the preset while it was open here; the unit refuses
  // the save of its changed fields with 404 rather than recreate it.
  it('says a preset was deleted elsewhere when the unit answers 404', async () => {
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
    await expandStandup()

    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const shown = await screen.findByText('This preset was deleted elsewhere.')
    expect(shown.classList.contains('error')).toBe(true)
  })

  it('discards what was typed, and then has nothing to save', async () => {
    const fetch = stubFetch()
    await expandStandup()
    await fireEvent.input(field('Description'), { target: { value: 'Typed' } })

    await fireEvent.click(screen.getByRole('button', { name: 'Discard' }))
    expect(field('Description').value).toBe('Daily standup notes')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('No changes to save.')).toBeTruthy()
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  // Opening another preset starts its form over: the first one's unsaved
  // typing goes, with no note, and is not sent with the other preset.
  it("starts another preset's form over, with no note and nothing to save", async () => {
    const fetch = stubFetch()
    const retro = {
      description: 'Retro notes',
      system_prompt: 'Summarize the retro.',
      user_template: '{{transcript}}',
      model: '',
    }
    render(PresetEditor, { presets: { ...presets, retro } })
    await fireEvent.click(screen.getByRole('button', { name: /standup/ }))
    await fireEvent.input(field('Description'), { target: { value: 'Typed' } })

    await fireEvent.click(screen.getByRole('button', { name: /retro/ }))
    expect(field('Description').value).toBe('Retro notes')
    expect(
      screen.queryByText('Changed on the unit or in another tab; your edit was replaced.'),
    ).toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('No changes to save.')).toBeTruthy()
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  it('saves nothing when no field was changed', async () => {
    const fetch = stubFetch()
    await expandStandup()

    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const shown = await screen.findByText('No changes to save.')
    expect(shown.classList.contains('error')).toBe(false)
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  it('shows a field the unit changed, and keeps what was typed in another', async () => {
    const view = await expandStandup()
    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes' } })

    await view.rerender({
      presets: { standup: { ...presets.standup, model: 'vertex/gemini-4-carbon' } },
    })

    expect(field('Model Override (optional)').value).toBe('vertex/gemini-4-carbon')
    expect(field('Description').value).toBe('Weekly notes')
  })

  // The other tab changes the description the person is typing: the field
  // takes it, and says so next to it until the person types there again.
  it('says when a change from elsewhere replaced the description being typed', async () => {
    const note = 'Changed on the unit or in another tab; your edit was replaced.'
    const view = await expandStandup()
    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes' } })

    await view.rerender({
      presets: { standup: { ...presets.standup, model: 'vertex/gemini-4-carbon' } },
    })
    expect(screen.queryByText(note)).toBeNull()
    await view.rerender({
      presets: {
        standup: { ...presets.standup, description: 'Elsewhere', model: 'vertex/gemini-4-carbon' },
      },
    })

    expect(field('Description').value).toBe('Elsewhere')
    expect(field('Description').closest('.field')?.textContent).toContain(note)
    await fireEvent.input(field('Description'), { target: { value: 'Mine again' } })
    expect(screen.queryByText(note)).toBeNull()
  })

  // The person opened another preset while the first one's save was on its
  // way: the answer must not write the saved preset's fields into it.
  it('leaves a preset opened during a save as it is when the save lands', async () => {
    const answer = deferredFetch()
    const retro = {
      description: 'Retro notes',
      system_prompt: 'Summarize the retro.',
      user_template: '{{transcript}}',
      model: '',
    }
    const { container } = render(PresetEditor, { presets: { ...presets, retro } })
    await fireEvent.click(screen.getByRole('button', { name: /standup/ }))
    await fireEvent.input(field('Description'), { target: { value: 'Weekly notes' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const editable = [
      ...container.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>(
        '.preset-form input:not(#preset-name), .preset-form textarea',
      ),
    ]
    expect(editable).toHaveLength(4)
    expect(editable.every((control) => control.disabled)).toBe(true)
    await fireEvent.click(screen.getByRole('button', { name: /retro/ }))
    answer(
      new Response(
        JSON.stringify({
          ...unitConfig,
          summarization: {
            ...unitConfig.summarization,
            presets: { standup: { ...presets.standup, description: 'Weekly notes' }, retro },
          },
        }),
      ),
    )

    await waitFor(() => expect((field('Description') as HTMLInputElement).disabled).toBe(false))
    expect(field('Name').value).toBe('retro')
    expect(field('Description').value).toBe('Retro notes')
    expect(field('System Prompt').value).toBe('Summarize the retro.')
    expect(field('Model Override (optional)').value).toBe('')
    expect(screen.queryByText('Saved!')).toBeNull()
  })

  it('sends every field of a new preset', async () => {
    const fetch = stubFetch()
    render(PresetEditor, { presets })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    await fireEvent.input(field('Name'), { target: { value: 'retro' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    await fireEvent.input(field('System Prompt'), { target: { value: 'Summarize the retro.' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        {
          url: '/api/config',
          method: 'PATCH',
          body: {
            summarization: {
              presets: {
                retro: {
                  description: '',
                  system_prompt: 'Summarize the retro.',
                  user_template: '{{transcript}}',
                  model: '',
                },
              },
            },
          },
        },
      ]),
    )
  })

  // A second "+ Add Preset" while a Create is on its way opens a new blank
  // form; the first Create's answer must not fill it.
  it('leaves a second new-preset form as it is when the first Create lands', async () => {
    const answer = deferredFetch()
    const { container } = render(PresetEditor, { presets })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    await fireEvent.input(field('Name'), { target: { value: 'retro' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    await fireEvent.input(field('System Prompt'), { target: { value: 'Summarize the retro.' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    const editable = [
      ...container.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>(
        '.preset-form input, .preset-form textarea',
      ),
    ]
    expect(editable).toHaveLength(5)
    expect(editable.every((control) => control.disabled)).toBe(true)
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    const retro = {
      description: '',
      system_prompt: 'Summarize the retro.',
      user_template: '{{transcript}}',
      model: '',
    }
    answer(
      new Response(
        JSON.stringify({
          ...unitConfig,
          summarization: { ...unitConfig.summarization, presets: { ...presets, retro } },
        }),
      ),
    )

    await waitFor(() => expect((field('Name') as HTMLInputElement).disabled).toBe(false))
    expect(field('Name').value).toBe('')
    expect(screen.getByRole('button', { name: 'Generate with LLM' })).toBeTruthy()
    expect(screen.queryByText('Saved!')).toBeNull()
  })

  // A new preset sends every field, so one under a name already taken would
  // replace that preset whole.
  it('creates no preset under a name already taken', async () => {
    const fetch = stubFetch()
    render(PresetEditor, { presets })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    await fireEvent.input(field('Name'), { target: { value: 'standup' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    await fireEvent.input(field('System Prompt'), { target: { value: 'Something else.' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    const shown = await screen.findByText('A preset named "standup" already exists.')
    expect(shown.classList.contains('error')).toBe(true)
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  // Every object has toString and constructor; no preset does here.
  it('creates a preset under a name every object has built in', async () => {
    const fetch = stubFetch()
    render(PresetEditor, { presets })
    await fireEvent.click(screen.getByRole('button', { name: '+ Add Preset' }))
    await fireEvent.input(field('Name'), { target: { value: 'toString' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit manually' }))
    await fireEvent.input(field('System Prompt'), { target: { value: 'Summarize.' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() =>
      expect(
        sent(fetch)
          .filter((r) => r.method === 'PATCH')
          .map((r) => r.body),
      ).toEqual([
        {
          summarization: {
            presets: {
              toString: {
                description: '',
                system_prompt: 'Summarize.',
                user_template: '{{transcript}}',
                model: '',
              },
            },
          },
        },
      ]),
    )
  })

  it.each(dialogs)(
    'keeps the $button dialog open on a click inside it',
    async ({ button, heading }) => {
      stubNoSessions()
      await expandStandup()
      await fireEvent.click(screen.getByRole('button', { name: button }))

      await fireEvent.click(screen.getByRole('heading', { name: heading }))
      await fireEvent.click(screen.getByRole('dialog'))

      expect(screen.getByRole('dialog')).toBeTruthy()
    },
  )

  // Selecting text in a field and letting go past the dialog's edge ends the
  // press on the overlay; the browser then sends the click to the overlay.
  it.each(dialogs)(
    'keeps the $button dialog open when a press inside it ends on the overlay',
    async ({ button, heading }) => {
      stubNoSessions()
      const { container } = await expandStandup()
      await fireEvent.click(screen.getByRole('button', { name: button }))

      await fireEvent.pointerDown(screen.getByRole('heading', { name: heading }))
      await fireEvent.click(overlay(container))

      expect(screen.getByRole('dialog')).toBeTruthy()
    },
  )

  it.each(dialogs)(
    'closes the $button dialog on a click on the overlay around it',
    async ({ button }) => {
      stubNoSessions()
      const { container } = await expandStandup()
      await fireEvent.click(screen.getByRole('button', { name: button }))

      await fireEvent.pointerDown(overlay(container))
      await fireEvent.click(overlay(container))

      expect(screen.queryByRole('dialog')).toBeNull()
    },
  )

  it.each(dialogs)(
    'closes the $button dialog with its $closer button',
    async ({ button, closer }) => {
      stubNoSessions()
      await expandStandup()
      await fireEvent.click(screen.getByRole('button', { name: button }))

      await fireEvent.click(screen.getByRole('button', { name: closer }))

      expect(screen.queryByRole('dialog')).toBeNull()
    },
  )

  // Refine answers with every field of the preset, and with the preset it
  // started from, as the unit held it (from; left out: a server older than
  // from). The unit answers a save with the config.
  function stubRefine(
    result: Omit<PresetDetail, 'model'>,
    started: { from?: PresetDetail } = { from: presets.standup },
  ) {
    const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/config/presets/refine') {
        return Promise.resolve(new Response(JSON.stringify({ ...result, model: '', ...started })))
      }
      if (init?.method === 'PATCH') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              ...unitConfig,
              summarization: { ...unitConfig.summarization, presets },
            }),
          ),
        )
      }
      return Promise.resolve(new Response('[]'))
    })
    vi.stubGlobal('fetch', fetch)
    return fetch
  }

  async function refine(): Promise<void> {
    await fireEvent.click(screen.getByRole('button', { name: 'Refine' }))
    await fireEvent.input(field('What would you like to change?'), {
      target: { value: 'Shorter, please.' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Refine with LLM' }))
    expect(await screen.findByText('Refined! Review the changes and Save.')).toBeTruthy()
  }

  function patches(fetch: ReturnType<typeof stubRefine>): unknown[] {
    return fetch.mock.calls
      .filter(([, init]) => init?.method === 'PATCH')
      .map(([, init]) => JSON.parse(String(init?.body)))
  }

  // Sol's case: a refinement changes only the system prompt. A save sends
  // only that, so it does not send the description back over a change made
  // to it in another tab.
  it('sends only the fields a refinement changed', async () => {
    const fetch = stubRefine({
      description: 'Daily standup notes',
      system_prompt: 'More concise system prompt',
      user_template: '{{transcript}}',
    })
    await expandStandup()

    await refine()
    expect(field('System Prompt').value).toBe('More concise system prompt')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(patches(fetch)).toEqual([
        {
          summarization: { presets: { standup: { system_prompt: 'More concise system prompt' } } },
        },
      ]),
    )
  })

  it('leaves nothing to save after a refinement that changed nothing', async () => {
    const fetch = stubRefine({
      description: 'Daily standup notes\n',
      system_prompt: 'Summarize the standup.',
      user_template: '{{transcript}}',
    })
    await expandStandup()

    await refine()
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('No changes to save.')).toBeTruthy()
    expect(patches(fetch)).toEqual([])
  })

  // A refinement starts from the preset as the unit holds it: a field it
  // returns unchanged keeps what the person typed there, and the save sends
  // the typing with the refined prompt.
  it('keeps what was typed in a field a refinement did not change', async () => {
    const fetch = stubRefine({
      description: 'Daily standup notes',
      system_prompt: 'More concise system prompt',
      user_template: '{{transcript}}',
    })
    await expandStandup()
    await fireEvent.input(field('Description'), { target: { value: 'Mine' } })

    await refine()
    expect(field('Description').value).toBe('Mine')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(patches(fetch)).toEqual([
        {
          summarization: {
            presets: {
              standup: { description: 'Mine', system_prompt: 'More concise system prompt' },
            },
          },
        },
      ]),
    )
  })

  // The proof's case: another tab set the description "Elsewhere", which this
  // page has not read. The refinement starts from the unit's preset, returns
  // "Elsewhere" as it found it, and changes the prompt. The description is no
  // edit, so a save does not send it over a later change.
  it('sends no field the refinement found already changed on the unit', async () => {
    const fetch = stubRefine(
      {
        description: 'Elsewhere',
        system_prompt: 'New prompt',
        user_template: '{{transcript}}',
      },
      { from: { ...presets.standup, description: 'Elsewhere' } },
    )
    await expandStandup()

    await refine()
    expect(field('Description').value).toBe('Daily standup notes')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(patches(fetch)).toEqual([
        { summarization: { presets: { standup: { system_prompt: 'New prompt' } } } },
      ]),
    )
  })

  // A server older than from: a field is a refinement's change where it
  // differs from the config and from what the field shows. Nothing is dropped.
  it('takes a refinement from a server that names no starting preset', async () => {
    const fetch = stubRefine(
      {
        description: 'Daily standup notes',
        system_prompt: 'New prompt',
        user_template: 'Notes: {{transcript}}',
      },
      {},
    )
    await expandStandup()

    await refine()
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(patches(fetch)).toEqual([
        {
          summarization: {
            presets: {
              standup: { system_prompt: 'New prompt', user_template: 'Notes: {{transcript}}' },
            },
          },
        },
      ]),
    )
  })

  it('takes no refinement while its save is on its way', async () => {
    const answer = deferredFetch()
    await expandStandup()
    await fireEvent.input(field('Description'), { target: { value: 'Mine' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect((screen.getByRole('button', { name: 'Refine' }) as HTMLButtonElement).disabled).toBe(
      true,
    )
    answer(
      new Response(
        JSON.stringify({ ...unitConfig, summarization: { ...unitConfig.summarization, presets } }),
      ),
    )
    await waitFor(() =>
      expect((screen.getByRole('button', { name: 'Refine' }) as HTMLButtonElement).disabled).toBe(
        false,
      ),
    )
  })
})
