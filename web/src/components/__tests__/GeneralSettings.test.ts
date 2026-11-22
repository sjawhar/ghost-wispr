import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import GeneralSettings from '../GeneralSettings.svelte'
import { resetConfigState } from '../../lib/config.svelte'
import { deferredFetch, sent, stubFetch, unitConfig as config } from './configFixture'

function value(label: string): string {
  return (screen.getByLabelText(label) as HTMLInputElement).value
}

describe('GeneralSettings', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('shows the settings the unit holds', () => {
    render(GeneralSettings, { config })

    expect(value('Silence Timeout')).toBe('30s')
    expect(value('Default Summarization Model')).toBe('vertex/gemini-4-carbon')
    expect(value('Transcription Endpointing (ms)')).toBe('400')
    expect(value('Utterance End (ms)')).toBe('1000')
  })

  it('shows a setting the unit changed', async () => {
    const view = render(GeneralSettings, { config })

    await view.rerender({ config: { ...config, silence_timeout: '1m' } })

    expect(value('Silence Timeout')).toBe('1m')
  })

  it('keeps what was typed when the unit changes another setting', async () => {
    const view = render(GeneralSettings, { config })
    await fireEvent.input(screen.getByLabelText('Default Summarization Model'), {
      target: { value: 'openai/gpt-4o-mini' },
    })

    await view.rerender({ config: { ...config, silence_timeout: '1m' } })

    expect(value('Default Summarization Model')).toBe('openai/gpt-4o-mini')
    expect(value('Silence Timeout')).toBe('1m')
  })

  // The unit's utterance end changed after the page loaded, and the page was
  // not told: a save of the endpointing alone must not send the old value.
  it('sends only the settings that were changed, so a save reverts no change made elsewhere', async () => {
    const fetch = stubFetch({
      config: { ...config, transcription: { endpointing: '400', utterance_end_ms: '1500' } },
    })
    render(GeneralSettings, { config })

    await fireEvent.input(screen.getByLabelText('Transcription Endpointing (ms)'), {
      target: { value: '300' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        { url: '/api/config', method: 'PATCH', body: { transcription: { endpointing: '300' } } },
      ]),
    )
  })

  // Text typed while a save is on its way would be replaced by the save's
  // answer, so the fields take none until it lands.
  it('takes no typing while its save is on its way', async () => {
    const answer = deferredFetch()
    render(GeneralSettings, { config })
    const silence = screen.getByLabelText('Silence Timeout') as HTMLInputElement

    await fireEvent.input(silence, { target: { value: '45s' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(silence.disabled).toBe(true)
    expect(
      (screen.getByLabelText('Default Summarization Model') as HTMLInputElement).disabled,
    ).toBe(true)
    answer(new Response(JSON.stringify({ ...config, silence_timeout: '45s' })))
    await waitFor(() => expect(silence.disabled).toBe(false))
    expect(silence.value).toBe('45s')
  })
})
