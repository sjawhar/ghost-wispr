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

  // A value changed and changed back is still the person's value: it is sent.
  it('sends a value the person changed and changed back', async () => {
    const fetch = stubFetch({ config })
    render(VoiceSettings, { config })
    const voice = screen.getByLabelText('Text-to-speech voice')

    await fireEvent.input(voice, { target: { value: 'Puck' } })
    await fireEvent.input(voice, { target: { value: 'Kore' } })
    await save()

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        { url: '/api/config', method: 'PATCH', body: { tts: { voice: 'Kore' } } },
      ]),
    )
  })

  // A save acknowledges the fields it carried, and only those: the next save
  // sends a field edited after it, and not the one it carried.
  it('sends a field once its save is acknowledged, and a later edit again', async () => {
    const fetch = stubFetch({ config: { ...config, tts: { ...config.tts, voice: 'Puck' } } })
    const view = render(VoiceSettings, { config })

    await fireEvent.input(screen.getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await save()
    expect(await screen.findByText('Saved!')).toBeTruthy()
    // The settings page passes in the config the save stored: the person's
    // own save replaced no edit.
    await view.rerender({ config: { ...config, tts: { ...config.tts, voice: 'Puck' } } })
    expect(
      screen.queryByText('Changed on the unit or in another tab; your edit was replaced.'),
    ).toBeNull()
    await fireEvent.click(screen.getByLabelText('Enable meeting voice'))
    await save()

    await waitFor(() =>
      expect(
        sent(fetch)
          .filter((r) => r.method === 'PATCH')
          .map((r) => r.body),
      ).toEqual([{ tts: { voice: 'Puck' } }, { voice: { enabled: false } }]),
    )
  })

  // A save the unit refused carried nothing: the next save sends the field
  // again.
  it('sends a field again after a save of it failed', async () => {
    let answers = 0
    vi.stubGlobal(
      'fetch',
      vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
        answers++
        return Promise.resolve(
          answers === 1
            ? new Response(JSON.stringify({ error: 'unit busy' }), { status: 500 })
            : new Response(JSON.stringify({ ...config, tts: { ...config.tts, voice: 'Puck' } })),
        ).then((response) => {
          expect(JSON.parse(String(init?.body))).toEqual({ tts: { voice: 'Puck' } })
          return response
        })
      }),
    )
    render(VoiceSettings, { config })

    await fireEvent.input(screen.getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await save()
    expect(await screen.findByText('unit busy')).toBeTruthy()
    await save()

    expect(await screen.findByText('Saved!')).toBeTruthy()
    expect(answers).toBe(2)
  })

  // A save tells the field what the unit holds until the config says so too,
  // and no longer: when another tab then changes the voice back, the field
  // shows that.
  it('takes a later change of a saved field once the config has caught up', async () => {
    stubFetch({ config: { ...config, tts: { ...config.tts, voice: 'Puck' } } })
    const view = render(VoiceSettings, { config })
    const voice = screen.getByLabelText('Text-to-speech voice') as HTMLInputElement

    await fireEvent.input(voice, { target: { value: 'Puck' } })
    await save()
    expect(await screen.findByText('Saved!')).toBeTruthy()
    await view.rerender({ config: { ...config, tts: { ...config.tts, voice: 'Puck' } } })
    await view.rerender({ config })

    expect(voice.value).toBe('Kore')
  })

  it('says when a change from elsewhere replaced the voice being typed', async () => {
    const view = render(VoiceSettings, { config })
    const voice = screen.getByLabelText('Text-to-speech voice') as HTMLInputElement
    await fireEvent.input(voice, { target: { value: 'Puck' } })

    await view.rerender({ config: { ...config, tts: { ...config.tts, voice: 'Charon' } } })

    expect(voice.value).toBe('Charon')
    expect(
      screen.getByText('Changed on the unit or in another tab; your edit was replaced.'),
    ).toBeTruthy()
  })

  // The proof's case: the model typed with spaces around it is sent trimmed,
  // and another answer carries the trimmed value while the save is on its way.
  // Spaces around the text are no replacement.
  it('says nothing when another answer carries a trimmed save on its way', async () => {
    const answer = deferredFetch()
    const view = render(VoiceSettings, { config })
    await fireEvent.input(screen.getByLabelText('Text-to-speech model'), {
      target: { value: '  openai/tts-1-hd  ' },
    })
    await save()

    const saved = { ...config, tts: { ...config.tts, model: 'openai/tts-1-hd' } }
    await view.rerender({ config: saved })
    answer(new Response(JSON.stringify(saved)))
    expect(await screen.findByText('Saved!')).toBeTruthy()

    expect(
      screen.queryByText('Changed on the unit or in another tab; your edit was replaced.'),
    ).toBeNull()
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

  // A General save (45s) goes out first and a voice save (Puck) second, and
  // the voice save's answer lands first. Whichever save the unit applied
  // last holds the later state, with the higher revision count:
  // - the General save reached the unit first, so its answer holds voice
  //   Kore and must not put it back in the voice field;
  // - the General save reached the unit last, so its answer holds both
  //   changes and must be taken.
  it.each([
    {
      unitApplied: 'the General save first',
      generalAnswer: { silence_timeout: '45s', voice: 'Kore', count: 1 },
      voiceAnswer: { silence_timeout: '45s', voice: 'Puck', count: 2 },
    },
    {
      unitApplied: 'the voice save first',
      generalAnswer: { silence_timeout: '45s', voice: 'Puck', count: 2 },
      voiceAnswer: { silence_timeout: '30s', voice: 'Puck', count: 1 },
    },
  ])(
    'keeps the later state when $unitApplied and the answers land in the other order',
    async ({ generalAnswer, voiceAnswer }) => {
      const answer = ({ silence_timeout, voice, count }: typeof generalAnswer) =>
        new Response(
          JSON.stringify({
            ...config,
            silence_timeout,
            tts: { ...config.tts, voice },
            revision: { run: 'run-1', count },
          }),
        )
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
          return Promise.resolve(
            new Response(JSON.stringify({ ...config, revision: { run: 'run-1', count: 0 } })),
          )
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

      answers[1](answer(voiceAnswer))
      expect(await within(voice).findByText('Saved!')).toBeTruthy()
      answers[0](answer(generalAnswer))
      expect(await within(general).findByText('Saved!')).toBeTruthy()

      expect(configState.config?.tts.voice).toBe('Puck')
      expect(configState.config?.silence_timeout).toBe('45s')
      expect((within(voice).getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe(
        'Puck',
      )
      expect((within(general).getByLabelText('Silence Timeout') as HTMLInputElement).value).toBe(
        '45s',
      )
    },
  )

  // A fake unit for SettingsPage: a GET answers with the state the unit holds
  // now (`unit.current`), or, by `unit.readMode`, waits until the test answers
  // it, fails on the network, or answers 500; each PATCH waits until the test
  // answers it, and its body is kept in `unit.bodies`.
  function fakeUnit(start: ConfigResponse) {
    const unit = {
      current: start,
      patches: [] as ((answer: ConfigResponse) => void)[],
      bodies: [] as unknown[],
      reads: 0,
      readMode: 'answer' as 'answer' | 'hold' | 'network' | 'http500',
      held: [] as (() => void)[],
    }
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input).startsWith('/api/providers')) {
          return Promise.resolve(new Response(JSON.stringify({ providers: [] })))
        }
        if (init?.method === 'PATCH') {
          unit.bodies.push(JSON.parse(String(init.body)))
          // The app's TypeScript library, ES2022, has no Promise.withResolvers.
          return new Promise<Response>((resolve) =>
            unit.patches.push((answer) => resolve(new Response(JSON.stringify(answer)))),
          )
        }
        unit.reads++
        if (unit.readMode === 'hold') {
          return new Promise<Response>((resolve) =>
            unit.held.push(() => resolve(new Response(JSON.stringify(unit.current)))),
          )
        }
        if (unit.readMode === 'network') {
          return Promise.reject(new TypeError('Failed to fetch'))
        }
        if (unit.readMode === 'http500') {
          return Promise.resolve(
            new Response(JSON.stringify({ error: 'unit busy' }), { status: 500 }),
          )
        }
        return Promise.resolve(new Response(JSON.stringify(unit.current)))
      }),
    )
    return unit
  }

  // Sol's case: a General save sent to run A is held; the unit restarts as
  // run B, and a voice save of Puck lands there; then A's answer, holding
  // voice Kore, lands. The page reads the config again on each answer from
  // another run, and the live run B answers: the voice field keeps Puck.
  it('keeps a save made after a restart when the old run answers late', async () => {
    const unit = fakeUnit({ ...config, revision: { run: 'run-A', count: 9 } })
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
    expect(unit.patches).toHaveLength(2)

    const inB = {
      ...config,
      silence_timeout: '45s',
      tts: { ...config.tts, voice: 'Puck' },
      revision: { run: 'run-B', count: 1 },
    }
    unit.current = inB
    unit.patches[1](inB)
    expect(await within(voice).findByText('Saved!')).toBeTruthy()
    unit.patches[0]({
      ...config,
      silence_timeout: '45s',
      tts: { ...config.tts, voice: 'Kore' },
      revision: { run: 'run-A', count: 10 },
    })
    expect(await within(general).findByText('Saved!')).toBeTruthy()

    await waitFor(() => expect(configState.config?.revision).toEqual({ run: 'run-B', count: 1 }))
    expect((within(voice).getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe(
      'Puck',
    )
    expect(configState.config?.tts.voice).toBe('Puck')
  })

  // The unit restarts while silence is typed and not saved; a voice save is
  // answered by the new run, and the page reads the config again. The new
  // run holds the same silence, so the typing stays.
  it('keeps unsaved typing through a read of the config after a restart', async () => {
    const unit = fakeUnit({ ...config, revision: { run: 'run-A', count: 9 } })
    render(SettingsPage, { onBack: () => {} })
    const general = await screen.findByTestId('general-settings')
    const voice = screen.getByTestId('voice-settings')
    const silence = within(general).getByLabelText('Silence Timeout') as HTMLInputElement
    const readsBefore = unit.reads

    await fireEvent.input(silence, { target: { value: '41s' } })
    await fireEvent.input(within(voice).getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await fireEvent.click(within(voice).getByRole('button', { name: 'Save' }))
    const inB = {
      ...config,
      tts: { ...config.tts, voice: 'Puck' },
      revision: { run: 'run-B', count: 1 },
    }
    unit.current = inB
    unit.patches[0](inB)

    await waitFor(() => expect(configState.config?.revision).toEqual({ run: 'run-B', count: 1 }))
    expect(unit.reads).toBe(readsBefore + 1)
    expect(silence.value).toBe('41s')
    expect((within(voice).getByLabelText('Text-to-speech voice') as HTMLInputElement).value).toBe(
      'Puck',
    )
  })

  // Sol's case: the page shows run A with voice Kore. The unit restarts as
  // run B, and the person saves voice Puck there; the page reads the config
  // again, and that read is held, or fails. Another tab saves voice Charon.
  // The person then changes only whether the meeting voice is on, and saves:
  // that save must not send Puck back over Charon.
  it.each(['hold', 'network', 'http500'] as const)(
    "sends only what the person changed while the read after a new run's answer is %s",
    async (readMode) => {
      const unit = fakeUnit({ ...config, revision: { run: 'run-A', count: 9 } })
      render(SettingsPage, { onBack: () => {} })
      const voice = await screen.findByTestId('voice-settings')
      unit.readMode = readMode

      await fireEvent.input(within(voice).getByLabelText('Text-to-speech voice'), {
        target: { value: 'Puck' },
      })
      await fireEvent.click(within(voice).getByRole('button', { name: 'Save' }))
      const inB = {
        ...config,
        tts: { ...config.tts, voice: 'Puck' },
        revision: { run: 'run-B', count: 1 },
      }
      unit.current = inB
      unit.patches[0](inB)
      expect(await within(voice).findByText('Saved!')).toBeTruthy()

      unit.current = {
        ...inB,
        tts: { ...inB.tts, voice: 'Charon' },
        revision: { run: 'run-B', count: 2 },
      }
      await fireEvent.click(within(voice).getByLabelText('Enable meeting voice'))
      await fireEvent.click(within(voice).getByRole('button', { name: 'Save' }))

      await waitFor(() => expect(unit.bodies).toHaveLength(2))
      expect(unit.bodies[1]).toEqual({ voice: { enabled: false } })
    },
  )

  // While the page reads the config again it says the settings may be out of
  // date; the line goes once the read lands.
  it('says the settings may be out of date while it reads them again', async () => {
    const unit = fakeUnit({ ...config, revision: { run: 'run-A', count: 9 } })
    render(SettingsPage, { onBack: () => {} })
    const voice = await screen.findByTestId('voice-settings')
    expect(screen.queryByText('Settings may be out of date, reloading…')).toBeNull()
    unit.readMode = 'hold'

    await fireEvent.input(within(voice).getByLabelText('Text-to-speech voice'), {
      target: { value: 'Puck' },
    })
    await fireEvent.click(within(voice).getByRole('button', { name: 'Save' }))
    const inB = {
      ...config,
      tts: { ...config.tts, voice: 'Puck' },
      revision: { run: 'run-B', count: 1 },
    }
    unit.current = inB
    unit.patches[0](inB)

    expect(await screen.findByText('Settings may be out of date, reloading…')).toBeTruthy()
    unit.held[0]()
    await waitFor(() =>
      expect(screen.queryByText('Settings may be out of date, reloading…')).toBeNull(),
    )
    expect(configState.config?.revision).toEqual({ run: 'run-B', count: 1 })
  })

  // The proof's case: after a restart, voice Puck is saved, and the read after
  // it is on its way when the person types Aoede in the same field. The read
  // lands with the Puck this page saved: that is not news to the field, so
  // Aoede stays, and a Save sends it.
  it('keeps typing in a field just saved when the read after it lands', async () => {
    const unit = fakeUnit({ ...config, revision: { run: 'run-A', count: 9 } })
    render(SettingsPage, { onBack: () => {} })
    const voiceSection = await screen.findByTestId('voice-settings')
    const voice = within(voiceSection).getByLabelText('Text-to-speech voice') as HTMLInputElement
    unit.readMode = 'hold'

    await fireEvent.input(voice, { target: { value: 'Puck' } })
    await fireEvent.click(within(voiceSection).getByRole('button', { name: 'Save' }))
    const inB = {
      ...config,
      tts: { ...config.tts, voice: 'Puck' },
      revision: { run: 'run-B', count: 1 },
    }
    unit.current = inB
    unit.patches[0](inB)
    expect(await within(voiceSection).findByText('Saved!')).toBeTruthy()

    // Typed in two inputs, as a person does.
    await fireEvent.input(voice, { target: { value: 'Ao' } })
    await fireEvent.input(voice, { target: { value: 'Aoede' } })
    unit.held[0]()
    await waitFor(() => expect(configState.config?.revision).toEqual({ run: 'run-B', count: 1 }))
    expect(voice.value).toBe('Aoede')
    await fireEvent.click(within(voiceSection).getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(unit.bodies).toHaveLength(2))
    expect(unit.bodies[1]).toEqual({ tts: { voice: 'Aoede' } })
  })

  // Sol's round-7 case: voice Puck is saved, and the unit takes it at A/10,
  // but its answer is held. Another tab saves Charon at A/11. General's save
  // answers A/12, holding Charon. Then the held A/10 answer lands. The field
  // follows the config, which stays at A/12: it shows Charon, and a voice Save
  // has nothing to send.
  it("keeps the newer value when a save's own answer lands late", async () => {
    const unit = fakeUnit({ ...config, revision: { run: 'run-A', count: 9 } })
    render(SettingsPage, { onBack: () => {} })
    const general = await screen.findByTestId('general-settings')
    const voiceSection = screen.getByTestId('voice-settings')
    const voice = within(voiceSection).getByLabelText('Text-to-speech voice') as HTMLInputElement

    await fireEvent.input(voice, { target: { value: 'Puck' } })
    await fireEvent.click(within(voiceSection).getByRole('button', { name: 'Save' }))
    await fireEvent.input(within(general).getByLabelText('Silence Timeout'), {
      target: { value: '45s' },
    })
    await fireEvent.click(within(general).getByRole('button', { name: 'Save' }))
    expect(unit.patches).toHaveLength(2)

    unit.patches[1]({
      ...config,
      silence_timeout: '45s',
      tts: { ...config.tts, voice: 'Charon' },
      revision: { run: 'run-A', count: 12 },
    })
    expect(await within(general).findByText('Saved!')).toBeTruthy()
    expect(voice.value).toBe('Charon')
    unit.patches[0]({
      ...config,
      tts: { ...config.tts, voice: 'Puck' },
      revision: { run: 'run-A', count: 10 },
    })
    expect(await within(voiceSection).findByText('Saved!')).toBeTruthy()

    expect(configState.config?.revision).toEqual({ run: 'run-A', count: 12 })
    expect(voice.value).toBe('Charon')
    await fireEvent.click(within(voiceSection).getByRole('button', { name: 'Save' }))
    expect(await within(voiceSection).findByText('No changes to save.')).toBeTruthy()
    expect(unit.bodies).toHaveLength(2)
  })

  // Sol's round-8 case: standup's description is saved as "My saved draft",
  // and the unit takes it at A/10, but its answer is held. Another tab sets
  // the description "Elsewhere" and the model "gateway/newer" at A/11.
  // General's save answers A/12, holding both. Then the held A/10 answer
  // lands. The form follows the config, which stays at A/12, and a preset
  // Save has nothing to send.
  it("keeps the newer preset when the preset save's own answer lands late", async () => {
    const standup = {
      description: 'Original',
      system_prompt: 'Summarize the standup.',
      user_template: '{{transcript}}',
      model: 'vertex/gemini-4-carbon',
    }
    const withStandup = (preset: typeof standup) => ({
      ...config,
      summarization: { ...config.summarization, presets: { standup: preset } },
    })
    const unit = fakeUnit({ ...withStandup(standup), revision: { run: 'run-A', count: 9 } })
    render(SettingsPage, { onBack: () => {} })
    const editor = await screen.findByTestId('preset-editor')
    const general = screen.getByTestId('general-settings')
    await fireEvent.click(within(editor).getByRole('button', { name: /standup/ }))
    const description = within(editor).getByLabelText('Description') as HTMLInputElement
    const model = within(editor).getByLabelText('Model Override (optional)') as HTMLInputElement

    await fireEvent.input(description, { target: { value: 'My saved draft' } })
    await fireEvent.click(within(editor).getByRole('button', { name: 'Save' }))
    await fireEvent.input(within(general).getByLabelText('Silence Timeout'), {
      target: { value: '45s' },
    })
    await fireEvent.click(within(general).getByRole('button', { name: 'Save' }))
    expect(unit.patches).toHaveLength(2)

    unit.patches[1]({
      ...withStandup({ ...standup, description: 'Elsewhere', model: 'gateway/newer' }),
      silence_timeout: '45s',
      revision: { run: 'run-A', count: 12 },
    })
    expect(await within(general).findByText('Saved!')).toBeTruthy()
    expect(description.value).toBe('Elsewhere')
    expect(model.value).toBe('gateway/newer')
    unit.patches[0]({
      ...withStandup({ ...standup, description: 'My saved draft' }),
      revision: { run: 'run-A', count: 10 },
    })
    expect(await within(editor).findByText('Saved!')).toBeTruthy()

    expect(configState.config?.revision).toEqual({ run: 'run-A', count: 12 })
    expect(description.value).toBe('Elsewhere')
    expect(model.value).toBe('gateway/newer')
    await fireEvent.click(within(editor).getByRole('button', { name: 'Save' }))
    expect(await within(editor).findByText('No changes to save.')).toBeTruthy()
    expect(unit.bodies).toHaveLength(2)
  })

  // The preset editor on the same terms: its model is saved after a restart,
  // and the read after the new run's answer is held, or fails. Another tab
  // changes the description. The person types a description and saves: only
  // the description is sent, not the model again.
  it.each(['hold', 'network', 'http500'] as const)(
    "sends only the preset fields the person changed while the read after a new run's answer is %s",
    async (readMode) => {
      const standup = {
        description: 'Original',
        system_prompt: 'Summarize the standup.',
        user_template: '{{transcript}}',
        model: 'vertex/gemini-4-carbon',
      }
      const withStandup = (preset: typeof standup) => ({
        ...config,
        summarization: { ...config.summarization, presets: { standup: preset } },
      })
      const unit = fakeUnit({ ...withStandup(standup), revision: { run: 'run-A', count: 9 } })
      render(SettingsPage, { onBack: () => {} })
      const editor = await screen.findByTestId('preset-editor')
      await fireEvent.click(within(editor).getByRole('button', { name: /standup/ }))
      unit.readMode = readMode

      await fireEvent.input(within(editor).getByLabelText('Model Override (optional)'), {
        target: { value: 'gateway/newer' },
      })
      await fireEvent.click(within(editor).getByRole('button', { name: 'Save' }))
      const inB = {
        ...withStandup({ ...standup, model: 'gateway/newer' }),
        revision: { run: 'run-B', count: 1 },
      }
      unit.current = inB
      unit.patches[0](inB)
      expect(await within(editor).findByText('Saved!')).toBeTruthy()

      unit.current = {
        ...withStandup({ ...standup, description: 'Elsewhere', model: 'gateway/newer' }),
        revision: { run: 'run-B', count: 2 },
      }
      await fireEvent.input(within(editor).getByLabelText('Description'), {
        target: { value: 'Mine' },
      })
      await fireEvent.click(within(editor).getByRole('button', { name: 'Save' }))

      await waitFor(() => expect(unit.bodies).toHaveLength(2))
      expect(unit.bodies[1]).toEqual({
        summarization: { presets: { standup: { description: 'Mine' } } },
      })
    },
  )
})
