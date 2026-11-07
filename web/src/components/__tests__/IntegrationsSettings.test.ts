import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import IntegrationsSettings from '../IntegrationsSettings.svelte'
import type { ConfigResponse } from '../../lib/config-api'

const config: ConfigResponse = {
  silence_timeout: '30s',
  summarization: { model: 'openai/gpt-4o-mini', base_url: '', presets: {} },
  transcription: { endpointing: '400', utterance_end_ms: '1000' },
  gdrive: { folder_id: '', has_credentials: false, sync_enabled: true },
  gc: { enabled: true, max_age_days: 30, max_audio_size_mb: 1024 },
  api_keys: {},
}

describe('IntegrationsSettings', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('says 0 means no limit for each GC limit, and allows it', () => {
    render(IntegrationsSettings, { config })

    const maxAge = screen.getByLabelText('Delete sessions older than (days)')
    const budget = screen.getByLabelText('Audio budget (MB)')
    expect(maxAge.getAttribute('min')).toBe('0')
    expect(budget.getAttribute('min')).toBe('0')
    expect(screen.getByText(/^0 = never\./)).toBeTruthy()
    expect(screen.getByText(/^0 = no budget: GC removes no audio to free space\./)).toBeTruthy()
  })

  it('saves 0 as the limit', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(config)))
    vi.stubGlobal('fetch', fetch)
    const { container } = render(IntegrationsSettings, { config })

    await fireEvent.input(screen.getByLabelText('Delete sessions older than (days)'), {
      target: { value: '0' },
    })
    await fireEvent.input(screen.getByLabelText('Audio budget (MB)'), { target: { value: '0' } })
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    const patches = fetch.mock.calls.map(([, init]) => JSON.parse(init.body as string))
    expect(patches).toContainEqual({ gc: { enabled: true, max_age_days: 0, max_audio_size_mb: 0 } })
  })
})
