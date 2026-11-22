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
    const { container } = render(GeneralSettings, { config })
    const inputs = [...container.querySelectorAll('input')]
    expect(inputs).toHaveLength(4)

    await fireEvent.input(screen.getByLabelText('Silence Timeout'), { target: { value: '45s' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(inputs.map((input) => input.disabled)).toEqual([true, true, true, true])
    answer(new Response(JSON.stringify({ ...config, silence_timeout: '45s' })))
    await waitFor(() =>
      expect(inputs.map((input) => input.disabled)).toEqual([false, false, false, false]),
    )
    expect(value('Silence Timeout')).toBe('45s')
  })

  // The unit changed a field the person had typed in: the field shows the
  // unit's value, and the person's edit is over, so nothing is sent.
  it('sends nothing for a field whose typing the unit replaced', async () => {
    const fetch = stubFetch()
    const view = render(GeneralSettings, { config })
    await fireEvent.input(screen.getByLabelText('Silence Timeout'), { target: { value: '45s' } })

    await view.rerender({ config: { ...config, silence_timeout: '1m' } })
    expect(value('Silence Timeout')).toBe('1m')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('No changes to save.')).toBeTruthy()
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  // Sol's case: 45s is typed over 30s; another tab sets 60s, then 30s again,
  // then 90s, which this page has not read. The unit's change ended the edit
  // for good, so a save of only the endpointing sends only the endpointing.
  it('does not revive an edit when the unit returns to the value it was made over', async () => {
    const fetch = stubFetch()
    const view = render(GeneralSettings, { config })
    await fireEvent.input(screen.getByLabelText('Silence Timeout'), { target: { value: '45s' } })

    await view.rerender({ config: { ...config, silence_timeout: '60s' } })
    expect(value('Silence Timeout')).toBe('60s')
    await view.rerender({ config: { ...config, silence_timeout: '30s' } })
    expect(value('Silence Timeout')).toBe('30s')
    await fireEvent.input(screen.getByLabelText('Transcription Endpointing (ms)'), {
      target: { value: '500' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(
        sent(fetch)
          .filter((r) => r.method === 'PATCH')
          .map((r) => r.body),
      ).toEqual([{ transcription: { endpointing: '500' } }]),
    )
  })

  // A change from elsewhere that replaces what the person was typing says so
  // next to the field, until the person types there again.
  it('says when a change from elsewhere replaced what was typed, until the next input', async () => {
    const view = render(GeneralSettings, { config })
    const note = 'Changed on the unit or in another tab; your edit was replaced.'
    const silence = screen.getByLabelText('Silence Timeout')
    await fireEvent.input(silence, { target: { value: '45s' } })
    expect(screen.queryByText(note)).toBeNull()

    await view.rerender({ config: { ...config, silence_timeout: '60s' } })
    const shown = screen.getByText(note)
    expect(silence.closest('.field')?.contains(shown)).toBe(true)
    // A change to a field nobody was typing in says nothing.
    await view.rerender({
      config: {
        ...config,
        silence_timeout: '60s',
        transcription: { endpointing: '450', utterance_end_ms: '1000' },
      },
    })
    expect(screen.getAllByText(note)).toHaveLength(1)

    await fireEvent.input(silence, { target: { value: '50s' } })
    expect(screen.queryByText(note)).toBeNull()
  })
})
