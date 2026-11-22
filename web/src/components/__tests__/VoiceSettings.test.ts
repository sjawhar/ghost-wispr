import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import VoiceSettings from '../VoiceSettings.svelte'
import SettingsPage from '../SettingsPage.svelte'
import type { ConfigResponse } from '../../lib/config-api'
import { configState, resetConfigState, setConfig } from '../../lib/config.svelte'
import { deferredFetch, sent, stubFetch, unitConfig } from './configFixture'

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
    const view = render(VoiceSettings, { config })

    await fireEvent.input(screen.getByLabelText('Text-to-speech model'), {
      target: { value: 'deepgram/aura-2-example-en' },
    })
    await save()
    await waitFor(() => expect(configState.config).toEqual(answer))
    // The settings page passes in the config the save stored.
    await view.rerender({ config: configState.config! })
    expect((screen.getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe('')
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

  it('shows a voice setting the unit changed', async () => {
    const view = render(VoiceSettings, { config })

    await view.rerender({ config: { ...config, tts: { ...config.tts, voice: 'Charon' } } })

    expect((screen.getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe('Charon')
  })

  it('keeps a typed voice when the unit changes another setting', async () => {
    const view = render(VoiceSettings, { config })
    await fireEvent.input(screen.getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })

    await view.rerender({ config: { ...config, voice: { enabled: false, engine: 'tts' } } })

    expect((screen.getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe('Puck')
    expect(checked('Enable meeting voice')).toBe(false)
  })

  // Text typed while a save is on its way would be replaced by the save's
  // answer, so the fields take none until it lands.
  it('takes no typing while its save is on its way', async () => {
    const answer = deferredFetch()
    const { container } = render(VoiceSettings, { config })
    const controls = [...container.querySelectorAll('input')]
    expect(controls).toHaveLength(6)

    await fireEvent.input(screen.getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await save()

    expect(controls.every((control) => control.disabled)).toBe(true)
    answer(new Response(JSON.stringify({ ...config, tts: { ...config.tts, voice: 'Puck' } })))
    await waitFor(() => expect(controls.some((control) => control.disabled)).toBe(false))
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

  // The app loaded the config when it started; the voice was turned off on
  // the unit since, and the settings page loads the config again when it
  // opens. A save of the voice alone must not turn the meeting voice back on.
  it('shows the config it loads, and a save reverts no change made since the app loaded', async () => {
    setConfig(config)
    const fetch = stubFetch({ config: { ...config, voice: { enabled: false, engine: 'tts' } } })
    render(SettingsPage, { onBack: () => {} })
    await waitFor(() => expect(configState.config?.voice.enabled).toBe(false))

    expect(checked('Enable meeting voice')).toBe(false)
    const voice = screen.getByTestId('voice-settings')
    await fireEvent.input(within(voice).getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await fireEvent.click(within(voice).getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        { url: '/api/config', method: 'PATCH', body: { tts: { voice: 'Puck' } } },
      ]),
    )
  })

  // A General save goes out first and its answer, with voice Kore, lands
  // last; the voice save of Puck goes out second and lands first. The older
  // answer must not put Kore back in the voice field.
  it('keeps a newer save when an older answer lands after it', async () => {
    const answers: ((answer: Response) => void)[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input).startsWith('/api/providers')) {
          return Promise.resolve(new Response(JSON.stringify({ providers: [] })))
        }
        if (init?.method === 'PATCH') {
          // The app's TypeScript library, ES2022, has no Promise.withResolvers.
          return new Promise<Response>((resolve) => answers.push(resolve))
        }
        return Promise.resolve(new Response(JSON.stringify(config)))
      }),
    )
    render(SettingsPage, { onBack: () => {} })
    const general = await screen.findByTestId('general-settings')
    const voice = screen.getByTestId('voice-settings')

    await fireEvent.input(within(general).getByLabelText('Silence Timeout'), {
      target: { value: '45s' },
    })
    await fireEvent.click(within(general).getByRole('button', { name: 'Save' }))
    await fireEvent.input(within(voice).getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await fireEvent.click(within(voice).getByRole('button', { name: 'Save' }))
    expect(answers).toHaveLength(2)

    answers[1](new Response(JSON.stringify({ ...config, tts: { ...config.tts, voice: 'Puck' } })))
    expect(await within(voice).findByText('Saved!')).toBeTruthy()
    answers[0](new Response(JSON.stringify({ ...config, silence_timeout: '45s' })))
    expect(await within(general).findByText('Saved!')).toBeTruthy()

    expect((within(voice).getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe(
      'Puck',
    )
    expect(configState.config?.tts.voice).toBe('Puck')
  })
})
