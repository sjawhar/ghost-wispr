import { fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import VoiceSettings from '../VoiceSettings.svelte'
import SettingsPage from '../SettingsPage.svelte'
import type { ConfigResponse } from '../../lib/config-api'
import { configState, resetConfigState } from '../../lib/config.svelte'

const config: ConfigResponse = {
  silence_timeout: '30s',
  summarization: { model: 'openai/gpt-4o-mini', base_url: '', presets: {} },
  transcription: { endpointing: '400', utterance_end_ms: '1000' },
  gdrive: { folder_id: '', has_credentials: false, sync_enabled: true },
  gc: { enabled: true, max_age_days: 30, max_audio_size_mb: 1024 },
  api_keys: {},
  recording_rules: [],
  voice: { enabled: true, engine: 'tts', tts_engine: 'aura' },
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

  it('shows the voice the unit holds', () => {
    render(VoiceSettings, { config })

    expect(checked('Enable meeting voice')).toBe(true)
    expect(checked('Our own voice (tts)')).toBe(true)
    expect(checked('GPT-Live (gptlive)')).toBe(false)
    expect(checked('Deepgram voice agent (dgagent)')).toBe(false)
    expect(checked('Aura')).toBe(true)
    expect(checked('Gemini')).toBe(false)
  })

  it('shows a voice that is off and another voice', () => {
    render(VoiceSettings, {
      config: { ...config, voice: { enabled: false, engine: 'dgagent', tts_engine: 'gemini' } },
    })

    expect(checked('Enable meeting voice')).toBe(false)
    expect(checked('Deepgram voice agent (dgagent)')).toBe(true)
    expect(checked('Our own voice (tts)')).toBe(false)
  })

  it('sends only the voice fields that changed, and keeps what the unit answers', async () => {
    const answer: ConfigResponse = {
      ...config,
      voice: { enabled: true, engine: 'gptlive', tts_engine: 'aura' },
    }
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(answer)))
    vi.stubGlobal('fetch', fetch)
    render(VoiceSettings, { config })

    await fireEvent.click(screen.getByLabelText('GPT-Live (gptlive)'))
    await save()

    expect(fetch).toHaveBeenCalledTimes(1)
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/config')
    expect(init.method).toBe('PATCH')
    expect(JSON.parse(init.body as string)).toEqual({ voice: { engine: 'gptlive' } })
    await waitFor(() => expect(configState.config).toEqual(answer))
  })

  it("sends the switch and voice A's engine", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(config)))
    vi.stubGlobal('fetch', fetch)
    render(VoiceSettings, { config })

    await fireEvent.click(screen.getByLabelText('Enable meeting voice'))
    await fireEvent.click(screen.getByLabelText('Gemini'))
    await save()

    expect(fetch).toHaveBeenCalledTimes(1)
    expect(JSON.parse(fetch.mock.calls[0][1].body as string)).toEqual({
      voice: { enabled: false, tts_engine: 'gemini' },
    })
  })

  it('sends nothing when nothing changed', async () => {
    const fetch = vi.fn()
    vi.stubGlobal('fetch', fetch)
    render(VoiceSettings, { config })

    await save()

    expect(fetch).not.toHaveBeenCalled()
  })

  it("shows the unit's refusal as an error", async () => {
    const refusal = 'invalid voice.engine "robot": supported engines are tts, gptlive, dgagent'
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: refusal }), { status: 400 })),
    )
    render(VoiceSettings, { config })

    await fireEvent.click(screen.getByLabelText('Deepgram voice agent (dgagent)'))
    await save()

    const shown = await screen.findByText(/supported engines are tts, gptlive, dgagent/)
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
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(() => Promise.resolve(new Response(JSON.stringify(config)))),
    )
    render(SettingsPage, { onBack: () => {} })

    expect(await screen.findByRole('heading', { name: 'Meeting voice' })).toBeTruthy()
    expect(checked('Our own voice (tts)')).toBe(true)
  })
})
