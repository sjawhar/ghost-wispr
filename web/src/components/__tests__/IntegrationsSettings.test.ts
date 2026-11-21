import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import IntegrationsSettings from '../IntegrationsSettings.svelte'
import { sent, stubFetch, unitConfig as config } from './configFixture'

describe('IntegrationsSettings', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows the model providers, not per-vendor key slots', async () => {
    stubFetch()
    render(IntegrationsSettings, { config })

    expect(screen.getByRole('heading', { name: 'Model providers' })).toBeTruthy()
    expect(screen.queryByRole('heading', { name: 'API Keys' })).toBeNull()
    const vertex = await screen.findByRole('listitem', { name: 'vertex' })
    expect(await within(vertex).findByText('Signed in')).toBeTruthy()
  })

  it('says 0 means no limit for each GC limit, and allows it', () => {
    stubFetch()
    render(IntegrationsSettings, { config })

    const maxAge = screen.getByLabelText('Delete sessions older than (days)')
    const budget = screen.getByLabelText('Audio budget (MB)')
    expect(maxAge.getAttribute('min')).toBe('0')
    expect(budget.getAttribute('min')).toBe('0')
    expect(screen.getByText(/^0 = never\./)).toBeTruthy()
    expect(screen.getByText(/^0 = no budget: GC removes no audio to free space\./)).toBeTruthy()
  })

  it('saves 0 as the limit', async () => {
    const fetch = stubFetch()
    const { container } = render(IntegrationsSettings, { config })

    await fireEvent.input(screen.getByLabelText('Delete sessions older than (days)'), {
      target: { value: '0' },
    })
    await fireEvent.input(screen.getByLabelText('Audio budget (MB)'), { target: { value: '0' } })
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        {
          url: '/api/config',
          method: 'PATCH',
          body: { gc: { enabled: true, max_age_days: 0, max_audio_size_mb: 0 } },
        },
      ]),
    )
  })
})
