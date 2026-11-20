import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import VoiceSettings from '../VoiceSettings.svelte'
import SettingsPage from '../SettingsPage.svelte'
import type { ConfigResponse } from '../../lib/config-api'
import { configState, resetConfigState } from '../../lib/config.svelte'
import { sent, stubFetch, unitConfig } from './configFixture'

const config: ConfigResponse = {
  ...unitConfig,
  tts: { model: 'vertex/gemini-3.8-flash-tts', voice: 'Kore' },
  voice: { enabled: true, engine: 'tts' },
}

function checked(label: string): boolean {
  return (screen.getByLabelText(label) as HTMLInputElement).checked
}

function save(): Promise<boolean> {
  return fireEvent.click(screen.getByRole('button', { name: 'Save' }))
}

describe('VoiceSettings', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('shows the voice the unit holds, in the standard terms', () => {
    render(VoiceSettings, { config })

    expect(checked('Enable meeting voice')).toBe(true)
    expect(checked('Text-to-speech (tts)')).toBe(true)
    expect(checked('Speech-to-speech: OpenAI GPT-Live (gptlive)')).toBe(false)
    expect(checked('Voice agent: Deepgram (dgagent)')).toBe(false)
  })

  it('offers the text-to-speech model and voice under text-to-speech, not a Gemini or Aura choice', () => {
    render(VoiceSettings, { config })

    expect((screen.getByLabelText('Text-to-speech model') as HTMLInputElement).value).toBe(
      'vertex/gemini-3.8-flash-tts',
    )
    expect((screen.getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe('Kore')
    expect(screen.queryByLabelText('Gemini')).toBeNull()
    expect(screen.queryByLabelText('Aura')).toBeNull()
  })

  it('shows a voice that is off and another voice, without the text-to-speech fields', () => {
    render(VoiceSettings, {
      config: { ...config, voice: { enabled: false, engine: 'dgagent' } },
    })

    expect(checked('Enable meeting voice')).toBe(false)
    expect(checked('Voice agent: Deepgram (dgagent)')).toBe(true)
    expect(checked('Text-to-speech (tts)')).toBe(false)
    expect(screen.queryByLabelText('Text-to-speech model')).toBeNull()
  })

  it('sends only the voice fields that changed, and keeps what the unit answers', async () => {
    const answer: ConfigResponse = { ...config, voice: { enabled: true, engine: 'gptlive' } }
    const fetch = stubFetch({ config: answer })
    render(VoiceSettings, { config })

    await fireEvent.click(screen.getByLabelText('Speech-to-speech: OpenAI GPT-Live (gptlive)'))
    await save()

    expect(sent(fetch)).toEqual([
      { url: '/api/config', method: 'PATCH', body: { voice: { engine: 'gptlive' } } },
    ])
    await waitFor(() => expect(configState.config).toEqual(answer))
  })

  it('saves the text-to-speech model and voice with tts', async () => {
    const fetch = stubFetch()
    render(VoiceSettings, { config })

    await fireEvent.input(screen.getByLabelText('Text-to-speech model'), {
      target: { value: ' deepgram/aura-2-example-en ' },
    })
    await fireEvent.input(screen.getByLabelText('Text-to-speech voice'), { target: { value: '' } })
    await save()

    expect(sent(fetch)).toEqual([
      {
        url: '/api/config',
        method: 'PATCH',
        body: { tts: { model: 'deepgram/aura-2-example-en', voice: '' } },
      },
    ])
  })

  it('shows the voice the unit answers after a new model, and saves nothing more after it', async () => {
    // A new model without a voice speaks in its own default: the unit answers
    // tts.voice "", and the page must not send the old model's voice back.
    const answer: ConfigResponse = {
      ...config,
      tts: { model: 'deepgram/aura-2-example-en', voice: '' },
    }
    const fetch = stubFetch({ config: answer })
    render(VoiceSettings, { config })

    await fireEvent.input(screen.getByLabelText('Text-to-speech model'), {
      target: { value: 'deepgram/aura-2-example-en' },
    })
    await save()
    await waitFor(() =>
      expect((screen.getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe(''),
    )
    await save()

    expect(sent(fetch)).toEqual([
      {
        url: '/api/config',
        method: 'PATCH',
        body: { tts: { model: 'deepgram/aura-2-example-en' } },
      },
    ])
    expect(await screen.findByText('No changes to save.')).toBeTruthy()
  })

  it('sends the switch and a changed text-to-speech voice together', async () => {
    const fetch = stubFetch()
    render(VoiceSettings, { config })

    await fireEvent.click(screen.getByLabelText('Enable meeting voice'))
    await fireEvent.input(screen.getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await save()

    expect(sent(fetch)).toEqual([
      {
        url: '/api/config',
        method: 'PATCH',
        body: { voice: { enabled: false }, tts: { voice: 'Puck' } },
      },
    ])
  })

  it('sends nothing when nothing changed', async () => {
    const fetch = stubFetch()
    render(VoiceSettings, { config })

    await save()

    expect(fetch).not.toHaveBeenCalled()
  })

  it("shows the unit's refusal as an error", async () => {
    const refusal = 'tts.model names provider "nowhere", which providers does not define'
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: refusal }), { status: 400 })),
    )
    render(VoiceSettings, { config })

    await fireEvent.input(screen.getByLabelText('Text-to-speech model'), {
      target: { value: 'nowhere/tts-example' },
    })
    await save()

    const shown = await screen.findByText(refusal)
    expect(shown.classList.contains('error')).toBe(true)
    expect(configState.config).toBeNull()
  })
})

describe('SettingsPage', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('has the Meeting voice section', async () => {
    stubFetch({ config })
    render(SettingsPage, { onBack: () => {} })

    expect(await screen.findByRole('heading', { name: 'Meeting voice' })).toBeTruthy()
    expect(checked('Text-to-speech (tts)')).toBe(true)
  })
})
