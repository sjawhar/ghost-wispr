import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchConfig, fetchProviders, patchConfig, testPreset } from '../config-api'

afterEach(() => {
  vi.restoreAllMocks()
})

describe('config api client', () => {
  it('fetches config', async () => {
    const mockConfig = {
      silence_timeout: '30s',
      summarization: { model: 'vertex/gemini-3.5-flash-lite', presets: {} },
      transcription: { endpointing: '400', utterance_end_ms: '1000' },
      gdrive: { folder_id: '', has_credentials: false },
      tts: { model: 'vertex-us/gemini-3.8-flash-tts', voice: 'Kore' },
      providers: { vertex: { api: 'vertex', auth: { kind: 'google' } } },
    }

    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => mockConfig,
      }),
    )

    await expect(fetchConfig()).resolves.toEqual(mockConfig)
  })

  it('patches config and returns updated result', async () => {
    const updated = {
      silence_timeout: '45s',
      summarization: { model: 'vertex/gemini-3.5-flash-lite', presets: {} },
      transcription: { endpointing: '400', utterance_end_ms: '1000' },
      gdrive: { folder_id: '', has_credentials: false },
      tts: { model: 'vertex-us/gemini-3.8-flash-tts', voice: 'Kore' },
      providers: { vertex: { api: 'vertex', auth: { kind: 'google' } } },
    }

    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => updated,
    })
    vi.stubGlobal('fetch', fetchMock)

    const result = await patchConfig({ silence_timeout: '45s' })
    expect(result).toEqual(updated)

    expect(fetchMock).toHaveBeenCalledWith('/api/config', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ silence_timeout: '45s' }),
    })
  })

  it('patchConfig throws on validation error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 400,
        text: async () => '{"error":"invalid silence_timeout"}',
      }),
    )

    await expect(patchConfig({ silence_timeout: 'bad' })).rejects.toThrow(/invalid/)
  })

  it("throws the unit's refusal in its own words, not its JSON", async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            error: 'provider_keys: provider "vertex" signs in with google, not a key',
          }),
          { status: 400 },
        ),
      ),
    )

    await expect(patchConfig({ provider_keys: { vertex: 'k-1' } })).rejects.toThrow(
      /^provider_keys: provider "vertex" signs in with google, not a key$/,
    )
  })

  it('fetches the providers, and checks them again with refresh', async () => {
    const providers = [
      {
        name: 'vertex',
        api: 'vertex',
        url: 'https://aiplatform.googleapis.com',
        auth: 'google',
        sign_in: 'Google workload identity (google_credentials_file)',
        key_set: false,
        checked: true,
        ok: true,
        message: 'Signed in: Google issued an access token.',
        checked_at: '2026-11-20T19:00:00Z',
        roles: ['summarization.model'],
      },
    ]
    const fetchMock = vi
      .fn()
      .mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ providers }))))
    vi.stubGlobal('fetch', fetchMock)

    await expect(fetchProviders()).resolves.toEqual(providers)
    await expect(fetchProviders(true)).resolves.toEqual(providers)
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      '/api/providers',
      '/api/providers?refresh=1',
    ])
  })

  it('tests preset against session', async () => {
    const mockResponse = { summary: '## Summary', error: '' }

    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => mockResponse,
    })
    vi.stubGlobal('fetch', fetchMock)

    const result = await testPreset('default', 's1')
    expect(result).toEqual(mockResponse)

    expect(fetchMock).toHaveBeenCalledWith('/api/config/presets/default/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: 's1' }),
    })
  })

  it('testPreset returns error field on LLM failure', async () => {
    const mockResponse = { summary: '', error: 'model unavailable' }

    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => mockResponse,
      }),
    )

    const result = await testPreset('default', 's1')
    expect(result.error).toBe('model unavailable')
    expect(result.summary).toBe('')
  })
})
