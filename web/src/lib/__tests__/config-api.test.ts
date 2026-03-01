import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchConfig, patchConfig, testPreset } from '../config-api'

afterEach(() => {
  vi.restoreAllMocks()
})

describe('config api client', () => {
  it('fetches config', async () => {
    const mockConfig = {
      silence_timeout: '30s',
      summarization: { model: 'openai/gpt-4o-mini', base_url: '', presets: {} },
      transcription: { endpointing: '400', utterance_end_ms: '1000' },
      gdrive: { folder_id: '', has_credentials: false },
      api_keys: { deepgram: false, openai: false, anthropic: false, gemini: false },
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
      summarization: { model: 'openai/gpt-4o-mini', base_url: '', presets: {} },
      transcription: { endpointing: '400', utterance_end_ms: '1000' },
      gdrive: { folder_id: '', has_credentials: false },
      api_keys: { deepgram: false, openai: false, anthropic: false, gemini: false },
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
