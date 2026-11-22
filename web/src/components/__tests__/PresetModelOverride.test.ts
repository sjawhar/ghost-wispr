import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import PresetEditor from '../PresetEditor.svelte'
import { resetConfigState } from '../../lib/config.svelte'
import { sent, stubFetch } from './configFixture'

describe("A preset's model override", () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  // The unit clears a preset's model when a patch names it as '', and the
  // preset then uses summarization.model.
  it('is cleared by sending an empty model', async () => {
    const fetch = stubFetch()
    render(PresetEditor, {
      presets: {
        standup: {
          description: 'Stand-up',
          system_prompt: 'Summarize briefly',
          user_template: '{{transcript}}',
          model: 'vertex/gemini-3.5-flash-lite',
        },
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /standup/ }))

    await fireEvent.input(screen.getByPlaceholderText('provider/model_name'), {
      target: { value: '' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      const patches = sent(fetch).filter((r) => r.method === 'PATCH')
      expect(patches).toHaveLength(1)
      expect(patches[0].body).toMatchObject({
        summarization: { presets: { standup: { model: '' } } },
      })
    })
  })
})
